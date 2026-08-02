//nolint:wsl_v5 // Wire translation keeps presence-aware assignments adjacent.
package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
)

// chatRequestFrom translates a portable request into the Chat Completions
// wire shape.
//
//nolint:gocyclo // Each branch is an independently documented wire option.
func (m *Model) chatRequestFrom(req ai.Request, stream bool) (any, error) {
	messages, err := chatMessagesFrom(req, m.compat, m.label())
	if err != nil {
		return nil, err
	}

	out := chatRequest{
		Model:            m.model,
		Messages:         messages,
		Tools:            chatToolsFrom(req.Tools),
		ToolChoice:       chatToolChoiceFrom(req.ToolChoice),
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Seed:             req.Seed,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Stop:             req.Stop,
		Stream:           stream,
	}
	providerOptions := requestOptions(req, m.provider)
	if m.provider == ai.ProviderOpenAI && req.TopK != nil {
		return nil, fmt.Errorf("%s: top-k sampling: %w", m.label(), ai.ErrUnsupported)
	}
	out.TopK = req.TopK
	out.MinP = providerOptions.MinP
	out.RepetitionPenalty = providerOptions.RepetitionPenalty
	if req.LogProbs != nil {
		out.LogProbs = &req.LogProbs.Enabled
		if req.LogProbs.Top != 0 {
			out.TopLogProbs = &req.LogProbs.Top
		}
	}

	switch m.compat.resolvedMaxTokensField() {
	case MaxTokensFieldLegacy:
		out.MaxTokens = req.MaxTokens
	case MaxTokensFieldCompletion:
		out.MaxCompletionTokens = req.MaxTokens
	default:
		return nil, fmt.Errorf("%s: unsupported max-token field %q", m.label(), m.compat.MaxTokensField)
	}

	if stream {
		switch m.compat.resolvedStreamUsage() {
		case StreamUsageInclude:
			out.StreamOptions = &chatStreamOptions{IncludeUsage: true}
		case StreamUsageOmit:
		default:
			return nil, fmt.Errorf("%s: unsupported stream-usage mode %q", m.label(), m.compat.StreamUsage)
		}
	}

	if rf := req.ResponseFormat; rf != nil {
		switch m.compat.resolvedStructuredOutput() {
		case StructuredOutputJSONSchema:
			name := rf.Name
			if name == "" {
				name = "output"
			}

			out.ResponseFormat = &chatResponseFormat{
				Type: "json_schema",
				JSONSchema: &chatJSONSchema{
					Name:        name,
					Description: rf.Description,
					Schema:      rf.Schema,
					Strict:      rf.Strict,
				},
			}
		case StructuredOutputJSONObject:
			out.ResponseFormat = &chatResponseFormat{Type: "json_object"}
		case StructuredOutputOmit:
		default:
			return nil, fmt.Errorf("%s: unsupported structured-output mode %q", m.label(), m.compat.StructuredOutput)
		}
	}

	if err := applyChatReasoning(&out, req.Reasoning, m.compat, m.label()); err != nil {
		return nil, err
	}

	return mergeExtraFields(out, providerOptions.ExtraFields)
}

func applyChatReasoning(out *chatRequest, reasoning *ai.ReasoningConfig, compat Compatibility, label string) error {
	if reasoning == nil {
		return nil
	}
	if reasoning.BudgetTokens != 0 || reasoning.IncludeSummary {
		return fmt.Errorf("%s: reasoning budget or inclusion: %w", label, ai.ErrUnsupported)
	}
	if reasoning.Mode == ai.ReasoningModeAdaptive || reasoning.Mode == ai.ReasoningModeDisabled {
		return fmt.Errorf("%s: reasoning mode %q: %w", label, reasoning.Mode, ai.ErrUnsupported)
	}

	effort := string(reasoning.Effort)

	switch compat.resolvedChatReasoning() {
	case ChatReasoningEffort:
		out.ReasoningEffort = effort
	case ChatReasoningObject:
		if effort != "" {
			out.Reasoning = &chatReasoning{Effort: effort}
		}
	case ChatReasoningDeepSeek:
		out.Thinking = &chatThinking{Type: "enabled"}
		if effort != "" {
			out.ReasoningEffort = deepSeekReasoningEffort(reasoning.Effort)
		}
	case ChatReasoningOmit:
		return fmt.Errorf("%s: reasoning disabled by compatibility profile: %w", label, ai.ErrUnsupported)
	default:
		return fmt.Errorf("%s: unsupported chat-reasoning format %q", label, compat.ChatReasoning)
	}

	return nil
}

func deepSeekReasoningEffort(effort ai.ReasoningEffort) string {
	switch effort {
	case ai.ReasoningLow, ai.ReasoningMedium, ai.ReasoningHigh:
		return "high"
	default:
		return string(effort)
	}
}

// chatMessagesFrom flattens the conversation into wire messages. The system
// prompt becomes a leading system message; each tool result becomes its own
// role:"tool" message, as the API requires.
func chatMessagesFrom(req ai.Request, compat Compatibility, label string) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, chatMessage{Role: "system", Content: req.System})
	}

	for _, msg := range req.Messages {
		converted, err := chatMessageFrom(msg, compat, label)
		if err != nil {
			return nil, err
		}

		out = append(out, converted...)
	}

	return out, nil
}

func chatMessageFrom(msg ai.Message, compat Compatibility, label string) ([]chatMessage, error) {
	switch msg.Role {
	case ai.RoleSystem:
		return []chatMessage{{Role: "system", Content: textOf(msg.Parts)}}, nil
	case ai.RoleUser:
		content, err := chatContentFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []chatMessage{{Role: "user", Content: content}}, nil
	case ai.RoleAssistant:
		converted, err := chatAssistantFrom(msg, compat.ReasoningHistory)
		if err != nil {
			return nil, fmt.Errorf("%s: encoding assistant history: %w", label, err)
		}

		return []chatMessage{converted}, nil
	case ai.RoleTool:
		return chatToolResultsFrom(msg)
	default:
		return nil, fmt.Errorf("%s: unsupported message role %q", label, msg.Role)
	}
}

// chatContentFrom renders user parts: a bare string when the message is a
// single text part (the common case), a content-part array otherwise.
func chatContentFrom(parts []ai.Part) (any, error) {
	if len(parts) == 1 {
		if text, ok := parts[0].(ai.TextPart); ok {
			return text.Text, nil
		}
	}

	out := make([]chatContentPart, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, chatContentPart{Type: typeText, Text: p.Text})
		case ai.ImagePart:
			url, err := imageURLFrom(p.Source)
			if err != nil {
				return nil, err
			}

			out = append(out, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
		case ai.FilePart:
			if p.Source.IsID() {
				out = append(out, chatContentPart{Type: "file", File: &chatFile{FileID: p.Source.ID}})
				continue
			}

			if p.Source.IsURL() {
				return nil, fmt.Errorf("openai: Chat Completions file parts do not accept URL %q: %w", p.Source.URL, ai.ErrUnsupported)
			}

			out = append(out, chatContentPart{Type: "file", File: &chatFile{
				Filename: p.Name,
				FileData: dataURL(p.Source),
			}})
		default:
			return nil, fmt.Errorf("openai: part %T not supported in user messages", part)
		}
	}

	return out, nil
}

// imageURLFrom renders a media source as the image_url value: a remote URL
// as-is, inline bytes as a data: URL.
func imageURLFrom(src ai.MediaSource) (string, error) {
	if src.IsURL() {
		return src.URL, nil
	}

	if src.MIMEType == "" {
		return "", errors.New("openai: inline image data needs a MIME type")
	}

	return dataURL(src), nil
}

func dataURL(src ai.MediaSource) string {
	return "data:" + src.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(src.Data)
}

// chatAssistantFrom renders a prior assistant turn. OpenAI drops reasoning;
// compatible providers may require plaintext or structured continuation state.
func chatAssistantFrom(msg ai.Message, reasoningField ReasoningHistoryField) (chatMessage, error) {
	out := chatMessage{Role: "assistant"}
	var contentChunks []json.RawMessage

	if reasoningField != ReasoningHistoryContentChunks {
		if text := textOf(msg.Parts); text != "" {
			out.Content = text
		}
	}

	for _, part := range msg.Parts {
		if err := appendChatAssistantPart(&out, &contentChunks, part, reasoningField); err != nil {
			return chatMessage{}, err
		}
	}

	if len(contentChunks) > 0 {
		out.Content = contentChunks
	}

	return out, nil
}

func appendChatAssistantPart(
	out *chatMessage,
	contentChunks *[]json.RawMessage,
	part ai.Part,
	reasoningField ReasoningHistoryField,
) error {
	switch p := part.(type) {
	case ai.TextPart:
		if reasoningField != ReasoningHistoryContentChunks {
			return nil
		}

		raw, err := json.Marshal(mistralContentChunk{Type: typeText, Text: p.Text})
		if err != nil {
			return fmt.Errorf("encoding Mistral text chunk: %w", err)
		}

		*contentChunks = append(*contentChunks, raw)
	case ai.ReasoningPart:
		return appendChatReasoningHistory(out, contentChunks, p, reasoningField)
	case ai.ToolCallPart:
		out.ToolCalls = append(out.ToolCalls, chatToolCall{
			ID:   p.ID,
			Type: typeFunction,
			Function: chatFunctionCall{
				Name:      p.Name,
				Arguments: string(p.Args),
			},
		})
	}

	return nil
}

func appendChatReasoningHistory(
	out *chatMessage,
	contentChunks *[]json.RawMessage,
	part ai.ReasoningPart,
	reasoningField ReasoningHistoryField,
) error {
	state, hasState := decodeChatReasoningState(part.Signature)
	if hasState && reasoningField == ReasoningHistoryDetails && state.Kind == chatReasoningOpenRouter {
		out.ReasoningDetails = append(out.ReasoningDetails, cloneRawMessages(state.OpenRouterDetails)...)

		return nil
	}
	if hasState && reasoningField == ReasoningHistoryContentChunks && state.Kind == chatReasoningMistral {
		*contentChunks = append(*contentChunks, cloneRawMessage(state.MistralThinking))

		return nil
	}

	switch reasoningField {
	case ReasoningHistoryContent:
		out.ReasoningContent += part.Text
	case ReasoningHistoryReasoning, ReasoningHistoryDetails:
		out.Reasoning += part.Text
	case ReasoningHistoryContentChunks:
		raw, err := mistralThinkingChunkFromText(part.Text)
		if err != nil {
			return err
		}

		*contentChunks = append(*contentChunks, raw)
	}

	return nil
}

func mistralThinkingChunkFromText(text string) (json.RawMessage, error) {
	nested, err := json.Marshal(mistralContentChunk{Type: typeText, Text: text})
	if err != nil {
		return nil, fmt.Errorf("encoding Mistral thinking text: %w", err)
	}

	raw, err := json.Marshal(mistralContentChunk{
		Type:     typeThinking,
		Thinking: []json.RawMessage{nested},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding Mistral thinking chunk: %w", err)
	}

	return raw, nil
}

func contentPartsFromChat(content *json.RawMessage) ([]ai.Part, error) {
	if content == nil || len(*content) == 0 || string(*content) == "null" {
		return nil, nil
	}

	var text string
	if err := json.Unmarshal(*content, &text); err == nil {
		if text == "" {
			return nil, nil
		}

		return []ai.Part{ai.TextPart{Text: text}}, nil
	}

	var chunks []json.RawMessage
	if err := json.Unmarshal(*content, &chunks); err != nil {
		return nil, fmt.Errorf("content must be a string or content-chunk array: %w", err)
	}

	parts := make([]ai.Part, 0, len(chunks))
	for index, raw := range chunks {
		var chunk mistralContentChunk
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return nil, fmt.Errorf("decoding content[%d]: %w", index, err)
		}

		switch chunk.Type {
		case typeText:
			parts = append(parts, ai.TextPart{Text: chunk.Text})
		case typeThinking:
			text, err := mistralThinkingText(raw)
			if err != nil {
				return nil, fmt.Errorf("decoding content[%d]: %w", index, err)
			}

			signature, err := encodeChatReasoningState(chatReasoningState{
				Kind:            chatReasoningMistral,
				MistralThinking: cloneRawMessage(raw),
			})
			if err != nil {
				return nil, fmt.Errorf("preserving content[%d]: %w", index, err)
			}

			parts = append(parts, ai.ReasoningPart{Text: text, Signature: signature})
		default:
			return nil, fmt.Errorf("content[%d] has unsupported type %q", index, chunk.Type)
		}
	}

	return parts, nil
}

// chatToolResultsFrom renders each tool result part as its own role:"tool"
// message keyed by tool_call_id.
func chatToolResultsFrom(msg ai.Message) ([]chatMessage, error) {
	var out []chatMessage

	for _, part := range msg.Parts {
		result, ok := part.(ai.ToolResultPart)
		if !ok {
			return nil, fmt.Errorf("openai: tool messages may only contain tool results, got %T", part)
		}

		content, err := chatToolResultContent(result)
		if err != nil {
			return nil, err
		}

		out = append(out, chatMessage{
			Role:       "tool",
			ToolCallID: result.ToolCallID,
			Content:    content,
		})
	}

	return out, nil
}

// chatToolResultContent renders tool output: plain string for text-only
// results, a content-part array when the result carries images.
func chatToolResultContent(result ai.ToolResultPart) (any, error) {
	textOnly := true

	for _, part := range result.Content {
		if _, ok := part.(ai.TextPart); !ok {
			textOnly = false
			break
		}
	}

	if textOnly {
		return textOf(result.Content), nil
	}

	return chatContentFrom(result.Content)
}

func textOf(parts []ai.Part) string {
	var out strings.Builder

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			out.WriteString(text.Text)
		}
	}

	return out.String()
}

func chatToolsFrom(tools []ai.Tool) []chatTool {
	if len(tools) == 0 {
		return nil
	}

	out := make([]chatTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, chatTool{
			Type: typeFunction,
			Function: chatFunctionDef{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.EffectiveInputSchema(),
			},
		})
	}

	return out
}

func chatToolChoiceFrom(choice ai.ToolChoice) any {
	switch choice.Mode {
	case ai.ToolChoiceAuto:
		return "auto"
	case ai.ToolChoiceNone:
		return "none"
	case ai.ToolChoiceRequired:
		return "required"
	case ai.ToolChoiceTool:
		return chatToolChoiceForced{Type: typeFunction, Function: chatToolChoiceName{Name: choice.Name}}
	default:
		return nil
	}
}

// responseFromChat translates a Chat Completions response body into the
// portable shape.
func responseFromChat(body chatResponse, raw []byte, provider ai.Provider) (*ai.Response, error) {
	if len(body.Choices) == 0 {
		return nil, &ai.Error{
			Provider: provider,
			Message:  "response contained no choices",
			Raw:      raw,
		}
	}

	choice := body.Choices[0]
	msg := ai.Message{Role: ai.RoleAssistant}

	switch {
	case len(choice.Message.ReasoningDetails) > 0:
		text, err := openRouterReasoningText(choice.Message.ReasoningDetails)
		if err != nil {
			return nil, fmt.Errorf("decoding reasoning_details: %w", err)
		}

		signature, err := encodeChatReasoningState(chatReasoningState{
			Kind:              chatReasoningOpenRouter,
			OpenRouterDetails: cloneRawMessages(choice.Message.ReasoningDetails),
		})
		if err != nil {
			return nil, fmt.Errorf("preserving reasoning_details: %w", err)
		}

		msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: text, Signature: signature})
	case choice.Message.ReasoningContent != "":
		msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: choice.Message.ReasoningContent})
	case choice.Message.Reasoning != "":
		msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: choice.Message.Reasoning})
	}

	contentParts, err := contentPartsFromChat(choice.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("decoding assistant content: %w", err)
	}
	msg.Parts = append(msg.Parts, contentParts...)

	for _, call := range choice.Message.ToolCalls {
		msg.Parts = append(msg.Parts, ai.ToolCallPart{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: ai.JSON(call.Function.Arguments),
		})
	}

	return &ai.Response{
		ID:           body.ID,
		Model:        body.Model,
		Provider:     provider,
		Message:      msg,
		FinishReason: finishReasonFromChat(choice.FinishReason),
		Usage:        usageFromChat(body.Usage),
		Raw:          raw,
	}, nil
}

func finishReasonFromChat(reason string) ai.FinishReason {
	switch reason {
	case "stop":
		return ai.FinishStop
	case "length":
		return ai.FinishLength
	case "tool_calls", typeFunctionCall:
		return ai.FinishToolCalls
	case "content_filter":
		return ai.FinishContentFilter
	case "":
		return ""
	default:
		return ai.FinishOther
	}
}

func usageFromChat(u *chatUsage) ai.Usage {
	if u == nil {
		return ai.Usage{}
	}

	out := ai.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedInputTokens = u.PromptTokensDetails.CachedTokens
	}

	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}

	return out
}
