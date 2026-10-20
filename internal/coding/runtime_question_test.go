package coding

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/question"
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

func TestRuntimeCloseParkedOnQuestionIsClean(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeQuestionResponse(t, "call-restart")),
	)
	sessionID := first.handle.Metadata().ID
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("choose")))
	require.NotNil(t, first.Snapshot().Question.Required)

	require.NoError(t, first.Close(t.Context()))

	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, newRuntimeModel())
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	require.Equal(t, PhasePaused, second.Snapshot().Phase)
	events := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(events), EventQuestionRequired)
}

func TestRuntimeRegistersOnlyUnifiedAskUserTool(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("no question needed"))
	runtime := openTestRuntime(t, model)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))

	requests := model.Requests()
	require.NotEmpty(t, requests)

	names := make([]string, 0, len(requests[0].Tools))
	for _, tool := range requests[0].Tools {
		names = append(names, tool.Name)
	}

	assert.Contains(t, names, question.ToolName)
	assert.NotContains(t, names, question.TextToolName)
}

func TestRuntimeQuestionBatchQueuesEachCall(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeQuestionBatchResponse(t, "call-1", "call-2", "call-3"),
		runtimeTextResponse("all three answered"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("ask three")))
	assert.Contains(t, eventTypes(events), EventQuestionRequired)

	first := runtime.Snapshot().Question.Required
	require.NotNil(t, first)
	assert.Equal(t, 1, first.QueueIndex)
	assert.Equal(t, 3, first.QueueSize)

	events = collectRuntimeEvents(t, runtime.ResolveQuestion(t.Context(), question.Resolution{
		RequestID: first.ID, SchemaDigest: first.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	}))
	assert.Contains(t, eventTypes(events), EventQuestionResolved)
	assert.Contains(t, eventTypes(events), EventQuestionRequired)

	second := runtime.Snapshot().Question.Required
	require.NotNil(t, second)
	assert.Equal(t, 2, second.QueueIndex)
	assert.Equal(t, 3, second.QueueSize)

	events = collectRuntimeEvents(
		t,
		runtime.RejectQuestion(t.Context(), second.ID, second.SchemaDigest),
	)
	assert.Contains(t, eventTypes(events), EventQuestionRejected)
	assert.Contains(t, eventTypes(events), EventQuestionRequired)

	third := runtime.Snapshot().Question.Required
	require.NotNil(t, third)
	assert.Equal(t, 3, third.QueueIndex)
	assert.Equal(t, 3, third.QueueSize)

	events = collectRuntimeEvents(t, runtime.ResolveQuestion(t.Context(), question.Resolution{
		RequestID: third.ID, SchemaDigest: third.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"Vue"}}},
	}))
	assert.Contains(t, eventTypes(events), EventQuestionResolved)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	assert.Nil(t, runtime.Snapshot().Question.Required)

	requests := model.Requests()
	require.Len(t, requests, 2)
	results := runtimeToolResultsByID(requests[1])
	require.Len(t, results, 3)
	assert.Contains(t, toolResultText(requests[1], "call-1"), "React")
	assert.True(t, results["call-2"].IsError)
	assert.Contains(t, toolResultText(requests[1], "call-2"), "canceled")
	assert.Contains(t, toolResultText(requests[1], "call-3"), "Vue")
}

func TestRuntimeMixedAskBatchIsDeniedAsOneUnit(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "runtime-test",
			Message: ai.Assistant(
				runtimeQuestionCall(t, "ask-1"),
				ai.ToolCallPart{ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"missing.txt"}`)},
				runtimeQuestionCall(t, "ask-2"),
			),
			FinishReason: ai.FinishToolCalls,
		},
		runtimeTextResponse("retry separately"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("ask and read")))
	assert.NotContains(t, eventTypes(events), EventQuestionRequired)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)

	requests := model.Requests()
	require.Len(t, requests, 2)
	results := runtimeToolResultsByID(requests[1])
	require.Len(t, results, 3)

	for _, id := range []string{"ask-1", "read-1", "ask-2"} {
		assert.True(t, results[id].IsError)
		assert.Contains(t, toolResultText(requests[1], id), question.MixedBatchErrorPrefix)
	}
}

func runtimeQuestionBatchResponse(t *testing.T, ids ...string) *ai.Response {
	t.Helper()

	parts := make([]ai.AssistantPart, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, runtimeQuestionCall(t, id))
	}

	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "runtime-test",
		Message: ai.Assistant(parts...), FinishReason: ai.FinishToolCalls,
	}
}

func runtimeQuestionCall(t *testing.T, id string) ai.ToolCallPart {
	t.Helper()

	args, err := json.Marshal(question.Spec{Questions: []question.Question{{
		Header: "Framework", Question: "Which framework should be used?",
		Options: []question.Option{
			{Label: "React", Description: "Established ecosystem"},
			{Label: "Vue", Description: "Progressive framework"},
		},
	}}})
	require.NoError(t, err)

	return ai.ToolCallPart{ID: id, Name: question.ToolName, Args: args}
}

func runtimeToolResultsByID(request ai.Request) map[string]ai.ToolResultPart {
	results := make(map[string]ai.ToolResultPart)

	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}

		for _, part := range parts {
			if result, ok := part.(ai.ToolResultPart); ok {
				results[result.ToolCallID] = result
			}
		}
	}

	return results
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
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}

		for _, part := range parts {
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
