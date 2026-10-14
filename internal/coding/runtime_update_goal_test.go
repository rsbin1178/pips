//nolint:wsl_v5 // Each declaration fixture keeps its scripted model and assertions adjacent.
package coding

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declarationResult returns one call's model-facing result from the events a
// Goal run emitted.
func declarationResult(t *testing.T, events []Event, callID string) ai.ToolResultPart {
	t.Helper()

	results := toolResultsByCallID(events)
	require.Contains(t, results, callID)

	return results[callID]
}

// TestRuntimeUpdateGoalEchoesAProgressNote pins the message channel: a note is
// accepted and echoed, and it changes nothing about the run.
func TestRuntimeUpdateGoalEchoesAProgressNote(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("note-1", tools.UpdateGoalName, `{"message":"halfway through the migration"}`),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.outcomes = []string{"complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	note := declarationResult(t, events, "note-1")
	assert.False(t, note.IsError, toolResultTextOf(t, note))
	assert.Contains(t, toolResultTextOf(t, note), `"success":true`)
	assert.Contains(t, toolResultTextOf(t, note), "halfway through the migration")

	// The note changed nothing: the ordinary assessment still ran and completed.
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, model.evaluated)
}

// TestRuntimeUpdateGoalBlockedReasonPausesTheGoalWithThatReason pins the failure
// signal: the model's own reason pauses the Goal through the runtime's pause
// path, and the paused Goal state reports it.
func TestRuntimeUpdateGoalBlockedReasonPausesTheGoalWithThatReason(t *testing.T) {
	t.Parallel()

	const reason = "the same migration check has failed three attempts in a row"
	model := newGoalTestModel(
		runtimeToolResponse("blocked-1", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeTextResponse("stuck"),
	)
	runtime := openTestRuntime(t, model)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "migrate the auth table"}))
	// A pause of an admitted work stage cancels that stage, exactly as an operator
	// pause does, so the run reports the cancellation the declaration caused.
	require.ErrorIs(t, err, context.Canceled)

	blocked := declarationResult(t, events, "blocked-1")
	assert.False(t, blocked.IsError, toolResultTextOf(t, blocked))
	assert.Contains(t, toolResultTextOf(t, blocked), reason)
	assert.Contains(t, toolResultTextOf(t, blocked), "paused as blocked")

	require.Eventually(t, func() bool {
		return runtime.GoalSnapshot().Status == GoalPaused
	}, 30*time.Second, 10*time.Millisecond)

	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalPaused, state.Status)
	assert.Equal(t, reason, state.Reason)
	// A blocked declaration is not an assessment: the model asked to stop, not to
	// be judged complete.
	assert.Equal(t, 0, model.evaluated)
	assert.False(t, state.Completed())
}

// TestRuntimeUpdateGoalCompletionDeclarationIsAuditedAtTheNextDecisionPoint pins
// the declaration channel: a completion claim replaces the ordinary assessment
// with the verifier, and a verified verdict ends the run without another work
// turn.
func TestRuntimeUpdateGoalCompletionDeclarationIsAuditedAtTheNextDecisionPoint(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true,"message":"the migration is done"}`),
		runtimeTextResponse("declared complete"),
	)
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	declared := declarationResult(t, events, "declare-1")
	assert.False(t, declared.IsError, toolResultTextOf(t, declared))
	assert.Contains(t, toolResultTextOf(t, declared), "independent evidence audit")

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, state.Attempts)
	// The declaration replaced the assessment, and the verdict is still the
	// auditor's.
	assert.Equal(t, 0, model.evaluated)
	assert.Equal(t, 1, model.verifications)
}

// TestRuntimeUpdateGoalRejectedDeclarationReturnsTheAuditorsGaps pins the other
// half of the same decision point: an unverified verdict returns the auditor's
// gaps to the model as the loop's own feedback.
func TestRuntimeUpdateGoalRejectedDeclarationReturnsTheAuditorsGaps(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true}`),
		runtimeTextResponse("nothing to show yet"),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.outcomes = []string{"complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	// The declaration alone did not complete the Goal: the auditor found no
	// recorded evidence — the declaration itself is not evidence — so the loop
	// continued with that gap.
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)
	// The declaration's audit needed no model verdict, because the auditor's own
	// evidence gate refused it; the ordinary completion at the second attempt is
	// the one verdict.
	assert.Equal(t, 1, model.verifications)
	assert.Equal(t, 1, model.evaluated)

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
	assert.True(t, requestContainsText(requests[2], "No actual tool evidence was recorded"))
}

// TestRuntimeUpdateGoalDeclarationIsConsumedOnce pins the consumption: a
// declaration is verified at exactly one decision point, and the next decision
// point takes the ordinary assessment again.
func TestRuntimeUpdateGoalDeclarationIsConsumedOnce(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true}`),
		runtimeTextResponse("nothing to show yet"),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate"),
		runtimeToolResponse("proof-read-2", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.outcomes = []string{"continue", "complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	// Three attempts: the declaration's rejected audit, then two assessed turns.
	// A declaration left pending would have verified again at the second decision
	// point and completed the Goal at two attempts without assessing anything.
	assert.Equal(t, 3, state.Attempts)
	assert.Equal(t, 2, model.evaluated)
	assert.Equal(t, 1, model.verifications)
}

// TestRuntimeUpdateGoalRefusesOutsideAGoalRun pins the first refusal: the Tool is
// offered outside a Goal run, so a call there is a named sentence rather than an
// unknown Tool.
func TestRuntimeUpdateGoalRefusesOutsideAGoalRun(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true}`),
		runtimeTextResponse("understood"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("report the goal status")))

	refused := declarationResult(t, events, "declare-1")
	require.True(t, refused.IsError, toolResultTextOf(t, refused))

	header, body, err := tools.ParseResult(toolResultTextOf(t, refused))
	require.NoError(t, err)
	assert.False(t, header.OK)
	assert.Equal(t, "goal_not_running", header.Code)
	assert.Contains(t, body, "no Goal run is in progress")
	assert.Empty(t, runtime.GoalSnapshot().ID)
}

// TestRuntimeUpdateGoalRefusesANonActiveGoal pins the second refusal: a paused or
// finished Goal never takes a completion or failure declaration, while a progress
// note, which changes nothing, is still accepted.
func TestRuntimeUpdateGoalRefusesANonActiveGoal(t *testing.T) {
	t.Parallel()

	t.Run("finished", func(t *testing.T) {
		t.Parallel()

		model := newGoalTestModel(
			runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
			runtimeTextResponse("candidate complete"),
		)
		model.outcomes = []string{"complete"}
		runtime := openTestRuntime(t, model)
		require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
		require.NoError(t, err)
		require.True(t, runtime.GoalSnapshot().Completed())

		model.responses = append(model.responses,
			runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true}`),
			runtimeToolResponse("note-1", tools.UpdateGoalName, `{"message":"still here"}`),
			runtimeTextResponse("done"),
		)
		events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("report the goal status")))

		refused := declarationResult(t, events, "declare-1")
		require.True(t, refused.IsError, toolResultTextOf(t, refused))

		header, body, err := tools.ParseResult(toolResultTextOf(t, refused))
		require.NoError(t, err)
		assert.Equal(t, "goal_not_active", header.Code)
		assert.Contains(t, body, "the Goal is not active")
		assert.Contains(t, body, "completed")

		note := declarationResult(t, events, "note-1")
		assert.False(t, note.IsError, toolResultTextOf(t, note))
		assert.Contains(t, toolResultTextOf(t, note), "still here")
	})

	t.Run("paused", func(t *testing.T) {
		t.Parallel()

		model := newGoalTestModel(
			runtimeToolResponse("blocked-1", tools.UpdateGoalName, `{"blocked_reason":"three attempts failed"}`),
			runtimeTextResponse("stuck"),
		)
		runtime := openTestRuntime(t, model)
		_, _ = collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "migrate the auth table"}))
		require.Eventually(t, func() bool {
			return runtime.GoalSnapshot().Status == GoalPaused
		}, 30*time.Second, 10*time.Millisecond)

		// A paused Goal is not running, so a later completion declaration is
		// refused rather than silently recorded. The channel is the one the Tool
		// carries; a paused Goal owns no conversation turn to call it from.
		_, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{Completed: true})
		require.ErrorIs(t, err, tools.ErrGoalNotActive)
		assert.False(t, runtime.GoalSnapshot().Completed())

		summary, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{Message: "noted"})
		require.NoError(t, err)
		assert.Equal(t, "noted", summary)
	})
}
