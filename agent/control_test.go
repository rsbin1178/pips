package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunMetadataDecoratesEventsContextAndResult(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("done")))

	var (
		events          []agent.Event
		contextMetadata []agent.RunMetadata
		prepared        agent.RunInfo
	)

	a, err := agent.New(model,
		agent.WithName("coordinator"),
		agent.WithOnEvent(func(ctx context.Context, ev agent.Event) {
			events = append(events, ev)

			meta, ok := agent.RunMetadataFromContext(ctx)
			require.True(t, ok)

			contextMetadata = append(contextMetadata, meta)
		}),
		agent.WithPrepareTurn(func(_ context.Context, info agent.RunInfo) agent.TurnUpdate {
			prepared = info
			return agent.TurnUpdate{}
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, "coordinator", a.Name())

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("go"))
	require.NoError(t, err)
	require.NotEmpty(t, result.RunID)
	assert.Empty(t, result.ParentRunID)
	assert.Equal(t, "coordinator", result.Agent)
	assert.Equal(t, result.RunMetadata, prepared.RunMetadata)
	require.NotEmpty(t, events)

	for idx, ev := range events {
		assert.Equal(t, result.RunID, ev.RunID)
		assert.Empty(t, ev.ParentRunID)
		assert.Equal(t, "coordinator", ev.Agent)
		assert.False(t, ev.Time.IsZero())
		assert.Equal(t, result.RunMetadata, contextMetadata[idx])

		if idx > 0 {
			assert.False(t, ev.Time.Before(events[idx-1].Time))
		}
	}
}

func TestNestedAgentRunLinksParentMetadata(t *testing.T) {
	t.Parallel()

	childModel := newScriptedModel(respond(textResponse("child answer")))

	var childStart agent.Event

	child, err := agent.New(childModel,
		agent.WithName("researcher"),
		agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
			if ev.Type == agent.EventRunStart {
				childStart = ev
			}
		}),
	)
	require.NoError(t, err)

	parentModel := newScriptedModel(
		respond(callResponse(call("c1", "delegate", `{"prompt":"investigate"}`))),
		respond(textResponse("combined answer")),
	)
	parent, err := agent.New(parentModel,
		agent.WithName("manager"),
		agent.WithTools(agent.AsTool(child, "delegate", "Delegate research.")),
	)
	require.NoError(t, err)

	result, err := parent.Run(t.Context(), agent.NewSession(), ai.UserText("solve"))
	require.NoError(t, err)
	require.NotEmpty(t, childStart.RunID)
	assert.NotEqual(t, result.RunID, childStart.RunID)
	assert.Equal(t, result.RunID, childStart.ParentRunID)
	assert.Equal(t, "researcher", childStart.Agent)
}

func TestInputGuardrailRejectsBeforeTranscriptMutation(t *testing.T) {
	t.Parallel()

	rejection := errors.New("unsafe input")
	model := newScriptedModel(respond(textResponse("unused")))
	order := make([]string, 0, 2)
	sess := agent.NewSession(ai.UserText("existing"))

	a, err := agent.New(model,
		agent.WithName("screened"),
		agent.WithInputGuardrail("first", func(_ context.Context, info agent.InputGuardrailInfo) error {
			order = append(order, "first")

			assert.Equal(t, "screened", info.Agent)
			assert.Len(t, info.Session, 1)
			assert.Len(t, info.Input, 1)

			return nil
		}),
		agent.WithInputGuardrail("safety", func(_ context.Context, _ agent.InputGuardrailInfo) error {
			order = append(order, "safety")
			return rejection
		}),
		agent.WithInputGuardrail("unreached", func(_ context.Context, _ agent.InputGuardrailInfo) error {
			order = append(order, "unreached")
			return nil
		}),
	)
	require.NoError(t, err)

	result, err := a.Run(t.Context(), sess, ai.UserText("new"))
	require.ErrorIs(t, err, agent.ErrGuardrail)
	require.ErrorIs(t, err, rejection)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.RunID)
	assert.Zero(t, result.Turns)
	assert.Equal(t, []string{"first", "safety"}, order)
	assert.Len(t, sess.Messages(), 1)
	assert.Empty(t, model.Requests())

	var guardErr *agent.GuardrailError
	require.ErrorAs(t, err, &guardErr)
	assert.Equal(t, agent.GuardrailInput, guardErr.Phase)
	assert.Equal(t, "safety", guardErr.Name)
}

func TestOutputGuardrailRejectsBeforeFinalMessageCommit(t *testing.T) {
	t.Parallel()

	rejection := errors.New("unsupported claim")
	model := newScriptedModel(respond(textResponse("provisional answer")))
	sess := agent.NewSession()

	var events []agent.EventType

	a, err := agent.New(model,
		agent.WithOnEvent(func(_ context.Context, ev agent.Event) {
			events = append(events, ev.Type)
		}),
		agent.WithOutputGuardrail("claims", func(_ context.Context, info agent.OutputGuardrailInfo) error {
			assert.Equal(t, "provisional answer", info.Response.Text())
			assert.Equal(t, 1, info.Turns)

			return rejection
		}),
	)
	require.NoError(t, err)

	result, err := a.Run(t.Context(), sess, ai.UserText("answer"))
	require.ErrorIs(t, err, agent.ErrGuardrail)
	require.ErrorIs(t, err, rejection)
	require.NotNil(t, result)
	assert.Equal(t, "provisional answer", result.Text())
	assert.Equal(t, 1, result.Turns)
	assert.Equal(t, ai.Usage{InputTokens: 10, OutputTokens: 5}, result.Usage)
	assert.Equal(t, result.Usage, sess.Usage())
	assert.Len(t, sess.Messages(), 1, "only the input is committed")
	assert.Equal(t, []agent.EventType{agent.EventRunStart, agent.EventTurnStart}, events)
}

func TestOutputGuardrailValidatesEachAnswerCandidate(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(textResponse("intermediate")),
		respond(textResponse("final")),
	)
	sess := agent.NewSession()
	sess.FollowUp(ai.UserText("continue"))

	var validated []string

	a, err := agent.New(model, agent.WithOutputGuardrail(
		"capture",
		func(_ context.Context, info agent.OutputGuardrailInfo) error {
			validated = append(validated, info.Response.Text())
			return nil
		},
	))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), sess, ai.UserText("start"))
	require.NoError(t, err)
	assert.Equal(t, "final", result.Text())
	assert.Equal(t, []string{"intermediate", "final"}, validated)
}

func TestStreamOutputGuardrailLeavesDeltasProvisional(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(respond(textResponse("visible before validation")))
	sess := agent.NewSession()
	a, err := agent.New(model, agent.WithOutputGuardrail(
		"reject",
		func(context.Context, agent.OutputGuardrailInfo) error {
			return errors.New("reject stream")
		},
	))
	require.NoError(t, err)

	var (
		sawTextDelta bool
		streamErr    error
	)

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("go")) {
		if err != nil {
			streamErr = err
			continue
		}

		if ev.Type == agent.EventDelta && ev.Delta.Type == ai.StreamTextDelta {
			sawTextDelta = true
		}
	}

	require.ErrorIs(t, streamErr, agent.ErrGuardrail)
	assert.True(t, sawTextDelta)
	assert.Len(t, sess.Messages(), 1)
}

func TestAgentRejectsInvalidGuardrailConfiguration(t *testing.T) {
	t.Parallel()

	model := newScriptedModel()

	_, err := agent.New(model, agent.WithInputGuardrail("", func(context.Context, agent.InputGuardrailInfo) error {
		return nil
	}))
	require.ErrorContains(t, err, "input guardrail")

	_, err = agent.New(model, agent.WithOutputGuardrail("named", nil))
	require.ErrorContains(t, err, "output guardrail")
}

func TestAsToolReportsPausedChild(t *testing.T) {
	t.Parallel()

	childModel := newScriptedModel(
		respond(callResponse(call("approval", "sensitive", `{}`))),
	)
	child, err := agent.New(childModel, agent.WithBeforeTool(
		func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
			return agent.ToolDecision{Action: agent.ToolDecisionPause}
		},
	))
	require.NoError(t, err)

	tool := agent.AsTool(child, "delegate", "Delegate work.")
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "outer", Name: "delegate", Args: ai.JSON(`{"prompt":"go"}`),
	})
	require.ErrorIs(t, err, agent.ErrSubagentPaused)
}
