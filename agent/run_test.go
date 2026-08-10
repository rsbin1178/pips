package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addTool is the standard test tool: adds two integers.
func addTool() agent.Tool {
	return agent.NewTool("add", "Add two integers.",
		func(_ context.Context, args struct {
			A int `json:"a"`
			B int `json:"b"`
		},
		) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})
}

func sessionText(messages []ai.Message) string {
	var value strings.Builder

	for _, message := range messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}

		for _, part := range parts {
			if text, ok := part.(ai.TextPart); ok {
				value.WriteString(text.Text)
			}
		}
	}

	return value.String()
}

func TestRunToolLoop(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":2,"b":3}`))),
		respond(textResponse("The answer is 5.")),
	)

	a, err := agent.New(model, agent.WithTools(addTool()), agent.WithSystem("Be terse."))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("What is 2+3?"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)
	assert.Equal(t, "The answer is 5.", result.Text())
	assert.Equal(t, ai.Usage{InputTokens: 20, OutputTokens: 10}, result.Usage)
	assert.Equal(t, result.Usage, sess.Usage())

	// Session: user, assistant(call), tool(result), assistant(text).
	msgs := sess.Messages()
	require.Len(t, msgs, 4)
	assert.IsType(t, ai.UserMessage{}, msgs[0])
	assert.IsType(t, ai.AssistantMessage{}, msgs[1])
	assert.IsType(t, ai.ToolMessage{}, msgs[2])
	assert.IsType(t, ai.AssistantMessage{}, msgs[3])

	toolMessage, ok := msgs[2].(ai.ToolMessage)
	require.True(t, ok)

	result5 := toolMessage.Parts[0]
	assert.Equal(t, "c1", result5.ToolCallID)
	assert.False(t, result5.IsError)
	assert.Equal(t, []ai.Part{ai.Text("5")}, result5.Content)

	// Requests carried the system prompt and the tool declaration; the
	// second request included the tool result.
	reqs := model.Requests()
	require.Len(t, reqs, 2)
	system, firstConversation, err := reqs[0].Messages.SplitSystem()
	require.NoError(t, err)
	assert.Equal(t, "Be terse.", ai.JoinSystemText(system))
	require.Len(t, firstConversation, 1)
	require.Len(t, reqs[0].Tools, 1)
	assert.Equal(t, "add", reqs[0].Tools[0].Name)
	_, secondConversation, err := reqs[1].Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, secondConversation, 3)
	assert.IsType(t, ai.ToolMessage{}, secondConversation[2])
}

func TestStreamToolLoop(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":2,"b":3}`))),
		respond(textResponse("5")),
	)

	a, err := agent.New(model, agent.WithTools(addTool()))
	require.NoError(t, err)

	sess := agent.NewSession()

	var types []agent.EventType

	deltas := 0

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("2+3?")) {
		require.NoError(t, err)
		require.NoError(t, ev.Validate())

		if _, ok := ev.Payload().(agent.ModelStreamEvent); ok {
			deltas++
			continue
		}

		types = append(types, ev.Type())

		if completed, ok := ev.Payload().(agent.RunCompleted); ok {
			assert.Equal(t, agent.StopEndTurn, completed.Stop)
			assert.Equal(t, ai.Usage{InputTokens: 20, OutputTokens: 10}, completed.Usage)
		}
	}

	assert.Equal(t, []agent.EventType{
		agent.EventRunStarted,
		agent.EventTurnStarted,
		agent.EventMessageCommitted, // assistant with tool call
		agent.EventToolStarted,
		agent.EventToolCompleted,
		agent.EventMessageCommitted, // tool results
		agent.EventTurnCompleted,
		agent.EventTurnStarted,
		agent.EventMessageCommitted, // final text
		agent.EventTurnCompleted,
		agent.EventRunCompleted,
	}, types)
	assert.Positive(t, deltas)

	// Stream and Run build identical sessions.
	msgs := sess.Messages()
	require.Len(t, msgs, 4)

	finalMessage, ok := msgs[3].(ai.AssistantMessage)
	require.True(t, ok)
	final, ok := finalMessage.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "5", final.Text)
}

func TestCandidateAnswerRetryDiscardsDraftAndConstrainsOneRequest(t *testing.T) {
	t.Parallel()

	tool := addTool()
	model := newScriptedModel(
		respond(textResponse("uncommitted draft")),
		respond(callResponse(call("c1", "add", `{"a":2,"b":3}`))),
		respond(textResponse("5")),
	)

	candidates := 0
	a, err := agent.New(
		model,
		agent.WithSystem("base system"),
		agent.WithTools(tool),
		agent.WithCandidateAnswer(func(
			_ context.Context,
			_ agent.CandidateAnswerInfo,
		) agent.CandidateAnswerDecision {
			candidates++
			if candidates != 1 {
				return agent.CandidateAnswerDecision{}
			}

			return agent.CandidateAnswerDecision{Retry: &agent.ModelRequestUpdate{
				Tools:        []agent.Tool{tool},
				ToolChoice:   ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: "add"},
				SystemSuffix: "retry with the exact tool",
			}}
		}),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	var events []agent.EventType

	for event, streamErr := range a.Stream(t.Context(), sess, ai.UserText("calculate")) {
		require.NoError(t, streamErr)
		require.NoError(t, event.Validate())

		events = append(events, event.Type())
	}

	assert.Contains(t, events, agent.EventCandidateDiscarded)
	assert.Equal(t, 2, candidates)
	assert.NotContains(t, sessionText(sess.Messages()), "uncommitted draft")
	assert.Contains(t, sessionText(sess.Messages()), "5")

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: "add"}, requests[1].ToolChoice)
	require.Len(t, requests[1].Tools, 1)
	assert.Equal(t, "add", requests[1].Tools[0].Name)
	system, _, err := requests[1].Messages.SplitSystem()
	require.NoError(t, err)
	assert.Contains(t, ai.JoinSystemText(system), "base system\n\nretry with the exact tool")
	assert.Equal(t, ai.ToolChoice{}, requests[2].ToolChoice, "constraint must be one-shot")
}

func TestRunOnEvent(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("hi")))

	var types []agent.EventType

	a, err := agent.New(model, agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
		types = append(types, ev.Type())
	}))
	require.NoError(t, err)

	_, err = a.Run(t.Context(), agent.NewSession(), ai.UserText("hello"))
	require.NoError(t, err)

	assert.Equal(t, []agent.EventType{
		agent.EventRunStarted,
		agent.EventTurnStarted,
		agent.EventMessageCommitted,
		agent.EventTurnCompleted,
		agent.EventRunCompleted,
	}, types)
}

func TestStopMaxTurns(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))),
		respond(callResponse(call("c2", "add", `{"a":2,"b":2}`))),
		respond(callResponse(call("c3", "add", `{"a":3,"b":3}`))),
	)

	a, err := agent.New(model, agent.WithTools(addTool()), agent.WithMaxTurns(2))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("loop"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopMaxTurns, result.Stop)
	assert.Equal(t, 2, result.Turns)
	// Every issued call was answered; the session can continue later.
	assert.Empty(t, sess.Pending())
}

func TestStopBudget(t *testing.T) {
	t.Parallel()

	// Each scripted response costs 15 tokens; the budget allows one turn.
	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))),
		respond(callResponse(call("c2", "add", `{"a":2,"b":2}`))),
	)

	a, err := agent.New(model, agent.WithTools(addTool()), agent.WithMaxTokens(15))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopBudget, result.Stop)
	assert.Equal(t, 1, result.Turns)
}

func TestStopWhen(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))),
		respond(callResponse(call("c2", "add", `{"a":2,"b":2}`))),
	)

	a, err := agent.New(model,
		agent.WithTools(addTool()),
		agent.WithStopWhen(func(info agent.RunInfo) bool { return info.Turns >= 1 }),
	)
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("go"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopWhen, result.Stop)
	assert.Equal(t, 1, result.Turns)
}

func TestRunModelErrorFirstTurn(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(failWith(errors.New("boom")))

	a, err := agent.New(model)
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("hi"))
	require.EqualError(t, err, "boom")
	require.NotNil(t, result)
	assert.Zero(t, result.Turns)
	assert.Empty(t, result.Stop)
	assert.Nil(t, result.Response)
}

func TestRunModelErrorMidRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(callResponse(call("c1", "add", `{"a":1,"b":1}`))),
		failWith(errors.New("boom")),
	)

	a, err := agent.New(model, agent.WithTools(addTool()))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("hi"))
	require.EqualError(t, err, "boom")
	assert.Equal(t, 1, result.Turns)
	require.NotNil(t, result.Response)

	// The failed turn left a protocol-complete session: the run can retry.
	assert.Empty(t, sess.Pending())
	assert.Len(t, sess.Messages(), 3) // user, assistant(call), tool(result)
}

func TestRunRequestEscapeHatch(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("ok")))

	a, err := agent.New(model, agent.WithRequest(func(req *ai.Request) {
		req.Temperature = ai.Ptr(0.2)
	}))
	require.NoError(t, err)

	_, err = a.Run(t.Context(), agent.NewSession(), ai.UserText("hi"))
	require.NoError(t, err)

	reqs := model.Requests()
	require.Len(t, reqs, 1)
	require.NotNil(t, reqs[0].Temperature)
	assert.InEpsilon(t, 0.2, *reqs[0].Temperature, 1e-9)
}

func TestStreamBreakCancelsRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("a long answer")))

	a, err := agent.New(model)
	require.NoError(t, err)

	sess := agent.NewSession()

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("hi")) {
		require.NoError(t, err)

		if _, ok := ev.Payload().(agent.ModelStreamEvent); ok {
			break // abandon mid-model-stream
		}
	}

	assert.True(t, model.abandoned.Load(), "provider stream should observe the break")
	// The model call never completed, so only the user message landed.
	require.Len(t, sess.Messages(), 1)
	assert.Empty(t, sess.Pending())

	// The session is reusable after an abandoned stream.
	model2 := newScriptedModel(respond(textResponse("done")))
	a2, err := agent.New(model2)
	require.NoError(t, err)

	result, err := a2.Run(t.Context(), sess)
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
}

func TestRunCancelDuringTool(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	blocking := agent.NewTool("block", "Blocks until cancelled.",
		func(ctx context.Context, _ struct{}) (string, error) {
			cancel() // simulate the caller cancelling mid-execution

			<-ctx.Done()

			return "", ctx.Err()
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "block", `{}`), call("c2", "block", `{}`))),
	)

	a, err := agent.New(model, agent.WithTools(blocking))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(ctx, sess, ai.UserText("hi"))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, result.Turns)

	// Both calls were answered (first with the tool's ctx error, second
	// synthesized), keeping the session protocol-complete.
	assert.Empty(t, sess.Pending())

	msgs := sess.Messages()
	require.Len(t, msgs, 3)

	toolResults, ok := msgs[2].(ai.ToolMessage)
	require.True(t, ok)

	for _, toolResult := range toolResults.Parts {
		assert.True(t, toolResult.IsError)
	}
}

func TestRunActiveConflict(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	blocking := agent.NewTool("block", "Blocks until released.",
		func(_ context.Context, _ struct{}) (string, error) {
			close(started)
			<-release

			return "done", nil
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "block", `{}`))),
		respond(textResponse("ok")),
	)

	a, err := agent.New(model, agent.WithTools(blocking))
	require.NoError(t, err)

	sess := agent.NewSession()
	done := make(chan error, 1)

	go func() {
		_, runErr := a.Run(context.Background(), sess, ai.UserText("hi"))
		done <- runErr
	}()

	<-started

	_, err = a.Run(t.Context(), sess, ai.UserText("again"))
	require.ErrorIs(t, err, agent.ErrRunActive)
	require.ErrorIs(t, sess.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: "c1", Content: agent.TextResult("out-of-band"),
	}), agent.ErrRunActive)

	close(release)
	require.NoError(t, <-done)
}

func TestSessionJSONRoundTripContinues(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(callResponse(call("c1", "add", `{"a":2,"b":3}`))))

	a, err := agent.New(model, agent.WithTools(addTool()), agent.WithMaxTurns(1))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("2+3?"))
	require.NoError(t, err)
	require.Equal(t, agent.StopMaxTurns, result.Stop)

	// Persist and restore the session, then continue with a fresh agent.
	blob, err := json.Marshal(sess)
	require.NoError(t, err)

	restored := agent.NewSession()
	require.NoError(t, json.Unmarshal(blob, restored))
	assert.Equal(t, sess.Messages(), restored.Messages())
	assert.Equal(t, sess.Usage(), restored.Usage())

	model2 := newScriptedModel(respond(textResponse("The answer is 5.")))
	a2, err := agent.New(model2, agent.WithTools(addTool()))
	require.NoError(t, err)

	final, err := a2.Run(t.Context(), restored)
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, final.Stop)
	assert.Equal(t, "The answer is 5.", final.Text())

	// The continued request replayed the restored history.
	reqs := model2.Requests()
	require.Len(t, reqs, 1)
	assert.Len(t, reqs[0].Messages, 3)
}

func TestStreamModelErrorYieldsError(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(failWith(errors.New("boom")))

	a, err := agent.New(model)
	require.NoError(t, err)

	var lastErr error

	for _, err := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("hi")) {
		if err != nil {
			lastErr = err
		}
	}

	require.EqualError(t, lastErr, "boom")
}

func TestRunNoToolsSingleTurn(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("hello")))

	a, err := agent.New(model)
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("hi"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 1, result.Turns)
	assert.Equal(t, "hello", result.Text())
}

func TestToolTimeout(t *testing.T) {
	t.Parallel()

	slow := agent.NewTool("slow", "Sleeps.",
		func(ctx context.Context, _ struct{}) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "done", nil
			}
		})

	model := newScriptedModel(
		respond(callResponse(call("c1", "slow", `{}`))),
		respond(textResponse("gave up")),
	)

	a, err := agent.New(model,
		agent.WithTools(slow),
		agent.WithToolTimeout(10*time.Millisecond),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("hi"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)

	toolMessage, ok := sess.Messages()[2].(ai.ToolMessage)
	require.True(t, ok)

	toolResult := toolMessage.Parts[0]
	assert.True(t, toolResult.IsError)
	assert.Contains(t, resultText(t, toolResult), "deadline")
}
