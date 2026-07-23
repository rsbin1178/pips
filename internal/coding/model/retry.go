package model

import (
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/middleware/retry"
)

const codingModelMaxRetries = 5

func withCodingRetry(model ai.LanguageModel, options ...retry.Option) ai.LanguageModel {
	retryOptions := make([]retry.Option, 0, len(options)+1)
	retryOptions = append(
		retryOptions,
		retry.WithMaxAttempts(codingModelMaxRetries+1),
	)
	retryOptions = append(retryOptions, options...)

	return ai.Chain(model, retry.New(retryOptions...))
}
