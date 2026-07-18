// Package ratelimit provides a client-side rate-limiting [ai.LanguageModel]
// middleware with two token buckets: requests per minute (RPM) and estimated
// input tokens per minute (TPM), the two quota dimensions LLM providers
// enforce. Calls block (respecting the context) until both buckets admit
// the request.
package ratelimit

import (
	"context"

	"golang.org/x/time/rate"

	"github.com/rsbin/pips/ai"
)

// estimateBytesPerToken is the crude fallback used when a model does not
// implement [ai.TokenCounter]: roughly four bytes of message text per token.
const estimateBytesPerToken = 4

type config struct {
	rpm *rate.Limiter
	tpm *rate.Limiter
	// countTokens overrides token estimation (tests).
	countTokens func(context.Context, ai.LanguageModel, ai.Request) int
}

// Option configures the middleware.
type Option func(*config)

// WithRPM caps requests per minute. Zero or negative disables the request
// bucket.
func WithRPM(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.rpm = rate.NewLimiter(rate.Limit(float64(n)/60), n)
		}
	}
}

// WithTPM caps estimated input tokens per minute. Zero or negative disables
// the token bucket.
func WithTPM(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.tpm = rate.NewLimiter(rate.Limit(float64(n)/60), n)
		}
	}
}

// New returns a [ai.Middleware] enforcing the configured buckets. Token
// counts use the model's [ai.TokenCounter] when implemented, falling back to
// a bytes/4 estimate over the request text.
func New(opts ...Option) ai.Middleware {
	cfg := config{countTokens: countTokens}
	for _, opt := range opts {
		opt(&cfg)
	}

	return func(next ai.LanguageModel) ai.LanguageModel {
		return &model{LanguageModel: next, cfg: cfg}
	}
}

type model struct {
	ai.LanguageModel
	cfg config
}

// Generate blocks until both buckets admit the request, then delegates.
func (m *model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	if err := m.wait(ctx, req); err != nil {
		return nil, err
	}

	return m.LanguageModel.Generate(ctx, req)
}

// Stream blocks until both buckets admit the request, then delegates. The
// wait happens on first iteration, keeping Stream's lazy contract.
func (m *model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if err := m.wait(ctx, req); err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for ev, err := range m.LanguageModel.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (m *model) wait(ctx context.Context, req ai.Request) error {
	if m.cfg.rpm != nil {
		if err := m.cfg.rpm.Wait(ctx); err != nil {
			return err
		}
	}

	if m.cfg.tpm != nil {
		tokens := m.cfg.countTokens(ctx, m.LanguageModel, req)
		if burst := m.cfg.tpm.Burst(); tokens > burst {
			tokens = burst // a request larger than the burst would never pass
		}

		if err := m.cfg.tpm.WaitN(ctx, tokens); err != nil {
			return err
		}
	}

	return nil
}

// countTokens prefers the provider's counting endpoint and falls back to a
// character-based estimate.
func countTokens(ctx context.Context, m ai.LanguageModel, req ai.Request) int {
	if counter, ok := m.(ai.TokenCounter); ok {
		if n, err := counter.CountTokens(ctx, req); err == nil && n > 0 {
			return n
		}
	}

	total := len(req.System)

	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if text, ok := part.(ai.TextPart); ok {
				total += len(text.Text)
			}
		}
	}

	tokens := max(total/estimateBytesPerToken, 1)

	return tokens
}
