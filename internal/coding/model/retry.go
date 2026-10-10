package model

import (
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/capability"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

// RetryBudget is the retry budget for one model request in a Coding run: ten
// replays of a failure that produced nothing, following the default the other
// agent frontends ship (Claude Code retries transient failures up to ten times;
// Grok Build's live retry state reports a budget in the same range). The backoff
// doubles from 500ms with full jitter, capped at 30s, and a provider Retry-After
// wins when it is longer.
//
// Coding also hands this value to the Agent loop's stream recovery as its replay
// allowance, so a stream that failed after producing only reasoning — nothing a
// consumer has to retract — earns the same budget as a request that produced
// nothing at all. One value keeps the two owners from drifting apart.
const RetryBudget = 10

// RetryWindowFloor is the shortest wall clock one model request may spend
// replaying, whatever the transport's idle bound is. The window bounds a whole
// replay episode rather than one attempt, so a short idle bound must not shrink
// it to where a brief outage exhausts it.
const RetryWindowFloor = 10 * time.Minute

// RetryWindowFor returns the wall clock one model request may spend replaying a
// failure that produced nothing, for a transport whose streaming idle bound is
// streamIdleTimeout: max(RetryWindowFloor, 2×idle). Zero selects
// ai.DefaultStreamIdleTimeout, the bound an unconfigured adapter applies, so an
// unset idle keeps the twenty-minute window Coding has always shipped.
//
// The window bounds the scheduling of attempts, never one already in flight: a
// provider that has gone silent can spend a whole idle window on the attempt
// that is running, so the worst case is the window plus that attempt. It is also
// what the Agent loop's recovery episode is bounded by, through
// agent.WithStreamRecoveryWindow — one value feeds both owners.
func RetryWindowFor(streamIdleTimeout time.Duration) time.Duration {
	if streamIdleTimeout <= 0 {
		streamIdleTimeout = ai.DefaultStreamIdleTimeout
	}

	return max(RetryWindowFloor, 2*streamIdleTimeout)
}

// withCodingModel wraps one adapter with the coding middleware chain: the
// resolved capability declaration is applied on top of whatever the adapter
// reports, then calls are retried within the window the model's own idle bound
// implies.
func withCodingModel(model ai.LanguageModel, resolved modelcatalog.ResolvedModel) ai.LanguageModel {
	return ai.Chain(
		withCodingRetry(model, RetryWindowFor(resolved.StreamIdleTimeout)),
		capability.New(resolved.Capabilities),
	)
}

func withCodingRetry(
	model ai.LanguageModel,
	retryWindow time.Duration,
	options ...retry.Option,
) ai.LanguageModel {
	retryOptions := make([]retry.Option, 0, len(options)+2)
	retryOptions = append(
		retryOptions,
		retry.WithMaxAttempts(RetryBudget+1),
		retry.WithMaxElapsed(retryWindow),
	)
	retryOptions = append(retryOptions, options...)

	return ai.Chain(model, retry.New(retryOptions...))
}
