package ai

// RerankRequest asks a [RerankModel] to score and rank candidate documents against a query.
type RerankRequest struct {
	// Query is the search intent or reference text to rank candidate documents against.
	Query string
	// Documents is the list of candidate texts to rank. Order is indexed from 0
	// and mapped to [RerankResult.Index].
	Documents []string
	// TopN optionally limits the number of returned results. If set, it must be
	// greater than 0. If nil, all candidate documents are ranked and returned.
	TopN *int
	// ReturnDocuments optionally requests that original document texts be returned
	// in [RerankResult.Document].
	ReturnDocuments bool

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// RerankResult is a single candidate document scored and ranked against the query.
type RerankResult struct {
	// Index is the zero-based position of the document in the original [RerankRequest.Documents].
	Index int
	// RelevanceScore is the relevance score assigned by the model (higher is more relevant).
	RelevanceScore float64
	// Document is the original document text, populated if ReturnDocuments was true
	// or if the provider returns it.
	Document string
}

// RerankResponse contains candidate documents sorted in descending order of relevance.
type RerankResponse struct {
	Results []RerankResult
	Usage   Usage
	Raw     JSON
}
