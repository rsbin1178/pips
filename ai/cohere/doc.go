// Package cohere implements ai.RerankModel against the Cohere Rerank API
// and compatible endpoints without the vendor SDK.
//
// In addition to Cohere's official API (POST /v1/rerank), this package works
// with compatible rerank services such as SiliconFlow, Jina AI, Together AI,
// and self-hosted inference servers (vLLM, Hugging Face TEI) by supplying
// [WithBaseURL], [WithAPIKey], and [WithProvider], or by using preset constructors
// like [SiliconFlowRerank], [JinaRerank], and [TogetherRerank].
package cohere
