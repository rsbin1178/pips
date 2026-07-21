//nolint:wsl_v5 // Wire validation and translation stay locally auditable.
package openai

import (
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
)

// responsesRequestFrom translates a portable request into the Responses wire
// shape. The system prompt becomes top-level instructions; messages become a
// flat list of typed input items.
//
//nolint:gocyclo // Unsupported-option checks and wire fields remain auditable together.
func (m *Model) responsesRequestFrom(req ai.Request, stream bool) (any, error) {
	if req.TopK != nil || req.Seed != nil || req.FrequencyPenalty != nil ||
		req.PresencePenalty != nil || len(req.Stop) != 0 {
		return nil, fmt.Errorf(
			"%s: request option is not supported by Responses: %w",
			m.label(),
			ai.ErrUnsupported,
		)
	}
	if req.Reasoning != nil && req.Reasoning.BudgetTokens != 0 {
		return nil, fmt.Errorf(
			"%s: reasoning budget is not supported by Responses: %w",
			m.label(),
			ai.ErrUnsupported,
		)
	}
	if req.Reasoning != nil && req.Reasoning.Mode == ai.ReasoningModeAdaptive {
		return nil, fmt.Errorf(
			"%s: adaptive reasoning is not supported by Responses: %w",
			m.label(),
			ai.ErrUnsupported,
		)
	}

	input, err := responseInputFrom(req.Messages, m.label())
	if err != nil {
		return nil, err
	}

	out := responsesRequest{
		Model:           m.model,
		Input:           input,
		Instructions:    req.System,
		Tools:           responsesToolsFrom(req.Tools),
		ToolChoice:      responsesToolChoiceFrom(req.ToolChoice),
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: req.MaxTokens,
		Stream:          stream,
	}
	if req.LogProbs != nil {
		if !req.LogProbs.Enabled {
			return nil, fmt.Errorf(
				"%s: disabling logprobs is not supported by Responses: %w",
				m.label(),
				ai.ErrUnsupported,
			)
		}
		out.TopLogProbs = &req.LogProbs.Top
	}
	if m.compat.IncludeEncryptedReasoning {
		out.Include = []string{"reasoning.encrypted_content"}
	}

	if rf := req.ResponseFormat; rf != nil {
		name := rf.Name
		if name == "" {
			name = "output"
		}

		out.Text = &responsesText{Format: &responsesFormat{
			Type:   "json_schema",
			Name:   name,
			Schema: rf.Schema,
			Strict: rf.Strict,
		}}
	}

	if req.Reasoning != nil {
		effort := string(req.Reasoning.Effort)
		if req.Reasoning.Mode == ai.ReasoningModeDisabled {
			effort = string(ai.ReasoningNone)
		}
		r := &responsesReasoning{Effort: effort}
		if req.Reasoning.IncludeSummary {
			r.Summary = "auto"
		}

		if r.Effort != "" || r.Summary != "" {
			out.Reasoning = r
		}
	}

	return mergeExtraFields(out, requestOptions(req, m.provider).ExtraFields)
}

func responseInputFrom(msgs []ai.Message, label string) ([]responseItem, error) {
	var out []responseItem

	for _, msg := range msgs {
		items, err := responseItemsFrom(msg, label)
		if err != nil {
			return nil, err
		}

		out = append(out, items...)
	}

	return out, nil
}

func responseItemsFrom(msg ai.Message, label string) ([]responseItem, error) {
	switch msg.Role {
	case ai.RoleSystem:
		return []responseItem{{Type: typeMessage, Role: "system", Content: []responseContent{{Type: "input_text", Text: textOf(msg.Parts)}}}}, nil
	case ai.RoleUser:
		content, err := responseUserContent(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []responseItem{{Type: typeMessage, Role: "user", Content: content}}, nil
	case ai.RoleAssistant:
		return responseAssistantItems(msg.Parts), nil
	case ai.RoleTool:
		return responseToolOutputs(msg.Parts)
	default:
		return nil, fmt.Errorf("%s: unsupported message role %q", label, msg.Role)
	}
}

func responseUserContent(parts []ai.Part) ([]responseContent, error) {
	out := make([]responseContent, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, responseContent{Type: "input_text", Text: p.Text})
		case ai.ImagePart:
			url, err := imageURLFrom(p.Source)
			if err != nil {
				return nil, err
			}

			out = append(out, responseContent{Type: "input_image", ImageURL: url})
		case ai.FilePart:
			if p.Source.IsID() {
				out = append(out, responseContent{Type: "input_file", FileID: p.Source.ID})
				continue
			}

			if p.Source.IsURL() {
				out = append(out, responseContent{Type: "input_file", FileURL: p.Source.URL})
				continue
			}

			out = append(out, responseContent{Type: "input_file", Filename: p.Name, FileData: dataURL(p.Source)})
		default:
			return nil, fmt.Errorf("openai: part %T not supported in user messages", part)
		}
	}

	return out, nil
}

// responseAssistantItems renders a prior assistant turn. Text becomes an
// output_text message, replayable reasoning becomes a reasoning input item,
// and each tool call becomes its own function_call item.
func responseAssistantItems(parts []ai.Part) []responseItem {
	var items []responseItem

	var text []responseContent

	flushText := func() {
		if len(text) == 0 {
			return
		}

		items = append(items, responseItem{Type: typeMessage, Role: "assistant", Content: text})
		text = nil
	}

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			text = append(text, responseContent{Type: "output_text", Text: p.Text})
		case ai.ReasoningPart:
			state, ok := decodeResponsesReasoningState(p.Signature)
			if !ok {
				continue
			}

			flushText()

			items = append(items, responseItem{
				ID:               state.ID,
				Type:             typeReasoning,
				EncryptedContent: state.EncryptedContent,
			})
		case ai.ToolCallPart:
			flushText()

			items = append(items, responseItem{
				Type:      typeFunctionCall,
				CallID:    p.ID,
				Name:      p.Name,
				Arguments: string(p.Args),
			})
		}
	}

	flushText()

	return items
}

func responseToolOutputs(parts []ai.Part) ([]responseItem, error) {
	var out []responseItem

	for _, part := range parts {
		result, ok := part.(ai.ToolResultPart)
		if !ok {
			return nil, fmt.Errorf("openai: tool messages may only contain tool results, got %T", part)
		}

		out = append(out, responseItem{
			Type:   "function_call_output",
			CallID: result.ToolCallID,
			Output: textOf(result.Content),
		})
	}

	return out, nil
}

func responsesToolsFrom(tools []ai.Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}

	out := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, responsesTool{
			Type:        typeFunction,
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
		})
	}

	return out
}

func responsesToolChoiceFrom(choice ai.ToolChoice) any {
	switch choice.Mode {
	case ai.ToolChoiceAuto:
		return "auto"
	case ai.ToolChoiceNone:
		return "none"
	case ai.ToolChoiceRequired:
		return "required"
	case ai.ToolChoiceTool:
		return responsesToolChoiceForced{Type: typeFunction, Name: choice.Name}
	default:
		return nil
	}
}

// responseFromResponses translates a Responses response body into the
// portable shape.
func responseFromResponses(body responsesResponse, raw []byte, provider ai.Provider) *ai.Response {
	msg := ai.Message{Role: ai.RoleAssistant}

	for _, item := range body.Output {
		appendOutputItem(&msg, item)
	}

	return &ai.Response{
		ID:           body.ID,
		Model:        body.Model,
		Provider:     provider,
		Message:      msg,
		FinishReason: finishReasonFromResponses(body),
		Usage:        usageFromResponses(body.Usage),
		Raw:          raw,
	}
}

func appendOutputItem(msg *ai.Message, item responseItem) {
	switch item.Type {
	case typeMessage:
		for _, content := range item.Content {
			if content.Type == "output_text" {
				msg.Parts = append(msg.Parts, ai.TextPart{Text: content.Text})
			}
		}
	case typeReasoning:
		var text strings.Builder
		for _, s := range item.Summary {
			text.WriteString(s.Text)
		}

		signature := encodeResponsesReasoningState(item)
		if text.Len() > 0 || signature != "" {
			msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: text.String(), Signature: signature})
		}
	case typeFunctionCall:
		msg.Parts = append(msg.Parts, ai.ToolCallPart{
			ID:   item.CallID,
			Name: item.Name,
			Args: ai.JSON(item.Arguments),
		})
	}
}

func finishReasonFromResponses(body responsesResponse) ai.FinishReason {
	for _, item := range body.Output {
		if item.Type == typeFunctionCall {
			return ai.FinishToolCalls
		}
	}

	if body.IncompleteDetails != nil && body.IncompleteDetails.Reason == "max_output_tokens" {
		return ai.FinishLength
	}

	switch body.Status {
	case "completed":
		return ai.FinishStop
	case "incomplete":
		return ai.FinishLength
	case "failed":
		return ai.FinishOther
	default:
		return ai.FinishStop
	}
}

func usageFromResponses(u *responsesUsage) ai.Usage {
	if u == nil {
		return ai.Usage{}
	}

	out := ai.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CachedInputTokens = u.InputTokensDetails.CachedTokens
	}

	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}

	return out
}
