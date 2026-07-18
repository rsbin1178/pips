package anthropic

import "github.com/rsbin/pips/ai"

// Content block type discriminators used on the wire in both directions.
const (
	blockTypeText             = "text"
	blockTypeImage            = "image"
	blockTypeDocument         = "document"
	blockTypeThinking         = "thinking"
	blockTypeRedactedThinking = "redacted_thinking"
	blockTypeToolUse          = "tool_use"
	blockTypeToolResult       = "tool_result"
)

// Messages API wire types — the subset this adapter produces and consumes.

type messagesRequest struct {
	Model        string            `json:"model"`
	Messages     []wireMessage     `json:"messages"`
	System       []wireTextBlock   `json:"system,omitempty"`
	MaxTokens    int               `json:"max_tokens"`
	Temperature  *float64          `json:"temperature,omitempty"`
	TopP         *float64          `json:"top_p,omitempty"`
	StopSeqs     []string          `json:"stop_sequences,omitempty"`
	Tools        []wireTool        `json:"tools,omitempty"`
	ToolChoice   *wireToolChoice   `json:"tool_choice,omitempty"`
	Thinking     *wireThinking     `json:"thinking,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	CacheControl *cacheControl     `json:"cache_control,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
}

type wireOutputConfig struct {
	Format *wireOutputFormat `json:"format,omitempty"`
}

type wireOutputFormat struct {
	Type   string     `json:"type"` // "json_schema"
	Schema *ai.Schema `json:"schema"`
}

type wireThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type wireMessage struct {
	Role    string      `json:"role"` // "user" | "assistant"
	Content []wireBlock `json:"content"`
}

// wireBlock is a content block in either direction. Only the fields relevant
// to its Type are populated.
type wireBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking / redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"` // redacted_thinking opaque blob

	// image / document
	Source *wireSource `json:"source,omitempty"`

	// tool_use
	ID    string  `json:"id,omitempty"`
	Name  string  `json:"name,omitempty"`
	Input ai.JSON `json:"input,omitempty"`

	// tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   []wireBlock `json:"content,omitempty"`
	IsError   bool        `json:"is_error,omitempty"`

	// prompt caching
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type wireTextBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl marks a prompt-caching breakpoint.
type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
	TTL  string `json:"ttl,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"` // "base64" | "url" | "file"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

type wireTool struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	InputSchema *ai.Schema `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool" | "none"
	Name string `json:"name,omitempty"`
}

type messagesResponse struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	Role       string      `json:"role"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      wireUsage   `json:"usage"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// Streaming event payloads.

type streamEnvelope struct {
	Type         string            `json:"type"`
	Index        int               `json:"index"`
	Message      *messagesResponse `json:"message"`
	ContentBlock *wireBlock        `json:"content_block"`
	Delta        *streamDelta      `json:"delta"`
	Usage        *wireUsage        `json:"usage"`
	Error        *streamError      `json:"error"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type streamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
