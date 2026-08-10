package ai_test

import (
	"errors"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// events builds an ai.Stream that yields the given events then stops.
func events(evs ...ai.StreamEvent) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func TestCollectTextAndUsage(t *testing.T) {
	t.Parallel()

	resp, err := ai.Collect(events(
		ai.StreamEvent{
			Type:     ai.StreamMessageStart,
			Provider: ai.ProviderGroq,
			ID:       "resp_1",
			Model:    "gpt-4o-2024-11-20",
		},
		ai.StreamEvent{Type: ai.StreamTextDelta, Text: "Hel"},
		ai.StreamEvent{Type: ai.StreamTextDelta, Text: "lo"},
		ai.StreamEvent{
			Type:         ai.StreamMessageEnd,
			FinishReason: ai.FinishStop,
			Usage:        &ai.Usage{InputTokens: 10, OutputTokens: 2},
		},
	))
	require.NoError(t, err)

	assert.Equal(t, "resp_1", resp.ID)
	assert.Equal(t, "gpt-4o-2024-11-20", resp.Model)
	assert.Equal(t, ai.ProviderGroq, resp.Provider)
	assert.Equal(t, "Hello", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, 10, resp.Usage.InputTokens)
	assert.IsType(t, ai.AssistantMessage{}, resp.Message)
	require.Len(t, resp.Message.Parts, 1)
}

func TestCollectInterleavedParts(t *testing.T) {
	t.Parallel()

	resp, err := ai.Collect(events(
		ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp_2"},
		ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thinking "},
		ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "hard"},
		ai.StreamEvent{Type: ai.StreamTextDelta, Text: "I'll check the weather."},
		ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallIndex: 0, ToolCallID: "call_1", ToolCallName: "get_weather"},
		ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: 0, ArgsDelta: `{"city":`},
		ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: 0, ArgsDelta: `"Paris"}`},
		ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: 0},
		ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishToolCalls},
	))
	require.NoError(t, err)

	require.Len(t, resp.Message.Parts, 3)
	assert.Equal(t, ai.ReasoningPart{Text: "thinking hard"}, resp.Message.Parts[0])
	assert.Equal(t, ai.TextPart{Text: "I'll check the weather."}, resp.Message.Parts[1])
	assert.Equal(t,
		ai.ToolCallPart{ID: "call_1", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)},
		resp.Message.Parts[2])

	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.Equal(t, "thinking hard", resp.Reasoning())
}

func TestCollectParallelToolCalls(t *testing.T) {
	t.Parallel()

	resp, err := ai.Collect(events(
		ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallIndex: 0, ToolCallID: "call_a", ToolCallName: "a"},
		ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallIndex: 1, ToolCallID: "call_b", ToolCallName: "b"},
		ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: 1, ArgsDelta: `{"n":2}`},
		ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: 0, ArgsDelta: `{"n":1}`},
		ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishToolCalls},
	))
	require.NoError(t, err)

	calls := resp.ToolCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, ai.JSON(`{"n":1}`), calls[0].Args)
	assert.Equal(t, ai.JSON(`{"n":2}`), calls[1].Args)
}

func TestCollectMidStreamErrorReturnsPartial(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	stream := func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "partial"}, nil) {
			return
		}

		yield(ai.StreamEvent{}, boom)
	}

	resp, err := ai.Collect(stream)
	require.ErrorIs(t, err, boom)
	require.NotNil(t, resp)
	assert.Equal(t, "partial", resp.Text())
}

func TestCollectReasoningSignature(t *testing.T) {
	t.Parallel()

	resp, err := ai.Collect(events(
		ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thinking"},
		ai.StreamEvent{Type: ai.StreamReasoningDelta, Signature: "sig-abc"},
		ai.StreamEvent{Type: ai.StreamTextDelta, Text: "answer"},
		ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop},
	))
	require.NoError(t, err)

	require.Len(t, resp.Message.Parts, 2)
	assert.Equal(t, ai.ReasoningPart{Text: "thinking", Signature: "sig-abc"}, resp.Message.Parts[0])
	assert.Equal(t, ai.TextPart{Text: "answer"}, resp.Message.Parts[1])
}
