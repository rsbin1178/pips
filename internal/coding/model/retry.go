package model

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/capability"
	"github.com/rsbin1178/pips/ai/middleware/retry"
)

// codingModelMaxRetries is the retry budget for one model request in a Coding
// run: ten replays of a failure that produced nothing, following the default
// the other agent frontends ship (Claude Code retries transient failures up to
// ten times; Grok Build's live retry state reports a budget in the same range).
// The backoff doubles from 500ms with full jitter, capped at 30s, and a
// provider Retry-After wins when it is longer.
const codingModelMaxRetries = 10

// withCodingModel wraps an adapter with the coding middleware chain: the
// resolved capability declaration is applied on top of whatever the adapter
// reports, then calls are retried.
func withCodingModel(model ai.LanguageModel, capabilities ai.CapabilityOverride) ai.LanguageModel {
	return ai.Chain(withCodingRetry(model), capability.New(capabilities))
}

func withCodingRetry(model ai.LanguageModel, options ...retry.Option) ai.LanguageModel {
	retryOptions := make([]retry.Option, 0, len(options)+1)
	retryOptions = append(
		retryOptions,
		retry.WithMaxAttempts(codingModelMaxRetries+1),
	)
	retryOptions = append(retryOptions, options...)

	return ai.Chain(model, retry.New(retryOptions...))
}
