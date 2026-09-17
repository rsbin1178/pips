// Package together implements ai.LanguageModel, ai.EmbeddingModel, and
// ai.RerankModel against Together AI APIs.
//
// Models can be configured via [WithAPIKey] (defaulting to the TOGETHER_API_KEY
// environment variable) and [WithBaseURL] (defaulting to
// https://api.together.ai/v1).
package together
