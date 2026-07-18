package anthropic_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const thinkingStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user asks "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"a simple question."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Paris"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" is the capital."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

const toolsStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s2","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"usage":{"input_tokens":50,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamThinkingAndText(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, thinkingStream))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("capital?")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningMedium},
	}))
	require.NoError(t, err)

	require.Len(t, resp.Message.Parts, 2)
	reasoning := as[ai.ReasoningPart](t, resp.Message.Parts[0])
	assert.Equal(t, "The user asks a simple question.", reasoning.Text)
	assert.Equal(t, "sig-xyz", reasoning.Signature)
	assert.Equal(t, "Paris is the capital.", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)

	// input_tokens folds in cache reads (25 + 5), output from message_delta.
	assert.Equal(t, 30, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.CachedInputTokens)
	assert.Equal(t, 15, resp.Usage.OutputTokens)
}

func TestStreamToolCalls(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, toolsStream))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather?")},
		Tools:    []ai.Tool{{Name: "get_weather"}},
	}))
	require.NoError(t, err)

	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "toolu_a", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris"}`, string(calls[0].Args))
	assert.Equal(t, 20, resp.Usage.OutputTokens)
}

func TestStreamErrorEvent(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveRawSSE(t,
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))

	var gotErr error

	for _, err := range model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}}) {
		if err != nil {
			gotErr = err
			break
		}
	}

	require.Error(t, gotErr)

	var apiErr *ai.Error
	require.ErrorAs(t, gotErr, &apiErr)
	assert.Equal(t, "overloaded_error", apiErr.Type)
	// The mid-stream error wraps the same class sentinel as the HTTP path.
	require.ErrorIs(t, gotErr, ai.ErrOverloaded)
	assert.True(t, ai.IsRetryable(gotErr))
}
