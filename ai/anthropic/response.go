package anthropic

import "github.com/rsbin/pips/ai"

// responseFrom translates a Messages response body into the portable shape.
func responseFrom(body messagesResponse, raw []byte) *ai.Response {
	msg := ai.Message{Role: ai.RoleAssistant}

	for _, block := range body.Content {
		if part := partFromBlock(block); part != nil {
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

func partFromBlock(block wireBlock) ai.Part {
	switch block.Type {
	case blockTypeText:
		return ai.TextPart{Text: block.Text}
	case blockTypeThinking:
		return ai.ReasoningPart{Text: block.Thinking, Signature: block.Signature}
	case blockTypeRedactedThinking:
		return ai.ReasoningPart{Redacted: true, Signature: block.Data}
	case blockTypeToolUse:
		return ai.ToolCallPart{ID: block.ID, Name: block.Name, Args: block.Input}
	default:
		return nil
	}
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
