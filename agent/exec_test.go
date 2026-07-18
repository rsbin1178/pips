package agent_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolResults extracts the tool results of the session's single tool message
// at index i.
func toolResults(t *testing.T, sess *agent.Session, i int) []ai.ToolResultPart {
	t.Helper()

	msg := sess.Messages()[i]
	require.Equal(t, ai.RoleTool, msg.Role)

	results := make([]ai.ToolResultPart, 0, len(msg.Parts))

	for _, part := range msg.Parts {
		result, ok := part.(ai.ToolResultPart)
		require.True(t, ok)

		results = append(results, result)
	}

	return results
}

func resultText(t *testing.T, r ai.ToolResultPart) string {
	t.Helper()

	require.NotEmpty(t, r.Content)

	text, ok := r.Content[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text
}

func TestToolFailuresBecomeErrorResults(t *testing.T) {
	t.Parallel()

	failing := agent.NewTool("failing", "Always errors.",
		func(_ context.Context, _ struct{}) (string, error) {
			return "", errors.New("disk on fire")
		})
	panicking := agent.NewTool("panicking", "Always panics.",
		func(_ context.Context, _ struct{}) (string, error) {
			panic("boom")
		})

	model := newScriptedModel(
		respond(callResponse(
			call("c1", "failing", `{}`),
			call("c2", "panicking", `{}`),
			call("c3", "ghost", `{}`),  // not registered
			call("c4", "add", `not{j`), // undecodable arguments
		)),
		respond(textResponse("noted")),
	)

	a, err := agent.New(model, agent.WithTools(failing, panicking, addTool()))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("try things"))
	require.NoError(t, err, "tool failures must not abort the run")
	assert.Equal(t, agent.StopEndTurn, result.Stop)

	results := toolResults(t, sess, 2)
	require.Len(t, results, 4)

	for i, r := range results {
		assert.True(t, r.IsError, "result %d should be an error", i)
	}

	assert.Equal(t, "disk on fire", resultText(t, results[0]))
	assert.Contains(t, resultText(t, results[1]), "tool panicked: boom")
	assert.Contains(t, resultText(t, results[2]), "unknown tool")
	assert.Contains(t, resultText(t, results[3]), "invalid arguments")
}

func TestParallelToolsRunConcurrently(t *testing.T) {
	t.Parallel()

	// Every call blocks on a shared barrier that only opens when all three
	// are inside Exec — the test deadlocks (and times out) if the runtime
	// serializes them.
	var barrier sync.WaitGroup

	barrier.Add(3)

	echo := agent.Parallel(agent.NewTool("echo", "Echoes n.",
		func(_ context.Context, args struct {
			N int `json:"n"`
		},
		) (string, error) {
			barrier.Done()
			barrier.Wait()

			return strconv.Itoa(args.N), nil
		}))

	model := newScriptedModel(
		respond(callResponse(
			call("c1", "echo", `{"n":1}`),
			call("c2", "echo", `{"n":2}`),
			call("c3", "echo", `{"n":3}`),
		)),
		respond(textResponse("done")),
	)

	a, err := agent.New(model, agent.WithTools(echo))
	require.NoError(t, err)

	sess := agent.NewSession()

	_, err = a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	// Results land in call order regardless of completion order.
	results := toolResults(t, sess, 2)
	require.Len(t, results, 3)

	for i, want := range []string{"1", "2", "3"} {
		assert.Equal(t, want, resultText(t, results[i]))
		assert.Equal(t, "c"+strconv.Itoa(i+1), results[i].ToolCallID)
	}
}

func TestSerialToolBarriers(t *testing.T) {
	t.Parallel()

	var active, serialSawConcurrent atomic.Int32

	concurrent := agent.Parallel(agent.NewTool("concurrent", "Tracks concurrency.",
		func(_ context.Context, _ struct{}) (string, error) {
			active.Add(1)
			defer active.Add(-1)

			return "ok", nil
		}))

	serial := agent.NewTool("serial", "Must run alone.",
		func(_ context.Context, _ struct{}) (string, error) {
			if active.Load() != 0 {
				serialSawConcurrent.Store(1)
			}

			return "ok", nil
		})

	model := newScriptedModel(
		respond(callResponse(
			call("c1", "concurrent", `{}`),
			call("c2", "concurrent", `{}`),
			call("c3", "serial", `{}`),
			call("c4", "concurrent", `{}`),
		)),
		respond(textResponse("done")),
	)

	a, err := agent.New(model, agent.WithTools(concurrent, serial))
	require.NoError(t, err)

	sess := agent.NewSession()

	_, err = a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Zero(t, serialSawConcurrent.Load(), "serial tool must not overlap concurrent tools")
	require.Len(t, toolResults(t, sess, 2), 4)
}

func TestGateDenyFeedsReasonToModel(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,"b":2}`))),
		respond(textResponse("understood")),
	)

	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.Decision {
			assert.Equal(t, "add", info.Name)
			assert.Equal(t, 1, info.Turn)

			return agent.Denied("arithmetic is forbidden today")
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("1+2?"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)

	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.True(t, results[0].IsError)
	assert.Equal(t, "arithmetic is forbidden today", resultText(t, results[0]))

	// The model saw the denial on its next call.
	reqs := model.Requests()
	require.Len(t, reqs, 2)
}

func TestGatePauseAndResume(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(
			call("c1", "add", `{"a":1,"b":1}`),
			call("c2", "add", `{"a":2,"b":2}`),
			call("c3", "add", `{"a":3,"b":3}`),
		)),
		respond(textResponse("resumed")),
	)

	// Pause on the second call: the first executes, the rest go pending.
	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.Decision {
			if info.ID == "c2" {
				return agent.Decision{Action: agent.Pause}
			}

			return agent.Decision{}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopPaused, result.Stop)
	require.Len(t, result.Pending, 2)
	assert.Equal(t, "c2", result.Pending[0].ID)
	assert.Equal(t, "c3", result.Pending[1].ID)
	assert.Equal(t, result.Pending, sess.Pending())

	// The first call's result landed before the pause.
	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.Equal(t, "2", resultText(t, results[0]))

	// Running with unresolved calls is refused.
	_, err = a.Run(t.Context(), sess)
	require.ErrorIs(t, err, agent.ErrPendingToolCalls)

	// Approve c2, reject c3.
	err = sess.ResolvePending(t.Context(), func(_ context.Context, pending ai.ToolCallPart) ([]ai.Part, error) {
		if pending.ID == "c2" {
			return agent.TextResult("4"), nil
		}

		return nil, errors.New("denied by operator")
	})
	require.NoError(t, err)
	assert.Empty(t, sess.Pending())

	final, err := a.Run(t.Context(), sess)
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, final.Stop)
	assert.Equal(t, "resumed", final.Text())

	// The resolution message answered both calls, error flag intact.
	resolved := toolResults(t, sess, 3)
	require.Len(t, resolved, 2)
	assert.Equal(t, "4", resultText(t, resolved[0]))
	assert.False(t, resolved[0].IsError)
	assert.Equal(t, "denied by operator", resultText(t, resolved[1]))
	assert.True(t, resolved[1].IsError)
}

func TestGatePauseFirstCallLeavesNoToolMessage(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))))

	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithBeforeTool(func(_ context.Context, _ agent.ToolCallInfo) agent.Decision {
			return agent.Decision{Action: agent.Pause}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopPaused, result.Stop)
	require.Len(t, result.Pending, 1)

	// No results executed, so no tool message: user + assistant only.
	msgs := sess.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, result.Pending, sess.Pending())
}
