package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResponsesToolCallIDRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		callID     string
		itemID     string
		wantID     string
		wantCallID string
		wantItemID string
	}{
		{
			name:       "item id encoded",
			callID:     "call_a",
			itemID:     "fc_1",
			wantID:     "call_a|id=fc_1",
			wantCallID: "call_a",
			wantItemID: "fc_1",
		},
		{
			name:       "no item id",
			callID:     "call_a",
			wantID:     "call_a",
			wantCallID: "call_a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id := encodeResponsesToolCallID(tc.callID, tc.itemID)
			assert.Equal(t, tc.wantID, id)

			callID, itemID := decodeResponsesToolCallID(id)
			assert.Equal(t, tc.wantCallID, callID)
			assert.Equal(t, tc.wantItemID, itemID)
		})
	}
}

func TestResponsesToolCallIDDecodesForeignIDs(t *testing.T) {
	t.Parallel()

	// Ids minted elsewhere carry no item id and must survive unchanged.
	callID, itemID := decodeResponsesToolCallID("toolu_01ABC")
	assert.Equal(t, "toolu_01ABC", callID)
	assert.Empty(t, itemID)
}

func TestFunctionCallItemIDSynthesizedWithoutProviderID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "fc_1", functionCallItemID("call_a", "fc_1"))
	assert.Equal(t, "fc_call_a", functionCallItemID("call_a", ""))
}
