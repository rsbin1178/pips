//nolint:wsl_v5 // Keep lifecycle setup beside its observable assertions.
package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeGoalReopenRequiresExplicitResumeBeforePendingReconciliation(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	first := openTestRuntimeAt(t, base, SessionTarget{}, newGoalTestModel(runtimeQuestionResponse(t, "goal-question")))
	_, err := collectRuntimeResult(first.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt after asking"}))
	require.NoError(t, err)
	before := first.Snapshot()
	require.NotNil(t, before.Question.Required)
	require.NoError(t, first.Close(t.Context()))

	model := newGoalTestModel(runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: before.SessionID}, model)
	require.NoError(t, os.WriteFile(filepath.Join(second.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err = collectRuntimeResult(second.Continue(t.Context()))
	require.ErrorIs(t, err, ErrRuntimePending)
	assert.Empty(t, model.Requests())
	_, err = collectRuntimeResult(second.ResumeGoal(t.Context()))
	require.NoError(t, err)
	state := second.Snapshot()
	require.Equal(t, GoalBlocked, state.Goal.Status)
	require.NotNil(t, state.Question.Required)
	assert.Equal(t, before.Question.Required.ID, state.Question.Required.ID)
	assert.Empty(t, model.Requests())
	_, err = collectRuntimeResult(second.ResolveQuestion(t.Context(), question.Resolution{
		RequestID: state.Question.Required.ID, SchemaDigest: state.Question.Required.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	}))
	require.NoError(t, err)
	assert.True(t, second.GoalSnapshot().Completed())
	assert.Len(t, model.Requests(), 2)
}

func TestRuntimeGoalPlanReviewRemainsExplicit(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(
		runtimeToolResponse("goal-enter", planmode.EnterToolName, `{}`),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate"),
	)
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt in plan mode"}))
	require.NoError(t, err)
	require.Equal(t, GoalBlocked, runtime.GoalSnapshot().Status)
	assert.Zero(t, model.evaluated)
	request := planReviewRequest(t, runtime)
	_, err = collectRuntimeResult(runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID, Decision: planreview.DecisionApprove,
	}))
	require.NoError(t, err)
	assert.True(t, runtime.GoalSnapshot().Completed())
	assert.Equal(t, planmode.StateActive, runtime.PlanState())
	assert.Len(t, model.Requests(), 3)
}

func TestRuntimeGoalVerifiedCompletionWinsOverNoProgressGuard(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel()
	for attempt := range 4 {
		model.responses = append(model.responses,
			runtimeToolResponse(fmt.Sprintf("read-%d", attempt), "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	}
	model.outcomes = []string{"continue", "continue", "continue", "complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	assert.True(t, runtime.GoalSnapshot().Completed())
	assert.Equal(t, 4, runtime.GoalSnapshot().Evaluations)
}

func TestRuntimeGoalClearDropsLateNotificationsWithoutBlockingUnrelatedWork(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"), runtimeTextResponse("unrelated result"))
	runtime := openTestRuntime(t, model)
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	root := runtime.Snapshot().Interaction.ID
	require.NoError(t, runtime.ClearGoal(t.Context()))
	require.NoError(t, runtime.notifications.Enqueue(testAgentNotification(runtime, "late-goal-child", root)))
	runtime.deliverNotificationBatch(t.Context())
	assert.Len(t, model.Requests(), 2)
	assert.Empty(t, runtime.GoalSnapshot().ID)
	require.NoError(t, runtime.notifications.Enqueue(testAgentNotification(runtime, "unrelated-child", "unrelated-root")))
	runtime.deliverNotificationBatch(t.Context())
	assert.Len(t, model.Requests(), 3)
	pending, err := runtime.notifications.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestRuntimeGoalForkDoesNotInheritControl(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	model := newGoalTestModel(runtimeTextResponse("one"), runtimeTextResponse("two"), runtimeTextResponse("three"))
	model.outcomes = []string{"continue", "continue", "continue"}
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "pending work"}))
	require.NoError(t, err)
	id, err := runtime.Fork(t.Context(), runtime.session.LeafID())
	require.NoError(t, err)
	forked := openTestRuntimeAt(t, base, SessionTarget{ID: id}, newGoalTestModel())
	assert.Empty(t, forked.GoalSnapshot().ID)
	assert.True(t, runtime.GoalSnapshot().Active())
}

func TestRuntimeGoalNavigationInvalidatesInterruptedDecisionEvidence(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeToolResponse("old-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	model.evaluationError = true
	runtime := openTestRuntime(t, model)
	proof := filepath.Join(runtime.workspace.Root(), "proof.txt")
	require.NoError(t, os.WriteFile(proof, []byte("old proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.Error(t, err)
	var target string
	for _, entry := range runtime.session.Entries() {
		if _, ok := entry.Message.(ai.UserMessage); ok {
			target = entry.ID
			break
		}
	}
	require.NotEmpty(t, target)
	_, err = collectRuntimeResult(runtime.Navigate(t.Context(), target, false))
	require.NoError(t, err)
	require.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	require.NoError(t, os.WriteFile(proof, []byte("fresh proof\n"), 0o600))
	model.evaluationError = false
	setRuntimeResponses(model.runtimeModel, runtimeToolResponse("fresh-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("fresh candidate"))
	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	assert.True(t, runtime.GoalSnapshot().Completed())
	assert.Len(t, model.Requests(), 4, "the invalidated decision must gather fresh work evidence")
}

var _ ai.LanguageModel = (*goalTestModel)(nil)
