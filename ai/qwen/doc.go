// Package qwen implements ai.LanguageModel and ai.EmbeddingModel against
// Alibaba Cloud Model Studio / DashScope (Qwen).
//
// The OpenAI-compatible endpoint covers Chat Completions (text, vision,
// function calling) and text embeddings on the same base URL. Multimodal
// embeddings, document reranking, and image generation use DashScope-native
// endpoints and are not part of this package.
//
// Models can be configured via [WithAPIKey] (defaulting to the
// DASHSCOPE_API_KEY environment variable) and [WithBaseURL] (defaulting to
// [DefaultBaseURL]). Region endpoints are available as [DefaultChinaBaseURL]
// and [DefaultUSBaseURL].
package qwen
