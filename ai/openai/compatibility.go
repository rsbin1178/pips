package openai

// MaxTokensField selects the Chat Completions field used for the output-token
// limit. The empty value uses max_completion_tokens.
type MaxTokensField string

// Chat Completions max-token fields.
const (
	MaxTokensFieldCompletion MaxTokensField = "max_completion_tokens"
	MaxTokensFieldLegacy     MaxTokensField = "max_tokens"
)

// StreamUsageMode controls whether Chat Completions asks for a final usage
// chunk. The empty value includes usage.
type StreamUsageMode string

// Stream usage modes.
const (
	StreamUsageInclude StreamUsageMode = "include"
	StreamUsageOmit    StreamUsageMode = "omit"
)

// StructuredOutputMode selects the Chat Completions response_format shape.
// The empty value uses json_schema.
type StructuredOutputMode string

// Structured-output encodings.
const (
	StructuredOutputJSONSchema StructuredOutputMode = "json_schema"
	StructuredOutputJSONObject StructuredOutputMode = "json_object"
	StructuredOutputOmit       StructuredOutputMode = "omit"
)

// ChatReasoningFormat selects how portable reasoning options are encoded on
// Chat Completions. The empty value sends reasoning_effort.
type ChatReasoningFormat string

// Chat reasoning encodings.
const (
	ChatReasoningEffort   ChatReasoningFormat = "reasoning_effort"
	ChatReasoningObject   ChatReasoningFormat = "reasoning_object"
	ChatReasoningDeepSeek ChatReasoningFormat = "deepseek"
	ChatReasoningOmit     ChatReasoningFormat = "omit"
)

// ReasoningHistoryField selects how prior assistant reasoning is represented
// during Chat Completions continuation. The empty value omits it.
type ReasoningHistoryField string

// Assistant reasoning-history fields.
const (
	ReasoningHistoryContent       ReasoningHistoryField = "reasoning_content"
	ReasoningHistoryReasoning     ReasoningHistoryField = "reasoning"
	ReasoningHistoryDetails       ReasoningHistoryField = "reasoning_details"
	ReasoningHistoryContentChunks ReasoningHistoryField = "content_chunks"
)

// Compatibility describes documented wire differences on OpenAI-shaped
// endpoints. Its zero value is OpenAI's default behavior.
//
// It does not describe provider identity, authentication, endpoint URLs, or
// model capabilities; configure those independently.
type Compatibility struct {
	MaxTokensField   MaxTokensField
	StreamUsage      StreamUsageMode
	StructuredOutput StructuredOutputMode
	ChatReasoning    ChatReasoningFormat
	ReasoningHistory ReasoningHistoryField

	// IncludeEncryptedReasoning asks Responses-compatible providers to return
	// replayable encrypted reasoning state.
	IncludeEncryptedReasoning bool
}

func (c Compatibility) resolvedMaxTokensField() MaxTokensField {
	if c.MaxTokensField == "" {
		return MaxTokensFieldCompletion
	}

	return c.MaxTokensField
}

func (c Compatibility) resolvedStreamUsage() StreamUsageMode {
	if c.StreamUsage == "" {
		return StreamUsageInclude
	}

	return c.StreamUsage
}

func (c Compatibility) resolvedStructuredOutput() StructuredOutputMode {
	if c.StructuredOutput == "" {
		return StructuredOutputJSONSchema
	}

	return c.StructuredOutput
}

func (c Compatibility) resolvedChatReasoning() ChatReasoningFormat {
	if c.ChatReasoning == "" {
		return ChatReasoningEffort
	}

	return c.ChatReasoning
}
