package coding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeQuestionPauseResolveAndReject(t *testing.T) {
	t.Parallel()

	t.Run("resolve", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel(
			runtimeQuestionResponse(t, "call-question"),
			runtimeTextResponse("continued with the answer"),
		)
		runtime := openTestRuntime(t, model)

		promptEvents := collectRuntimeEvents(
			t,
			runtime.Prompt(t.Context(), ai.UserText("plan this work")),
		)
		assert.Contains(t, eventTypes(promptEvents), EventQuestionRequired)
		assert.NotContains(t, eventTypes(promptEvents), EventApprovalRequired)

		paused := runtime.Snapshot()
		assert.Equal(t, PhasePaused, paused.Phase)
		require.NotNil(t, paused.Question.Required)
		request := question.CloneRequest(*paused.Question.Required)

		_, staleErr := collectRuntimeResult(runtime.ResolveQuestion(t.Context(), question.Resolution{
			RequestID: "stale", SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{{Selections: []string{"React"}}},
		}))
		require.ErrorIs(t, staleErr, ErrRuntimeInvalid)
		assert.Equal(t, request.ID, runtime.Snapshot().Question.Required.ID)

		resolveEvents := collectRuntimeEvents(
			t,
			runtime.ResolveQuestion(t.Context(), question.Resolution{
				RequestID: request.ID, SchemaDigest: request.SchemaDigest,
				Answers: []question.Answer{{Selections: []string{"React"}}},
			}),
		)
		assert.Contains(t, eventTypes(resolveEvents), EventQuestionResolved)
		assert.Contains(t, eventTypes(resolveEvents), EventRunCompleted)
		assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
		assert.Nil(t, runtime.Snapshot().Question.Required)
		assert.Len(t, model.Requests(), 2)
		assert.True(t, requestContainsToolText(model.Requests()[1], "React"))
	})

	t.Run("reject", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel(
			runtimeQuestionResponse(t, "call-cancel"),
			runtimeTextResponse("continued without the answer"),
		)
		runtime := openTestRuntime(t, model)
		collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))
		request := runtime.Snapshot().Question.Required
		require.NotNil(t, request)

		events := collectRuntimeEvents(
			t,
			runtime.RejectQuestion(t.Context(), request.ID, request.SchemaDigest),
		)
		assert.Contains(t, eventTypes(events), EventQuestionRejected)
		assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
		assert.True(t, requestContainsToolText(
			model.Requests()[1],
			"canceled this question",
		))
	})
}

func TestRuntimeQuestionSurvivesRestartWithoutModelReplay(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeQuestionResponse(t, "call-restart")),
	)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("choose")))
	before := question.CloneRequest(*first.Snapshot().Question.Required)
	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	model := newRuntimeModel(runtimeTextResponse("resumed"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, model)
	assert.Equal(t, PhasePaused, second.Snapshot().Phase)
	events := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(events), EventQuestionRequired)

	after := second.Snapshot().Question.Required
	require.NotNil(t, after)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.SchemaDigest, after.SchemaDigest)
	assert.Empty(t, model.Requests(), "reconciliation must not replay model work")

	collectRuntimeEvents(t, second.ResolveQuestion(t.Context(), question.Resolution{
		RequestID: after.ID, SchemaDigest: after.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	}))
	assert.Len(t, model.Requests(), 1)
}

func runtimeQuestionResponse(t *testing.T, callID string) *ai.Response {
	t.Helper()

	args, err := json.Marshal(question.Spec{Questions: []question.Question{{
		Header: "Framework", Question: "Which framework should be used?",
		Options: []question.Option{
			{Label: "React", Description: "Established ecosystem"},
			{Label: "Vue", Description: "Progressive framework"},
		},
	}}})
	require.NoError(t, err)

	return runtimeToolResponse(callID, question.ToolName, string(args))
}

func requestContainsToolText(request ai.Request, expected string) bool {
	for _, message := range request.Messages {
		for _, part := range message.Parts {
			result, ok := part.(ai.ToolResultPart)
			if !ok {
				continue
			}

			for _, content := range result.Content {
				if text, ok := content.(ai.TextPart); ok && strings.Contains(text.Text, expected) {
					return true
				}
			}
		}
	}

	return false
}
