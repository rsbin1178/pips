package anthropic

import "github.com/rsbin/pips/ai"

// responseFrom translates a Messages response body into the portable shape.
// A single forced-tool structured-output call is unwrapped into text so the
// document is available via [ai.Response.Text]. structuredTool is the name of
// that forced tool, or "" when the request did not request structured output.
func responseFrom(body messagesResponse, raw []byte, structuredTool string) *ai.Response {
	msg := ai.Message{Role: ai.RoleAssistant}

	for _, block := range body.Content {
		if part := partFromBlock(block, structuredTool); part != nil {
			msg.Parts = append(msg.Parts, part)
		}
	}

	return &ai.Response{
		ID:           body.ID,
		Model:        body.Model,
		Provider:     ai.ProviderAnthropic,
		Message:      msg,
		FinishReason: finishReasonFrom(body.StopReason),
		Usage:        usageFrom(body.Usage),
		Raw:          raw,
	}
}

func partFromBlock(block wireBlock, structuredTool string) ai.Part {
	switch block.Type {
	case blockTypeText:
		return ai.TextPart{Text: block.Text}
	case blockTypeThinking:
		return ai.ReasoningPart{Text: block.Thinking, Signature: block.Signature}
	case blockTypeRedactedThinking:
		return ai.ReasoningPart{Redacted: true, Signature: block.Data}
	case blockTypeToolUse:
		return toolCallPartFrom(block, structuredTool)
	default:
		return nil
	}
}

// toolCallPartFrom converts a tool_use block. The structured-output tool call
// is unwrapped: its JSON input becomes the message text, matching how the
// other providers deliver schema-constrained output.
func toolCallPartFrom(block wireBlock, structuredTool string) ai.Part {
	if structuredTool != "" && block.Name == structuredTool {
		return ai.TextPart{Text: string(block.Input)}
	}

	return ai.ToolCallPart{ID: block.ID, Name: block.Name, Args: block.Input}
}

func finishReasonFrom(reason string) ai.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return ai.FinishStop
	case "max_tokens":
		return ai.FinishLength
	case blockTypeToolUse:
		return ai.FinishToolCalls
	case "refusal":
		return ai.FinishContentFilter
	case "":
		return ""
	default:
		return ai.FinishOther
	}
}

func usageFrom(u wireUsage) ai.Usage {
	return ai.Usage{
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
	}
}

// finishReasonForStructured reports the finish reason to surface for a forced
// structured-output call: the wire stop_reason is "tool_use", but from the
// caller's perspective the model produced its (text) answer and stopped.
func finishReasonForStructured(req ai.Request, wire ai.FinishReason) ai.FinishReason {
	if req.ResponseFormat != nil && wire == ai.FinishToolCalls {
		return ai.FinishStop
	}

	return wire
}
