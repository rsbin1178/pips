package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatReasoningStateRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state chatReasoningState
	}{
		{
			name: "openrouter details",
			state: chatReasoningState{
				Kind: chatReasoningOpenRouter,
				OpenRouterDetails: []json.RawMessage{
					json.RawMessage(`{"type":"reasoning.text","text":"checking","signature":"sig"}`),
					json.RawMessage(`{"type":"reasoning.encrypted","data":"opaque"}`),
				},
			},
		},
		{
			name: "mistral thinking",
			state: chatReasoningState{
				Kind: chatReasoningMistral,
				MistralThinking: json.RawMessage(
					`{"type":"thinking","thinking":[{"type":"text","text":"checking"}],"signature":"sig","closed":true}`,
				),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			signature, err := encodeChatReasoningState(tc.state)
			require.NoError(t, err)
			assert.NotEmpty(t, signature)

			decoded, ok := decodeChatReasoningState(signature)
			require.True(t, ok)
			assert.Equal(t, tc.state.Kind, decoded.Kind)
			assert.JSONEq(t, marshalJSON(t, tc.state), marshalJSON(t, decoded))
		})
	}
}

func TestChatReasoningStateRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	unknownRaw, err := json.Marshal(chatReasoningState{Kind: "future"})
	require.NoError(t, err)

	tests := []struct {
		name      string
		signature string
	}{
		{name: "foreign", signature: "provider:opaque"},
		{name: "empty", signature: chatReasoningSignaturePrefix},
		{name: "malformed base64", signature: chatReasoningSignaturePrefix + "%%%"},
		{
			name:      "unknown kind",
			signature: chatReasoningSignaturePrefix + base64.RawURLEncoding.EncodeToString(unknownRaw),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, ok := decodeChatReasoningState(tc.signature)
			assert.False(t, ok)
		})
	}

	_, err = encodeChatReasoningState(chatReasoningState{
		Kind:              chatReasoningOpenRouter,
		OpenRouterDetails: []json.RawMessage{json.RawMessage(`{"type":"future"}`)},
	})
	require.Error(t, err)

	_, err = encodeChatReasoningState(chatReasoningState{
		Kind:            chatReasoningMistral,
		MistralThinking: json.RawMessage(`{"type":"text","text":"not thinking"}`),
	})
	require.Error(t, err)

	_, err = encodeChatReasoningState(chatReasoningState{
		Kind:            chatReasoningMistral,
		MistralThinking: json.RawMessage(`{"type":"thinking","signature":"missing-array"}`),
	})
	require.Error(t, err)
}

func marshalJSON(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return string(raw)
}
