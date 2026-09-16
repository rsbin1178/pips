//nolint:wsl_v5 // Wire validation and translation stay locally auditable.
package openai

import (
	"fmt"

	"github.com/rsbin1178/pips/ai"
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

	system, conversation, err := req.Messages.SplitSystem()
	if err != nil {
		return nil, fmt.Errorf("%s: invalid messages: %w", m.label(), err)
	}

	input, err := responseInputFrom(conversation, m.label())
	if err != nil {
		return nil, err
	}

	providerOpts := requestOptions(req, m.provider)
	tools, err := responsesToolsFrom(req.Tools, m.compat, providerOpts, m.label())
	if err != nil {
		return nil, err
	}

	out := responsesRequest{
		Model:           m.model,
		Input:           input,
		Instructions:    ai.JoinSystemText(system),
		Tools:           tools,
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

	return mergeExtraFields(out, providerOpts.ExtraFields)
}

func responseInputFrom(msgs ai.Messages, label string) ([]responseItem, error) {
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
	switch msg := msg.(type) {
	case ai.UserMessage:
		content, err := responseUserContent(msg.Parts)
		if err != nil {
			return nil, err
		}

		return []responseItem{{Type: typeMessage, Role: "user", Content: content}}, nil
	case ai.AssistantMessage:
		return responseAssistantItems(msg.Parts), nil
	case ai.ToolMessage:
		return responseToolOutputs(msg.Parts)
	case ai.SystemMessage:
		return nil, fmt.Errorf("%s: system message was not projected to instructions", label)
	default:
		return nil, fmt.Errorf("%s: unsupported message type %T", label, msg)
	}
}

func responseUserContent[T ai.Part](parts []T) ([]responseContent, error) {
	out := make([]responseContent, 0, len(parts))

	for _, part := range parts {
		switch p := any(part).(type) {
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
func responseAssistantItems(parts []ai.AssistantPart) []responseItem {
	var items []responseItem

	var text []responseContent

	flushText := func() {
		if len(text) == 0 {
			return
		}

		// A replayed assistant message is finished by definition, and the
		// schema marks its status required.
		items = append(items, responseItem{
			Type:    typeMessage,
			Role:    "assistant",
			Status:  "completed",
			Content: text,
		})
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
			items = append(items, responseReasoningInputItem(state, p.Text))
		case ai.ToolCallPart:
			flushText()

			callID, itemID := decodeResponsesToolCallID(p.ID)

			items = append(items, responseItem{
				Type:      typeFunctionCall,
				ID:        functionCallItemID(callID, itemID),
				CallID:    callID,
				Name:      p.Name,
				Arguments: string(p.Args),
			})
		}
	}

	flushText()

	return items
}

func responseToolOutputs(parts []ai.ToolResultPart) ([]responseItem, error) {
	var out []responseItem

	for _, result := range parts {
		callID, _ := decodeResponsesToolCallID(result.ToolCallID)

		out = append(out, responseItem{
			Type:   "function_call_output",
			CallID: callID,
			Output: textOf(ai.ProviderParts(result.Content)),
		})
	}

	return out, nil
}

func responsesToolsFrom(
	tools []ai.Tool,
	compat Compatibility,
	reqOpts RequestOptions,
	label string,
) ([]responsesTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	out := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		if !tool.IsEnabled() {
			continue
		}

		if tool.IsProviderExecuted() {
			if reqOpts.DisableBuiltinTools {
				continue
			}

			switch compat.resolvedBuiltinTools() {
			case BuiltinToolsReject:
				return nil, fmt.Errorf(
					"%s: provider-executed tool %q is not supported: %w",
					label,
					tool.Name,
					ai.ErrUnsupported,
				)
			case BuiltinToolsStrip:
				continue
			case BuiltinToolsAllow:
				t := responsesTool{
					Type: tool.ProviderType,
				}
				if t.Type == "" {
					t.Type = tool.Name
				}
				if tool.ProviderData != nil {
					switch d := tool.ProviderData.(type) {
					case FileSearchData:
						t.VectorStoreIDs = d.VectorStoreIDs
					case *FileSearchData:
						if d != nil {
							t.VectorStoreIDs = d.VectorStoreIDs
						}
					}
				}
				out = append(out, t)
			}
			continue
		}

		out = append(out, responsesTool{
			Type:        typeFunction,
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.EffectiveInputSchema(),
		})
	}

	return out, nil
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
	msg := ai.AssistantMessage{}
	var citations []ai.Citation
	var queries []string

	for _, item := range body.Output {
		appendOutputItem(&msg, item)

		if item.Type == typeMessage {
			for _, content := range item.Content {
				for _, ann := range content.Annotations {
					if ann.Type == "url_citation" {
						citations = append(citations, ai.Citation{
							URL:   ann.URL,
							Title: ann.Title,
							Index: len(citations),
							TextRange: &ai.TextRange{
								Start: ann.StartIndex,
								End:   ann.EndIndex,
							},
						})
					}
				}
			}
		}

		if item.Type == "web_search_call" && item.Action != nil && item.Action.Query != "" {
			queries = append(queries, item.Action.Query)
		}
	}

	var grounding *ai.GroundingMetadata
	if len(queries) > 0 {
		grounding = &ai.GroundingMetadata{
			WebSearchQueries: queries,
		}
	}

	return &ai.Response{
		ID:           body.ID,
		Model:        body.Model,
		Provider:     provider,
		Message:      msg,
		FinishReason: finishReasonFromResponses(body),
		Usage:        usageFromResponses(body.Usage),
		Citations:    citations,
		Grounding:    grounding,
		Raw:          raw,
	}
}

func appendOutputItem(msg *ai.AssistantMessage, item responseItem) {
	switch item.Type {
	case typeMessage:
		for _, content := range item.Content {
			if content.Type == "output_text" {
				msg.Parts = append(msg.Parts, ai.TextPart{Text: content.Text})
			}
		}
	case typeReasoning:
		text := reasoningTextOf(item)
		signature := encodeResponsesReasoningState(item)
		if text != "" || signature != "" {
			msg.Parts = append(msg.Parts, ai.ReasoningPart{Text: text, Signature: signature})
		}
	case typeFunctionCall:
		msg.Parts = append(msg.Parts, ai.ToolCallPart{
			ID:   encodeResponsesToolCallID(item.CallID, item.ID),
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
