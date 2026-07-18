package openai

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
)

// chatRequestFrom translates a portable request into the Chat Completions
// wire shape.
func (m *Model) chatRequestFrom(req ai.Request, stream bool) (any, error) {
	messages, err := chatMessagesFrom(req, m.compat, m.label())
	if err != nil {
		return nil, err
	}

	out := chatRequest{
		Model:       m.model,
		Messages:    messages,
		Tools:       chatToolsFrom(req.Tools),
		ToolChoice:  chatToolChoiceFrom(req.ToolChoice),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      stream,
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

	return mergeExtraFields(out, requestOptions(req, m.provider).ExtraFields)
}

func applyChatReasoning(out *chatRequest, reasoning *ai.ReasoningConfig, compat Compatibility, label string) error {
	if reasoning == nil {
		return nil
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
		return []chatMessage{chatAssistantFrom(msg, compat.ReasoningHistory)}, nil
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
			out = append(out, chatContentPart{Type: "text", Text: p.Text})
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
// compatible providers may require it on a provider-specific history field.
func chatAssistantFrom(msg ai.Message, reasoningField ReasoningHistoryField) chatMessage {
	out := chatMessage{Role: "assistant"}

	if text := textOf(msg.Parts); text != "" {
		out.Content = text
	}

	for _, part := range msg.Parts {
		switch p := part.(type) {
		case ai.ReasoningPart:
			switch reasoningField {
			case ReasoningHistoryContent:
				out.ReasoningContent += p.Text
			case ReasoningHistoryReasoning:
				out.Reasoning += p.Text
			}
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
	}

	return out
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
				Parameters:  tool.InputSchema,
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

	if choice.Message.ReasoningContent != "" {
		msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: choice.Message.ReasoningContent})
	} else if choice.Message.Reasoning != "" {
		msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: choice.Message.Reasoning})
	}

	if choice.Message.Content != nil && *choice.Message.Content != "" {
		msg.Parts = append(msg.Parts, ai.TextPart{Text: *choice.Message.Content})
	}

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
