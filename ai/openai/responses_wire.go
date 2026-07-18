package openai

import "github.com/rsbin/pips/ai"

// Responses API wire types — the subset this adapter produces and consumes.
// The Responses API models a conversation as a flat list of typed input
// items rather than role-tagged messages.

type responsesRequest struct {
	Model           string              `json:"model"`
	Input           []responseItem      `json:"input"`
	Instructions    string              `json:"instructions,omitempty"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	ToolChoice      any                 `json:"tool_choice,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	TopP            *float64            `json:"top_p,omitempty"`
	MaxOutputTokens *int                `json:"max_output_tokens,omitempty"`
	Text            *responsesText      `json:"text,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
}

// responseItem is one input or output item. Only the fields relevant to its
// Type are populated; Type selects the shape.
type responseItem struct {
	Type    string            `json:"type,omitempty"` // "message"|"function_call"|"function_call_output"|"reasoning"
	Role    string            `json:"role,omitempty"`
	Content []responseContent `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`

	// reasoning (output only)
	Summary []responseSummary `json:"summary,omitempty"`
}

type responseContent struct {
	Type     string `json:"type"` // "input_text"|"output_text"|"input_image"|"input_file"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type responseSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type responsesTool struct {
	Type        string     `json:"type"` // "function"
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  *ai.Schema `json:"parameters,omitempty"`
	Strict      bool       `json:"strict,omitempty"`
}

type responsesToolChoiceForced struct {
	Type string `json:"type"` // "function"
	Name string `json:"name"`
}

type responsesText struct {
	Format *responsesFormat `json:"format,omitempty"`
}

type responsesFormat struct {
	Type   string     `json:"type"` // "json_schema"
	Name   string     `json:"name"`
	Schema *ai.Schema `json:"schema"`
	Strict bool       `json:"strict,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesResponse struct {
	ID                string               `json:"id"`
	Model             string               `json:"model"`
	Status            string               `json:"status"`
	Output            []responseItem       `json:"output"`
	Usage             *responsesUsage      `json:"usage"`
	IncompleteDetails *responsesIncomplete `json:"incomplete_details"`
	Error             *responsesError      `json:"error"`
}

// responsesError is the error object on a failed response.
type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// Streaming event payloads.

type responsesStreamEvent struct {
	Type        string             `json:"type"`
	Delta       string             `json:"delta"`
	OutputIndex int                `json:"output_index"`
	Item        *responseItem      `json:"item"`
	Response    *responsesResponse `json:"response"`
	// error events
	Message string `json:"message"`
	Code    string `json:"code"`
}
