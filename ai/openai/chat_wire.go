package openai

import "github.com/rsbin/pips/ai"

// Chat Completions wire types — the subset of the schema this adapter
// produces and consumes. Field names follow the API exactly.

type chatRequest struct {
	Model               string              `json:"model"`
	Messages            []chatMessage       `json:"messages"`
	Tools               []chatTool          `json:"tools,omitempty"`
	ToolChoice          any                 `json:"tool_choice,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	TopK                *int                `json:"top_k,omitempty"`
	MinP                *float64            `json:"min_p,omitempty"`
	Seed                *int64              `json:"seed,omitempty"`
	FrequencyPenalty    *float64            `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64            `json:"presence_penalty,omitempty"`
	RepetitionPenalty   *float64            `json:"repetition_penalty,omitempty"`
	LogProbs            *bool               `json:"logprobs,omitempty"`
	TopLogProbs         *int                `json:"top_logprobs,omitempty"`
	MaxCompletionTokens *int                `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int                `json:"max_tokens,omitempty"` // compat mode only
	Stop                []string            `json:"stop,omitempty"`
	Stream              bool                `json:"stream,omitempty"`
	StreamOptions       *chatStreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat      *chatResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	Reasoning           *chatReasoning      `json:"reasoning,omitempty"`
	Thinking            *chatThinking       `json:"thinking,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"` // string or []chatContentPart
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Reasoning        string         `json:"reasoning,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url" | "file"
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	File     *chatFile     `json:"file,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"` // data: URL
	FileID   string `json:"file_id,omitempty"`
}

type chatToolCall struct {
	// Index appears only in streaming deltas.
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"` // "function"
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string          `json:"type"` // "function"
	Function chatFunctionDef `json:"function"`
}

type chatFunctionDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  *ai.Schema `json:"parameters,omitempty"`
}

type chatToolChoiceForced struct {
	Type     string             `json:"type"` // "function"
	Function chatToolChoiceName `json:"function"`
}

type chatToolChoiceName struct {
	Name string `json:"name"`
}

type chatResponseFormat struct {
	Type       string          `json:"type"` // "json_schema" | "json_object"
	JSONSchema *chatJSONSchema `json:"json_schema,omitempty"`
}

type chatReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type chatThinking struct {
	Type string `json:"type"` // "enabled"
}

type chatJSONSchema struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Schema      *ai.Schema `json:"schema"`
	Strict      bool       `json:"strict,omitempty"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Index        int                `json:"index"`
	Message      chatChoiceMessage  `json:"message"`
	Delta        *chatChoiceMessage `json:"delta"` // streaming chunks only
	FinishReason string             `json:"finish_reason"`
}

type chatChoiceMessage struct {
	Role             string         `json:"role,omitempty"`
	Content          *string        `json:"content"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"` // compat endpoints (DeepSeek, Ollama)
	Reasoning        string         `json:"reasoning,omitempty"`         // compat endpoints (OpenRouter, Together)
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
