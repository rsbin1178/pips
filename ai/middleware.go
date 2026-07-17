package ai

// Middleware wraps a [LanguageModel] with cross-cutting behavior (retries,
// rate limiting, observability) and returns a value that is itself a
// LanguageModel, so layers compose.
type Middleware func(LanguageModel) LanguageModel

// Chain applies middlewares to model so that the first middleware is the
// outermost layer:
//
//	m := ai.Chain(base, retry.New(), ratelimit.New(lim))
//	// request flow: retry → ratelimit → base
func Chain(model LanguageModel, middlewares ...Middleware) LanguageModel {
	for i := len(middlewares) - 1; i >= 0; i-- {
		model = middlewares[i](model)
	}
	return model
}
