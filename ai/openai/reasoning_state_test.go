package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesReasoningStateRoundTrip(t *testing.T) {
	t.Parallel()

	summary := []responseSummary{
		{Type: typeSummaryText, Text: "first"},
		{Type: typeSummaryText, Text: "second"},
	}
	item := responseItem{
		ID:      "rs_1",
		Type:    typeReasoning,
		Summary: &summary,
		Content: []responseContent{
			{Type: "reasoning_text", Text: "provider reasoning content"},
		},
		EncryptedContent: "encrypted-state",
		Status:           "completed",
	}

	signature := encodeResponsesReasoningState(item)
	require.NotEmpty(t, signature)

	state, ok := decodeResponsesReasoningState(signature)
	require.True(t, ok)

	replayed := responseReasoningInputItem(state, "unused fallback")
	raw, err := json.Marshal(replayed)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"id":"rs_1",
		"type":"reasoning",
		"summary":[
			{"type":"summary_text","text":"first"},
			{"type":"summary_text","text":"second"}
		],
		"content":[{"type":"reasoning_text","text":"provider reasoning content"}],
		"encrypted_content":"encrypted-state",
		"status":"completed"
	}`, string(raw))
}

func TestResponseReasoningInputItemLegacySummary(t *testing.T) {
	t.Parallel()

	legacy := responsesReasoningState{
		ID:               "rs_legacy",
		EncryptedContent: "legacy-state",
	}

	tests := []struct {
		name         string
		state        responsesReasoningState
		fallbackText string
		wantSummary  []responseSummary
	}{
		{
			name:         "visible text becomes summary",
			state:        legacy,
			fallbackText: "checking",
			wantSummary: []responseSummary{
				{Type: typeSummaryText, Text: "checking"},
			},
		},
		{
			name:        "missing text becomes empty array",
			state:       legacy,
			wantSummary: []responseSummary{},
		},
		{
			name: "provider content is not restated as summary",
			state: responsesReasoningState{
				ID:      "rs_text",
				Content: []responseContent{{Type: "reasoning_text", Text: "checking"}},
			},
			fallbackText: "checking",
			wantSummary:  []responseSummary{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replayed := responseReasoningInputItem(tt.state, tt.fallbackText)

			require.NotNil(t, replayed.Summary)
			assert.Equal(t, tt.wantSummary, *replayed.Summary)

			raw, err := json.Marshal(replayed)
			require.NoError(t, err)

			var encoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &encoded))
			assert.Contains(t, encoded, "summary")
			assert.NotNil(t, encoded["summary"])
		})
	}
}

func TestResponseItemSummaryPresence(t *testing.T) {
	t.Parallel()

	emptySummary := []responseSummary{}
	tests := []struct {
		name        string
		item        responseItem
		wantSummary bool
	}{
		{
			name: "empty reasoning summary is present",
			item: responseItem{
				ID:      "rs_empty",
				Type:    typeReasoning,
				Summary: &emptySummary,
			},
			wantSummary: true,
		},
		{
			name: "message summary is omitted",
			item: responseItem{
				Type:    typeMessage,
				Role:    "assistant",
				Content: []responseContent{{Type: "output_text", Text: "done"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(tt.item)
			require.NoError(t, err)

			var encoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &encoded))

			if tt.wantSummary {
				assert.Equal(t, []any{}, encoded["summary"])
				return
			}

			assert.NotContains(t, encoded, "summary")
		})
	}
}

func TestResponsesReasoningStateRejectsInvalidSignatures(t *testing.T) {
	t.Parallel()

	assert.Empty(t, encodeResponsesReasoningState(responseItem{
		Type:             typeReasoning,
		EncryptedContent: "encrypted-state",
	}))

	raw, err := json.Marshal(responsesReasoningState{EncryptedContent: "encrypted-state"})
	require.NoError(t, err)

	tests := []struct {
		name      string
		signature string
	}{
		{name: "foreign prefix", signature: "anthropic:signature"},
		{name: "malformed base64", signature: responsesReasoningSignaturePrefix + "%%%"},
		{
			name:      "missing required id",
			signature: responsesReasoningSignaturePrefix + base64.RawURLEncoding.EncodeToString(raw),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ok := decodeResponsesReasoningState(tt.signature)
			assert.False(t, ok)
		})
	}
}
