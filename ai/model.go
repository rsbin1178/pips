package ai

import "context"

// LanguageModel is a chat-capable model bound to a specific provider and
// model ID. Implementations are safe for concurrent use.
//
// Bare implementations perform exactly one attempt per call and never retry;
// wrap them with middleware for resilience.
type LanguageModel interface {
	// Generate performs a blocking request and returns the completed response.
	Generate(ctx context.Context, req Request) (*Response, error)
	// Stream performs a streaming request. Errors — including connection
	// failures — surface through the returned sequence on first iteration.
	// Breaking out of the range loop cancels the request.
	Stream(ctx context.Context, req Request) Stream
	// Provider identifies the adapter.
	Provider() Provider
	// ModelID is the model this instance is bound to.
	ModelID() string
	// Capabilities reports, best effort, what the bound model supports.
	Capabilities() Capabilities
}

// ImageModel generates images from text prompts. Implemented by the openai
// and gemini adapters; absent capabilities surface as [ErrUnsupported].
type ImageModel interface {
	GenerateImages(ctx context.Context, req ImageRequest) (*ImageResponse, error)
	Provider() Provider
	ModelID() string
}

// EmbeddingModel converts text into embedding vectors.
type EmbeddingModel interface {
	Embed(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error)
	Provider() Provider
	ModelID() string
}

// TokenCounter is the optional ability to count a request's input tokens
// without running inference. Anthropic and Gemini expose dedicated endpoints;
// OpenAI does not (its tokenization is client-side only). Discover it by type
// assertion:
//
//	if tc, ok := model.(ai.TokenCounter); ok {
//	    n, err := tc.CountTokens(ctx, req)
//	}
type TokenCounter interface {
	CountTokens(ctx context.Context, req Request) (int, error)
}
