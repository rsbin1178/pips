//nolint:wsl_v5 // Each declaration fixture keeps its scripted model and assertions adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
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

// TestRuntimeUpdateGoalBlockedStreakPausesTheGoalOnTheThirdDeclaration pins the
// failure signal's bar: the Tool tells the model that only three consecutive
// blocked declarations pause the Goal, and the runtime enforces it — the first
// two are accepted and answered with the streak, and the third pauses the Goal
// with the model's own reason.
func TestRuntimeUpdateGoalBlockedStreakPausesTheGoalOnTheThirdDeclaration(t *testing.T) {
	t.Parallel()

	const reason = "the same migration check has failed three attempts in a row"
	model := newGoalTestModel(
		runtimeToolResponse("blocked-1", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeToolResponse("blocked-2", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeToolResponse("blocked-3", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeTextResponse("stuck"),
	)
	runtime := openTestRuntime(t, model)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "migrate the auth table"}))
	// A pause of an admitted work stage cancels that stage, exactly as an operator
	// pause does, so the run reports the cancellation the third declaration caused.
	require.ErrorIs(t, err, context.Canceled)

	first := declarationResult(t, events, "blocked-1")
	assert.False(t, first.IsError, toolResultTextOf(t, first))
	assert.Contains(t, toolResultTextOf(t, first), "1/3")
	assert.NotContains(t, toolResultTextOf(t, first), "paused as blocked")

	second := declarationResult(t, events, "blocked-2")
	assert.False(t, second.IsError, toolResultTextOf(t, second))
	assert.Contains(t, toolResultTextOf(t, second), "2/3")
	assert.NotContains(t, toolResultTextOf(t, second), "paused as blocked")

	third := declarationResult(t, events, "blocked-3")
	assert.False(t, third.IsError, toolResultTextOf(t, third))
	assert.Contains(t, toolResultTextOf(t, third), reason)
	assert.Contains(t, toolResultTextOf(t, third), "paused as blocked")

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

// TestRuntimeUpdateGoalNonBlockedDeclarationResetsTheBlockedStreak pins the
// other half of the bar: only consecutive blocked declarations count, so a
// progress note between two of them starts the count again.
func TestRuntimeUpdateGoalNonBlockedDeclarationResetsTheBlockedStreak(t *testing.T) {
	t.Parallel()

	const reason = "the same migration check keeps failing"
	model := newGoalTestModel(
		runtimeToolResponse("blocked-1", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeToolResponse("note-1", tools.UpdateGoalName, `{"message":"still working on it"}`),
		runtimeToolResponse("blocked-2", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeToolResponse("blocked-3", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeToolResponse("blocked-4", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
		runtimeTextResponse("stuck"),
	)
	runtime := openTestRuntime(t, model)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "migrate the auth table"}))
	require.ErrorIs(t, err, context.Canceled)

	assert.Contains(t, toolResultTextOf(t, declarationResult(t, events, "blocked-1")), "1/3")
	assert.Contains(t, toolResultTextOf(t, declarationResult(t, events, "note-1")), "still working on it")
	// The note ended the run of blocked declarations: two more of them only reach
	// 2/3, and the third after the note is the one that pauses.
	assert.Contains(t, toolResultTextOf(t, declarationResult(t, events, "blocked-2")), "1/3")
	assert.Contains(t, toolResultTextOf(t, declarationResult(t, events, "blocked-3")), "2/3")
	assert.Contains(t, toolResultTextOf(t, declarationResult(t, events, "blocked-4")), "paused as blocked")

	require.Eventually(t, func() bool {
		return runtime.GoalSnapshot().Status == GoalPaused
	}, 30*time.Second, 10*time.Millisecond)
	assert.Equal(t, reason, runtime.GoalSnapshot().Reason)
}

// TestRuntimeUpdateGoalBlockedStreakResetsWithTheGoal pins the streak's
// lifetime: it is state of the run that declared it, so clearing the Goal and
// creating another, and resuming the same one, both start the count again.
func TestRuntimeUpdateGoalBlockedStreakResetsWithTheGoal(t *testing.T) {
	t.Parallel()

	const reason = "the same migration check keeps failing"

	t.Run("create", func(t *testing.T) {
		t.Parallel()

		model := newGoalTestModel(
			runtimeToolResponse("blocked-1", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
			runtimeToolResponse("blocked-2", tools.UpdateGoalName, `{"blocked_reason":"`+reason+`"}`),
			runtimeTextResponse("still working"),
		)
		runtime := openTestRuntime(t, model)
		// The budget ends the Goal after two blocked declarations, so the streak is
		// still 2 when the next Goal is created in its place.
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "migrate the auth table", MaxTokens: 1}))
		require.NoError(t, err)
		require.Equal(t, GoalLimited, runtime.GoalSnapshot().Status)

		waitForChildGoal(t, runtime, model, "s-goal-child")
		summary, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{BlockedReason: reason})
		require.NoError(t, err)
		assert.Contains(t, summary, "1/3")
	})

	t.Run("clear", func(t *testing.T) {
		t.Parallel()

		model := newGoalTestModel()
		runtime := openTestRuntime(t, model)
		waitForChildGoal(t, runtime, model, "s-goal-child")
		declareBlockedTwice(t, runtime, reason)

		require.NoError(t, runtime.ClearGoal(t.Context()))
		require.Empty(t, runtime.GoalSnapshot().ID)
		// A cleared Goal takes no further declaration until another Goal exists, so
		// the clear's own reset is only visible on the control it clears.
		runtime.goalControl.mu.Lock()
		clearedStreak := runtime.goalControl.blockedStreak
		runtime.goalControl.mu.Unlock()
		assert.Equal(t, 0, clearedStreak)

		waitForChildGoal(t, runtime, model, "s-goal-child-2")
		summary, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{BlockedReason: reason})
		require.NoError(t, err)
		assert.Contains(t, summary, "1/3")
		assert.Equal(t, GoalWaiting, runtime.GoalSnapshot().Status)
	})

	t.Run("resume", func(t *testing.T) {
		t.Parallel()

		model := newGoalTestModel()
		runtime := openTestRuntime(t, model)
		waitForChildGoal(t, runtime, model, "s-goal-child")
		declareBlockedTwice(t, runtime, reason)

		require.NoError(t, runtime.PauseGoal(t.Context()))
		require.Eventually(t, func() bool {
			return runtime.GoalSnapshot().Status == GoalPaused
		}, 30*time.Second, 10*time.Millisecond)

		_, err := collectRuntimeResult(runtime.ResumeGoal(t.Context()))
		require.NoError(t, err)

		summary, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{BlockedReason: reason})
		require.NoError(t, err)
		assert.Contains(t, summary, "1/3")
	})
}

// declareBlockedTwice records two blocked declarations against an active Goal and
// asserts that neither of them paused it.
func declareBlockedTwice(t *testing.T, runtime *Runtime, reason string) {
	t.Helper()

	for streak := range 2 {
		summary, err := runtime.declareGoal(t.Context(), tools.GoalDeclaration{BlockedReason: reason})
		require.NoError(t, err)
		assert.Contains(t, summary, fmt.Sprintf("%d/3", streak+1))
	}

	assert.NotEqual(t, GoalPaused, runtime.GoalSnapshot().Status)
}

// waitForChildGoal drives one Goal segment that ends waiting for a background
// child, so the Goal stays active with no live driver and a declaration can be
// called against it directly.
func waitForChildGoal(t *testing.T, runtime *Runtime, model *goalTestModel, childID string) {
	t.Helper()

	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	model.responses = append(model.responses, runtimeTextResponse("waiting for child"))

	var runErr error
	runtime.StartGoal(t.Context(), GoalRequest{Condition: "wait for the child"})(func(event Event, err error) bool {
		runErr = errors.Join(runErr, err)
		if event.Type == EventInteractionCompleted {
			runErr = errors.Join(runErr, runtime.notifications.Enqueue(testAgentNotification(runtime, childID, event.InteractionID)))
		}
		return true
	})
	require.NoError(t, runErr)
	require.Equal(t, GoalWaiting, runtime.GoalSnapshot().Status)
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
	model.verifierVerdicts = []string{goalRefutingVerdict}
	model.outcomes = []string{"complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	// The declaration alone did not complete the Goal: the auditor rejected it —
	// the ledger holds nothing yet, and the declaration itself is not evidence —
	// so the loop continued with the auditor's own gap. The rejection is the
	// auditor's verdict now, because the machine no longer refuses an empty
	// ledger before asking.
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)
	assert.Equal(t, 2, model.verifications)
	assert.Equal(t, 1, model.evaluated)

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
	assert.True(t, requestContainsText(requests[2], "record a check that proves the condition"))
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
	// The declaration's audit is refuted, so the Goal keeps running and the next
	// decision point takes the ordinary assessment; the third attempt's audit
	// takes the default cited verdict.
	model.verifierVerdicts = []string{goalRefutingVerdict}
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
	assert.Equal(t, 2, model.verifications)
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
			runtimeToolResponse("blocked-2", tools.UpdateGoalName, `{"blocked_reason":"three attempts failed"}`),
			runtimeToolResponse("blocked-3", tools.UpdateGoalName, `{"blocked_reason":"three attempts failed"}`),
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
