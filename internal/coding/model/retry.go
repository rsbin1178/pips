package model

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/capability"
	"github.com/rsbin1178/pips/ai/middleware/retry"
)

const codingModelMaxRetries = 5

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
