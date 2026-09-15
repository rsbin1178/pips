package openai

import (
	"encoding/base64"
	"strings"

	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

const responsesReasoningSignaturePrefix = "responses:"

// responsesReasoningState is provider-opaque continuation data stored inside
// ai.ReasoningPart.Signature. The prefix keeps unrelated provider signatures
// from being interpreted as Responses input items.
type responsesReasoningState struct {
	ID               string            `json:"id,omitempty"`
	Summary          []responseSummary `json:"summary,omitempty"`
	Content          []responseContent `json:"content,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Status           string            `json:"status,omitempty"`
}

func encodeResponsesReasoningState(item responseItem) string {
	if item.ID == "" {
		return ""
	}

	var summary []responseSummary
	if item.Summary != nil {
		summary = *item.Summary
	}

	raw, err := jsonx.Marshal(responsesReasoningState{
		ID:               item.ID,
		Summary:          summary,
		Content:          item.Content,
		EncryptedContent: item.EncryptedContent,
		Status:           item.Status,
	})
	if err != nil {
		return ""
	}

	return responsesReasoningSignaturePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeResponsesReasoningState(signature string) (responsesReasoningState, bool) {
	encoded, ok := strings.CutPrefix(signature, responsesReasoningSignaturePrefix)
	if !ok {
		return responsesReasoningState{}, false
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return responsesReasoningState{}, false
	}

	var state responsesReasoningState
	if err := jsonx.Unmarshal(raw, &state); err != nil || state.ID == "" {
		return responsesReasoningState{}, false
	}

	return state, true
}

// reasoningTextOf returns the visible reasoning text of an output item. A
// summary is the provider's rendering for display, so it wins; providers that
// report raw reasoning text instead leave the summary empty.
func reasoningTextOf(item responseItem) string {
	if item.Summary != nil {
		var summary strings.Builder
		for _, part := range *item.Summary {
			summary.WriteString(part.Text)
		}

		if summary.Len() > 0 {
			return summary.String()
		}
	}

	var content strings.Builder

	for _, part := range item.Content {
		if part.Type == "reasoning_text" {
			content.WriteString(part.Text)
		}
	}

	return content.String()
}

// responseReasoningInputItem restores a Provider reasoning item for a later
// Responses request. Summary must be present even when it is empty. Visible
// text only becomes the summary for signatures persisted before structured
// summaries were retained: an item that already carries provider content would
// otherwise send the same reasoning twice.
func responseReasoningInputItem(state responsesReasoningState, fallbackText string) responseItem {
	summary := state.Summary
	if len(summary) == 0 && len(state.Content) == 0 && fallbackText != "" {
		summary = []responseSummary{{Type: typeSummaryText, Text: fallbackText}}
	}

	if summary == nil {
		summary = []responseSummary{}
	}

	return responseItem{
		ID:               state.ID,
		Type:             typeReasoning,
		Content:          state.Content,
		Summary:          &summary,
		EncryptedContent: state.EncryptedContent,
		Status:           state.Status,
	}
}
