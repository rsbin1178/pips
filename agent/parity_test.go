package agent_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSteeringInjectsBeforeNextTurn(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "steerer", `{}`))),
		respond(textResponse("adjusted")),
	)

	sess := agent.NewSession()

	// The tool steers mid-run, as a user typing while the agent works.
	steerer := agent.NewTool("steerer", "Steers.",
		func(_ context.Context, _ struct{}) (string, error) {
			sess.Steer(ai.UserText("actually, do it in French"))
			return "ok", nil
		})

	a, err := agent.New(model, agent.WithTools(steerer))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)

	// The steered message reached the second model call, after the tool
	// results: user, assistant, tool, steered-user.
	reqs := model.Requests()
	require.Len(t, reqs, 2)
	require.Len(t, reqs[1].Messages, 4)
	assert.Equal(t, ai.RoleUser, reqs[1].Messages[3].Role)

	// And it landed in the session in the same position.
	msgs := sess.Messages()
	require.Len(t, msgs, 5)
	assert.Equal(t, ai.RoleUser, msgs[3].Role)
}

func TestSteeringExtendsFinishedRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(textResponse("first answer")),
		respond(textResponse("second answer")),
	)

	sess := agent.NewSession()

	var steered atomic.Bool

	// Steer exactly once, while the first turn's message lands.
	a, err := agent.New(model, agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
		if ev.Type == agent.EventMessage && !steered.Swap(true) {
			sess.Steer(ai.UserText("one more thing"))
		}
	}))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), sess, ai.UserText("hi"))
	require.NoError(t, err)

	// The model finished, but queued steering extended the loop.
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)
	assert.Equal(t, "second answer", result.Text())
}

func TestSteeringDrainModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mode          agent.QueueMode
		firstTurnMsgs int // messages in the first model request
		turns         int
	}{
		// QueueDrainOne injects the oldest message per turn: 1 steered message on
		// turn one, the second extends the run.
		{name: "one at a time", mode: agent.QueueDrainOne, firstTurnMsgs: 2, turns: 2},
		// QueueDrainAll injects both up front.
		{name: "all", mode: agent.QueueDrainAll, firstTurnMsgs: 3, turns: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := newScriptedModel(
				respond(textResponse("a")),
				respond(textResponse("b")),
			)

			a, err := agent.New(model, agent.WithSteeringMode(tt.mode))
			require.NoError(t, err)

			sess := agent.NewSession()
			sess.Steer(ai.UserText("first"), ai.UserText("second"))

			result, err := a.Run(t.Context(), sess, ai.UserText("go")) // + queued
			require.NoError(t, err)

			assert.Equal(t, tt.turns, result.Turns)
			assert.Len(t, model.Requests()[0].Messages, tt.firstTurnMsgs)
			assert.False(t, sess.HasQueued())
		})
	}
}

func TestFollowUpRunsAfterFinish(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(textResponse("done with task one")),
		respond(textResponse("done with task two")),
	)

	a, err := agent.New(model)
	require.NoError(t, err)

	sess := agent.NewSession()
	sess.FollowUp(ai.UserText("now task two"))

	result, err := a.Run(t.Context(), sess, ai.UserText("task one"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)
	assert.Equal(t, "done with task two", result.Text())

	// The follow-up was injected only after the first natural finish.
	reqs := model.Requests()
	require.Len(t, reqs, 2)
	assert.Len(t, reqs[0].Messages, 1)
	assert.Len(t, reqs[1].Messages, 3)
}

func TestQueuesSurvivePause(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))))

	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithBeforeTool(func(_ context.Context, _ agent.ToolCallInfo) agent.ToolDecision {
			return agent.ToolDecision{Action: agent.ToolDecisionPause}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()
	sess.FollowUp(ai.UserText("later"))

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopPaused, result.Stop)
	assert.True(t, sess.HasQueued(), "pause must not consume queued messages")
}

func TestTransformContext(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("ok")))

	// Keep only the last message — a stand-in for compaction.
	a, err := agent.New(model, agent.WithTransformContext(
		func(_ context.Context, msgs []ai.Message) ([]ai.Message, error) {
			return msgs[len(msgs)-1:], nil
		}))
	require.NoError(t, err)

	sess := agent.NewSession(ai.UserText("old"), ai.AssistantText("older"))

	_, err = a.Run(t.Context(), sess, ai.UserText("latest"))
	require.NoError(t, err)

	// The model saw the pruned view; the session kept full history.
	assert.Len(t, model.Requests()[0].Messages, 1)
	assert.Len(t, sess.Messages(), 4)
}

func TestTransformContextErrorFailsRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("unreached")))

	a, err := agent.New(model, agent.WithTransformContext(
		func(_ context.Context, _ []ai.Message) ([]ai.Message, error) {
			return nil, errors.New("compaction failed")
		}))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("hi"))
	require.EqualError(t, err, "compaction failed")
	assert.Zero(t, result.Turns)
}

func TestAfterToolOverride(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":2,"b":2}`))),
		respond(textResponse("done")),
	)

	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithAfterTool(func(_ context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
			assert.Equal(t, "add", info.Name)
			assert.Equal(t, "4", resultText(t, info.Result))

			isErr := true

			return &agent.ToolResultOverride{
				Content: agent.TextResult("redacted"),
				IsError: &isErr,
			}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	_, err = a.Run(t.Context(), sess, ai.UserText("2+2"))
	require.NoError(t, err)

	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.True(t, results[0].IsError)
	assert.Equal(t, "redacted", resultText(t, results[0]))
}

func TestAfterToolSkipsUnexecutedAndRecoversPanic(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(
			call("c1", "ghost", `{}`),              // unknown: hook must not fire
			call("c2", "add", `{"a":1,"b":1}`),     // denied: hook must not fire
			call("c3", "panicky", `{"a":1,"b":1}`), // executed: hook panics
		)),
		respond(textResponse("done")),
	)

	panicky := agent.NewTool("panicky", "Fine tool, hostile hook.",
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })

	var hooked atomic.Int32

	a, err := agent.New(model,
		agent.WithTools(addTool(), panicky),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			if info.ID == "c2" {
				return agent.DenyTool("no")
			}

			return agent.ToolDecision{}
		}),
		agent.WithAfterTool(func(_ context.Context, _ agent.ToolResultInfo) *agent.ToolResultOverride {
			hooked.Add(1)
			panic("hook bug")
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	_, err = a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, int32(1), hooked.Load(), "hook fires only for the executed call")

	results := toolResults(t, sess, 2)
	require.Len(t, results, 3)
	assert.Contains(t, resultText(t, results[2]), "after-tool hook panicked")
	assert.True(t, results[2].IsError)
}

func TestTerminateStopsRun(t *testing.T) {
	t.Parallel()

	final := agent.NewTool("final_answer", "Submit the final answer.",
		func(_ context.Context, args struct {
			Answer string `json:"answer"`
		},
		) (string, error) {
			return args.Answer, agent.ErrTerminate
		})

	model := newScriptedModel(respond(callResponse(call("c1", "final_answer", `{"answer":"42"}`))))

	a, err := agent.New(model, agent.WithTools(final))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("meaning of life?"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopTerminated, result.Stop)
	assert.Equal(t, 1, result.Turns)

	// The terminating result is a success, not an error.
	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.False(t, results[0].IsError)
	assert.Equal(t, "42", resultText(t, results[0]))
}

func TestTerminateRequiresWholeBatch(t *testing.T) {
	t.Parallel()

	final := agent.NewTool("final", "Terminates.",
		func(_ context.Context, _ struct{}) (string, error) {
			return "stop", agent.ErrTerminate
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "final", `{}`), call("c2", "add", `{"a":1,"b":1}`))),
		respond(textResponse("kept going")),
	)

	a, err := agent.New(model, agent.WithTools(final, addTool()))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("go"))
	require.NoError(t, err)

	// One non-terminating result in the batch keeps the loop alive.
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)
}

func TestTerminateStillDrainsFollowUps(t *testing.T) {
	t.Parallel()

	final := agent.NewTool("final", "Terminates.",
		func(_ context.Context, _ struct{}) (string, error) {
			return "stop", agent.ErrTerminate
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "final", `{}`))),
		respond(textResponse("follow-up handled")),
	)

	a, err := agent.New(model, agent.WithTools(final))
	require.NoError(t, err)

	sess := agent.NewSession()
	sess.FollowUp(ai.UserText("one more"))

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)
	assert.Equal(t, "follow-up handled", result.Text())
}

func TestPrepareTurnSwapsModelAndCompacts(t *testing.T) {
	t.Parallel()

	first := newScriptedModel(respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))))
	second := newScriptedModel(respond(textResponse("from the second model")))

	a, err := agent.New(first,
		agent.WithTools(addTool()),
		agent.WithPrepareTurn(func(_ context.Context, info agent.RunInfo) agent.TurnUpdate {
			return agent.TurnUpdate{
				Model: second,
				// Commit a compaction: summary replaces everything except
				// the protocol-required tail.
				ReplaceMessages: append(
					[]ai.Message{ai.UserText("summary of earlier work")},
					info.Response.Message,
					ai.ToolResultText("c1", "add", "2"),
				),
			}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, "from the second model", result.Text())
	require.Len(t, first.Requests(), 1, "first model serves only turn one")
	require.Len(t, second.Requests(), 1, "second model serves turn two")

	// Turn two ran against the rewritten history.
	assert.Len(t, second.Requests()[0].Messages, 3)
}

func TestTruncatedToolCallsAreNotExecuted(t *testing.T) {
	t.Parallel()

	var executed atomic.Int32

	counter := agent.NewTool("count", "Counts executions.",
		func(_ context.Context, _ struct{}) (string, error) {
			executed.Add(1)
			return "ran", nil
		})

	truncated := callResponse(call("c1", "count", `{}`), call("c2", "count", `{}`))
	truncated.FinishReason = ai.FinishLength

	model := newScriptedModel(
		respond(truncated),
		respond(textResponse("reissued")),
	)

	a, err := agent.New(model, agent.WithTools(counter))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("go"))
	require.NoError(t, err)

	assert.Zero(t, executed.Load(), "truncated calls must not execute")
	assert.Equal(t, agent.StopEndTurn, result.Stop)

	results := toolResults(t, sess, 2)
	require.Len(t, results, 2)

	for _, r := range results {
		assert.True(t, r.IsError)
		assert.Contains(t, resultText(t, r), "output token limit")
	}
}

func TestReportProgressEvents(t *testing.T) {
	t.Parallel()

	slowly := agent.Parallel(agent.NewTool("slowly", "Reports progress.",
		func(ctx context.Context, _ struct{}) (string, error) {
			agent.ReportProgress(ctx, ai.Text("halfway"))
			agent.ReportProgress(ctx, ai.Text("almost"))

			return "done", nil
		}))

	model := newScriptedModel(
		respond(callResponse(call("c1", "slowly", `{}`), call("c2", "slowly", `{}`))),
		respond(textResponse("ok")),
	)

	a, err := agent.New(model, agent.WithTools(slowly))
	require.NoError(t, err)

	updates := 0

	for ev, err := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("go")) {
		require.NoError(t, err)

		if ev.Type == agent.EventToolUpdate {
			updates++

			require.NotNil(t, ev.Call)
			assert.Equal(t, "slowly", ev.Call.Name)
			require.Len(t, ev.Update, 1)
		}
	}

	assert.Equal(t, 4, updates)
}

func TestReportProgressOutsideRunIsNoOp(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		agent.ReportProgress(t.Context(), ai.Text("nobody listening"))
	})
}

func TestSteeringCannotOutrunMaxTurns(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(textResponse("one")),
		respond(textResponse("two")),
	)

	a, err := agent.New(model, agent.WithMaxTurns(1))
	require.NoError(t, err)

	sess := agent.NewSession()

	var steered atomic.Bool

	// Try to keep the loop alive forever via steering.
	a2, err := agent.New(model, agent.WithMaxTurns(1), agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
		if ev.Type == agent.EventMessage && !steered.Swap(true) {
			sess.Steer(ai.UserText("keep going"))
		}
	}))
	require.NoError(t, err)

	_ = a // first agent unused beyond validation

	result, err := a2.Run(t.Context(), sess, ai.UserText("hi"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopMaxTurns, result.Stop)
	assert.Equal(t, 1, result.Turns)
	assert.True(t, sess.HasQueued(), "undrained steering stays queued")
}

func TestAsToolSubagent(t *testing.T) {
	t.Parallel()

	inner := newScriptedModel(respond(textResponse("Paris")))

	researcher, err := agent.New(inner, agent.WithSystem("Answer in one word."))
	require.NoError(t, err)

	outer := newScriptedModel(
		respond(callResponse(call("c1", "research", `{"prompt":"capital of France?"}`))),
		respond(textResponse("The capital is Paris.")),
	)

	main, err := agent.New(outer, agent.WithTools(agent.AsTool(researcher, "research", "Delegate a question.")))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := main.Run(t.Context(), sess, ai.UserText("What is the capital of France?"))
	require.NoError(t, err)
	assert.Equal(t, "The capital is Paris.", result.Text())

	// The sub-agent ran with its own prompt and returned its text as the
	// tool result.
	require.Len(t, inner.Requests(), 1)
	assert.Equal(t, "Answer in one word.", inner.Requests()[0].System)

	results := toolResults(t, sess, 2)
	require.Len(t, results, 1)
	assert.Equal(t, "Paris", resultText(t, results[0]))
}

func TestSessionQueueAPI(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession()
	assert.False(t, sess.HasQueued())

	sess.Steer(ai.UserText("a"))
	sess.FollowUp(ai.UserText("b"))
	assert.True(t, sess.HasQueued())

	sess.ClearSteering()
	assert.True(t, sess.HasQueued())

	sess.ClearFollowUps()
	assert.False(t, sess.HasQueued())
}

func TestSessionReplace(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession(ai.UserText("a"), ai.AssistantText("b"), ai.UserText("c"))
	sess.Replace(ai.UserText("summary"))

	msgs := sess.Messages()
	require.Len(t, msgs, 1)
}
