package ai

// EmbeddingTaskType selects downstream task optimization for embedding generation,
// especially for asymmetric retrieval tasks (such as search and RAG).
type EmbeddingTaskType string

// Known embedding task types.
const (
	EmbeddingTaskTypeQuery          EmbeddingTaskType = "query"
	EmbeddingTaskTypeDocument       EmbeddingTaskType = "document"
	EmbeddingTaskTypeSimilarity     EmbeddingTaskType = "similarity"
	EmbeddingTaskTypeClassification EmbeddingTaskType = "classification"
	EmbeddingTaskTypeClustering     EmbeddingTaskType = "clustering"
	EmbeddingTaskTypeQuestionAnswer EmbeddingTaskType = "question_answering"
	EmbeddingTaskTypeFactCheck      EmbeddingTaskType = "fact_verification"
	EmbeddingTaskTypeCodeQuery      EmbeddingTaskType = "code_retrieval_query"
)

// EmbeddingEncodingFormat specifies the transfer encoding format of the output vectors.
type EmbeddingEncodingFormat string

// Supported embedding encoding formats.
const (
	// EmbeddingEncodingFormatFloat returns vectors as standard JSON arrays of floats (default).
	EmbeddingEncodingFormatFloat EmbeddingEncodingFormat = "float"
	// EmbeddingEncodingFormatBase64 requests base64-encoded binary vectors from providers
	// that support it (OpenAI, Mistral) to reduce wire payload size and serialization overhead.
	// Adapters transparently decode base64 back into []float32.
	EmbeddingEncodingFormatBase64 EmbeddingEncodingFormat = "base64"
)

// EmbeddingRequest asks an [EmbeddingModel] to embed one or more inputs.
type EmbeddingRequest struct {
	// Input is the texts to embed. Order is preserved in the response.
	Input []string
	// Dimensions optionally requests reduced-dimension vectors on models that
	// support it. Nil uses the model default.
	Dimensions *int

	// TaskType optionally specifies downstream task optimization for models that
	// support asymmetric embeddings (such as Gemini). Empty leaves task optimization
	// to provider default.
	TaskType EmbeddingTaskType
	// Title optionally specifies the document title when TaskType is
	// [EmbeddingTaskTypeDocument]. Providers that do not use document titles ignore this.
	Title string
	// EncodingFormat optionally requests a compact transfer format (such as Base64)
	// on providers that support it. Regardless of transfer format, the returned
	// [EmbeddingResponse.Embeddings] are decoded as [][]float32.
	EncodingFormat EmbeddingEncodingFormat

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
