package ai

// Citation represents an attribution or source reference produced by a search
// or grounding tool.
type Citation struct {
	// URL is the web address of the referenced source.
	URL string `json:"url"`
	// Title is the human-readable title of the document or webpage.
	Title string `json:"title,omitempty"`
	// Snippet is the extracted content snippet that grounded the answer.
	Snippet string `json:"snippet,omitempty"`
	// Index is the reference index corresponding to citation markers in the text.
	Index int `json:"index,omitempty"`
	// TextRange is the character offset span in the generated text supported by
	// this citation.
	TextRange *TextRange `json:"text_range,omitempty"`
}

// TextRange specifies character offsets within generated content.
type TextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// GroundingMetadata carries query-level grounding details from search tools.
type GroundingMetadata struct {
	// WebSearchQueries lists queries formulated by the model during generation.
	WebSearchQueries []string `json:"web_search_queries,omitempty"`
	// Raw preserves provider-specific grounding structures.
	Raw any `json:"raw,omitempty"`
}
