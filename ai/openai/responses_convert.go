package openai

import (
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
)

// responsesRequestFrom translates a portable request into the Responses wire
// shape. The system prompt becomes top-level instructions; messages become a
// flat list of typed input items.
func (m *Model) responsesRequestFrom(req ai.Request, stream bool) (any, error) {
	input, err := responseInputFrom(req.Messages)
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
		r := &responsesReasoning{Effort: string(req.Reasoning.Effort)}
		if req.Reasoning.IncludeSummary {
			r.Summary = "auto"
		}

		if r.Effort != "" || r.Summary != "" {
			out.Reasoning = r
		}
	}

	return mergeExtraFields(out, requestOptions(req).ExtraFields)
}

func responseInputFrom(msgs []ai.Message) ([]responseItem, error) {
	var out []responseItem

	for _, msg := range msgs {
		items, err := responseItemsFrom(msg)
		if err != nil {
			return nil, err
		}

		out = append(out, items...)
	}

	return out, nil
}

func responseItemsFrom(msg ai.Message) ([]responseItem, error) {
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
		return nil, fmt.Errorf("openai: unsupported message role %q", msg.Role)
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
			if p.Source.IsURL() {
				return nil, fmt.Errorf("openai: file parts require inline data, got URL %q: %w", p.Source.URL, ai.ErrUnsupported)
			}

			out = append(out, responseContent{Type: "input_file", Filename: p.Name, FileData: dataURL(p.Source)})
		default:
			return nil, fmt.Errorf("openai: part %T not supported in user messages", part)
		}
	}

	return out, nil
}

// responseAssistantItems renders a prior assistant turn. Text becomes an
// output_text message; each tool call becomes its own function_call item.
// Reasoning is dropped (the API reconstructs it from server-side state).
func responseAssistantItems(parts []ai.Part) []responseItem {
	var items []responseItem

	var text []responseContent

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			text = append(text, responseContent{Type: "output_text", Text: p.Text})
		case ai.ToolCallPart:
			items = append(items, responseItem{
				Type:      typeFunctionCall,
				CallID:    p.ID,
				Name:      p.Name,
				Arguments: string(p.Args),
			})
		}
	}

	if len(text) > 0 {
		items = append([]responseItem{{Type: typeMessage, Role: "assistant", Content: text}}, items...)
	}

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
func responseFromResponses(body responsesResponse, raw []byte) *ai.Response {
	msg := ai.Message{Role: ai.RoleAssistant}

	for _, item := range body.Output {
		appendOutputItem(&msg, item)
	}

	return &ai.Response{
		ID:           body.ID,
		Model:        body.Model,
		Provider:     ai.ProviderOpenAI,
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
	case "reasoning":
		var text strings.Builder
		for _, s := range item.Summary {
			text.WriteString(s.Text)
		}

		if text.Len() > 0 {
			msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: text.String()})
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
