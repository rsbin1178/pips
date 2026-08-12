package gemini

import "github.com/rsbin1178/pips/ai"

// responseFrom translates a generateContent response body into the portable
// shape.
func responseFrom(body generateResponse, raw []byte) *ai.Response {
	resp := &ai.Response{
		ID:       body.ResponseID,
		Model:    body.ModelVersion,
		Provider: ai.ProviderGemini,
		Message:  ai.AssistantMessage{},
		Usage:    usageFrom(body.UsageMetadata),
		Raw:      raw,
	}

	if len(body.Candidates) == 0 {
		return resp
	}

	candidate := body.Candidates[0]
	resp.Message.Parts = partsFrom(candidate.Content.Parts)
	resp.FinishReason = finishReasonFrom(candidate.FinishReason)

	// Gemini reports STOP even when it emitted a tool call; normalize so
	// callers see the same tool_calls reason as other providers.
	if resp.FinishReason == ai.FinishStop && hasFunctionCall(resp.Message.Parts) {
		resp.FinishReason = ai.FinishToolCalls
	}

	return resp
}

// partsFrom converts wire parts, synthesizing tool-call ids so results can be
// matched back regardless of whether the model supplied ids.
func partsFrom(parts []wirePart) []ai.AssistantPart {
	var out []ai.AssistantPart

	toolIndex := 0

	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			out = append(out, ai.ToolCallPart{
				ID:   syntheticCallID(toolIndex, p.FunctionCall.ID, p.ThoughtSignature),
				Name: p.FunctionCall.Name,
				Args: p.FunctionCall.Args,
			})
			toolIndex++
		case p.Thought:
			out = append(out, ai.ReasoningPart{Text: p.Text, Signature: p.ThoughtSignature})
		case p.Text != "":
			out = append(out, ai.TextPart{Text: p.Text})
		}
	}

	return out
}

func finishReasonFrom(reason string) ai.FinishReason {
	switch reason {
	case "STOP":
		return ai.FinishStop
	case "MAX_TOKENS":
		return ai.FinishLength
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return ai.FinishContentFilter
	case "":
		return ""
	default:
		return ai.FinishOther
	}
}

func usageFrom(u *wireUsage) ai.Usage {
	if u == nil {
		return ai.Usage{}
	}

	return ai.Usage{
		InputTokens:       u.PromptTokenCount,
		OutputTokens:      u.CandidatesTokenCount + u.ThoughtsTokenCount,
		ReasoningTokens:   u.ThoughtsTokenCount,
		CachedInputTokens: u.CachedContentTokenCount,
	}
}

// hasFunctionCall reports whether the candidate parts include a tool call,
// used to normalize the finish reason (Gemini reports STOP even when it
// emitted a functionCall).
func hasFunctionCall(parts []ai.AssistantPart) bool {
	for _, p := range parts {
		if _, ok := p.(ai.ToolCallPart); ok {
			return true
		}
	}

	return false
}
