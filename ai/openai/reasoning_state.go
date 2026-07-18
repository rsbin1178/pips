package openai

import (
	"encoding/base64"
	"strings"

	"github.com/rsbin/pips/ai/internal/jsonx"
)

const responsesReasoningSignaturePrefix = "responses:"

// responsesReasoningState is provider-opaque continuation data stored inside
// ai.ReasoningPart.Signature. The prefix keeps unrelated provider signatures
// from being interpreted as Responses input items.
type responsesReasoningState struct {
	ID               string `json:"id,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

func encodeResponsesReasoningState(item responseItem) string {
	if item.ID == "" && item.EncryptedContent == "" {
		return ""
	}

	raw, err := jsonx.Marshal(responsesReasoningState{
		ID:               item.ID,
		EncryptedContent: item.EncryptedContent,
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
	if err := jsonx.Unmarshal(raw, &state); err != nil || state.ID == "" && state.EncryptedContent == "" {
		return responsesReasoningState{}, false
	}

	return state, true
}
