// Package zhipu implements ai.LanguageModel, ai.EmbeddingModel, and
// ai.RerankModel against Zhipu AI (GLM / BigModel) APIs.
//
// Models can be configured via [WithAPIKey] (defaulting to the ZHIPU_API_KEY
// environment variable) and [WithBaseURL] (defaulting to
// https://open.bigmodel.cn/api/paas/v4).
package zhipu
