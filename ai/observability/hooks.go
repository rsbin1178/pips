// Package observability turns cross-cutting instrumentation into an
// [ai.Middleware] via a set of callback hooks. It is dependency-free: it
// emits structured events but does not bind to any telemetry backend, leaving
// OpenTelemetry or logging integration to the caller (or a future subpackage).
package observability

import (
	"context"
	"time"

	"github.com/rsbin1178/pips/ai"
)

// CallInfo describes an in-flight request passed to the hooks.
type CallInfo struct {
	Provider ai.Provider
	ModelID  string
	Request  ai.Request
	// Streaming reports whether the call is Stream (true) or Generate.
	Streaming bool
}

// Result is passed to [Hooks.OnFinish] after a successful call.
type Result struct {
	CallInfo
	// Response is set for Generate calls; nil for Stream (use OnStreamEvent
	// and the message_end usage for streaming accounting).
	Response *ai.Response
	// Usage is the token usage observed (from the response, or accumulated
	// from stream events).
	Usage ai.Usage
	// FinishReason is the normalized stop reason.
	FinishReason ai.FinishReason
	// Duration is the wall-clock time from call start to completion.
	Duration time.Duration
}

// Hooks receives lifecycle callbacks. Every field is optional; nil hooks are
// skipped. Hooks run on the calling goroutine, so keep them fast and
// non-blocking.
type Hooks struct {
	// OnStart fires before the underlying call. The returned context is used
	// for the call, so a hook may attach a span or values; return ctx
	// unchanged to opt out.
	OnStart func(ctx context.Context, info CallInfo) context.Context
	// OnStreamEvent fires for each streaming event (never for Generate).
	OnStreamEvent func(ctx context.Context, info CallInfo, ev ai.StreamEvent)
	// OnFinish fires after a call completes without error.
	OnFinish func(ctx context.Context, result Result)
	// OnError fires when a call (or stream) fails.
	OnError func(ctx context.Context, info CallInfo, err error)
}

// Middleware returns an [ai.Middleware] that invokes h around each call.
func Middleware(h Hooks) ai.Middleware {
	return func(next ai.LanguageModel) ai.LanguageModel {
		return &model{LanguageModel: next, hooks: h}
	}
}

type model struct {
	ai.LanguageModel
	hooks Hooks
}

func (m *model) info(req ai.Request, streaming bool) CallInfo {
	return CallInfo{
		Provider:  m.Provider(),
		ModelID:   m.ModelID(),
		Request:   req,
		Streaming: streaming,
	}
}

func (m *model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	info := m.info(req, false)
	ctx = m.start(ctx, info)
	started := time.Now()

	resp, err := m.LanguageModel.Generate(ctx, req)
	if err != nil {
		m.error(ctx, info, err)
		return resp, err
	}

	if m.hooks.OnFinish != nil {
		result := Result{CallInfo: info, Response: resp, Duration: time.Since(started)}
		if resp != nil {
			result.Usage = resp.Usage
			result.FinishReason = resp.FinishReason
		}

		m.hooks.OnFinish(ctx, result)
	}

	return resp, nil
}

func (m *model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		info := m.info(req, true)
		ctx := m.start(ctx, info)
		started := time.Now()

		var usage ai.Usage

		var finish ai.FinishReason

		for ev, err := range m.LanguageModel.Stream(ctx, req) {
			if err != nil {
				m.error(ctx, info, err)
				yield(ai.StreamEvent{}, err)

				return
			}

			if m.hooks.OnStreamEvent != nil {
				m.hooks.OnStreamEvent(ctx, info, ev)
			}

			if ev.Type == ai.StreamMessageEnd {
				finish = ev.FinishReason
				if ev.Usage != nil {
					usage = *ev.Usage
				}
			}

			if !yield(ev, nil) {
				return
			}
		}

		if m.hooks.OnFinish != nil {
			m.hooks.OnFinish(ctx, Result{
				CallInfo:     info,
				Usage:        usage,
				FinishReason: finish,
				Duration:     time.Since(started),
			})
		}
	}
}

func (m *model) start(ctx context.Context, info CallInfo) context.Context {
	if m.hooks.OnStart != nil {
		if next := m.hooks.OnStart(ctx, info); next != nil {
			return next
		}
	}

	return ctx
}

func (m *model) error(ctx context.Context, info CallInfo, err error) {
	if m.hooks.OnError != nil {
		m.hooks.OnError(ctx, info, err)
	}
}
