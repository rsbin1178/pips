package ai

// EmbeddingRequest asks an [EmbeddingModel] to embed one or more inputs.
type EmbeddingRequest struct {
	// Input is the texts to embed. Order is preserved in the response.
	Input []string
	// Dimensions optionally requests reduced-dimension vectors on models that
	// support it. Nil uses the model default.
	Dimensions *int

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// EmbeddingResponse carries one vector per input, in input order.
type EmbeddingResponse struct {
	Embeddings [][]float32
	Usage      Usage
	Raw        JSON
}
