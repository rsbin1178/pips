package harness_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHarnessEarlyBreakSettlesDanglingToolCalls covers the stop that lands
// inside a tool batch: the assistant message carrying the call is already
// durable, the results are not, and the consumer is gone. The harness answers
// the call so the tail can still run.
func TestHarnessEarlyBreakSettlesDanglingToolCalls(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "add", `{"a":2,"b":3}`),
		textResponse("It is 5.", 30),
	)

	sess := buildSession(t)
	h, err := harness.New(model, sess, harness.WithTools(addTool()))
	require.NoError(t, err)

	for ev, streamErr := range h.PromptStream(t.Context(), "2+3?") {
		require.NoError(t, streamErr)

		if _, started := ev.Payload().(agent.ToolStarted); started {
			break
		}
	}

	pending, err := sess.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending, "the stop leaves no unanswered call behind")

	entries := sess.Entries()
	require.Len(t, entries, 3) // user, assistant call, settled result

	results, ok := entries[2].Message.(ai.ToolMessage)
	require.True(t, ok)
	require.Len(t, results.Parts, 1)
	assert.Equal(t, "c1", results.Parts[0].ToolCallID)
	assert.Equal(t, "add", results.Parts[0].Name)

	answer, ok := results.Parts[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, answer.Text, "was not executed")
	assert.True(t, results.Parts[0].IsError, "an unexecuted call is an error result")

	assert.Equal(t, harness.PhaseIdle, h.Phase())

	// The settled tail is runnable: the next prompt reaches the model.
	_, err = h.Prompt(t.Context(), "and then?")
	require.NoError(t, err)
}

// TestHarnessPauseKeepsPendingToolCalls is the counterpart: a run that paused
// for approval owns its pending calls, and settling them would answer a
// question the caller still has to decide.
func TestHarnessPauseKeepsPendingToolCalls(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "add", `{"a":1,"b":1}`),
		textResponse("resumed", 10),
	)

	sess := buildSession(t)
	h, err := harness.New(model, sess,
		harness.WithTools(addTool()),
		harness.WithAgentOptions(agent.WithBeforeTool(
			func(_ context.Context, _ agent.ToolCallInfo) agent.ToolDecision {
				return agent.ToolDecision{Action: agent.ToolDecisionPause}
			},
		)),
	)
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "go")
	require.NoError(t, err)
	require.Equal(t, agent.StopPaused, result.Stop)

	pending, err := sess.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1, "a paused call stays pending for the caller")

	err = h.ResolvePending(t.Context(), func(_ context.Context, _ ai.ToolCallPart) ([]ai.Part, error) {
		return agent.TextResult("2"), nil
	})
	require.NoError(t, err)

	final, err := h.Prompt(t.Context(), "and then?")
	require.NoError(t, err)
	assert.Equal(t, "resumed", final.Text())
}

func TestSessionSettleDanglingToolCallReasons(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reason harness.DanglingToolCallReason
		want   string
	}{
		{
			name: "user cancel", reason: harness.DanglingToolCallUserCancelled,
			want: "cancelled by the user",
		},
		{
			name: "harness halt", reason: harness.DanglingToolCallHarnessHalted,
			want: "halted by the harness",
		},
		{
			name: "reopened", reason: harness.DanglingToolCallInterrupted,
			want: "interrupted",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			sess := buildSession(t)
			appendText(t, sess, ai.UserMessage{}, "go", nil)

			_, err := sess.AppendMessage(ai.Assistant(
				ai.ToolCallPart{ID: "c1", Name: "add"},
			), nil)
			require.NoError(t, err)

			settled, err := sess.SettleDanglingToolCalls(testCase.reason)
			require.NoError(t, err)
			require.Equal(t, 1, settled)

			results, ok := sess.Entries()[2].Message.(ai.ToolMessage)
			require.True(t, ok)

			answer, ok := results.Parts[0].Content[0].(ai.TextPart)
			require.True(t, ok)
			assert.Contains(t, answer.Text, testCase.want)
			assert.Contains(t, answer.Text, "was not executed")
		})
	}
}

func TestSessionSettleDanglingToolCalls(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	appendText(t, sess, ai.UserMessage{}, "go", nil)

	_, err := sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "c1", Name: "first"},
		ai.ToolCallPart{ID: "c2", Name: "second"},
	), nil)
	require.NoError(t, err)

	settled, err := sess.SettleDanglingToolCalls(harness.DanglingToolCallHarnessHalted)
	require.NoError(t, err)
	assert.Equal(t, 2, settled)

	pending, err := sess.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)

	entries := sess.Entries()
	require.Len(t, entries, 3)

	results, ok := entries[2].Message.(ai.ToolMessage)
	require.True(t, ok)
	require.Len(t, results.Parts, 2, "results follow the original call order")

	for index, want := range []string{"c1", "c2"} {
		assert.Equal(t, want, results.Parts[index].ToolCallID)
		assert.True(t, results.Parts[index].IsError)
	}

	answer, ok := results.Parts[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, answer.Text, "halted by the harness")
	assert.Contains(t, answer.Text, `"first"`)

	again, err := sess.SettleDanglingToolCalls(harness.DanglingToolCallHarnessHalted)
	require.NoError(t, err)
	assert.Zero(t, again, "settling is idempotent")
}
