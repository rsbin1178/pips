package coding

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackgroundBoundsMatchTheReference pins the monitor output bound. The
// registry is monitor-only, so the bound is the reference's 10 MiB monitor cap,
// and the inlined tail and the mid-turn buffer stay as they are.
func TestBackgroundBoundsMatchTheReference(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 10<<20, backgroundOutputBytes)
	assert.Equal(t, 24<<10, backgroundTailBytes)
	assert.Equal(t, 50, maxPendingBackgroundEvents)
}

func todoPlanResponse(id, step string) *ai.Response {
	return runtimeToolResponse(
		id,
		tasklist.ToolName,
		`{"plan":[{"step":"`+step+`","status":"pending"}]}`,
	)
}

func promptCollect(t *testing.T, runtime *Runtime, text string) {
	t.Helper()

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText(text)))
}

// freshTurnReminder returns the reminder this request's own prompt carries. The
// conversation keeps every earlier reminder, so only the one immediately before
// the last message is this turn's.
func freshTurnReminder(request ai.Request) string {
	messages := request.Messages
	if len(messages) < 2 {
		return ""
	}

	text, ok := singleUserText(messages[len(messages)-2])
	if !ok || !strings.HasPrefix(text, systemReminderOpen+"\n") {
		return ""
	}

	return text
}

// TestRuntimeTaskToolReportsTheFoldedPlan pins the provider wiring: the
// update_plan Tool folds a merge onto the runtime's own projection, so a
// status-only flip reports the plan's real totals rather than the call's count.
func TestRuntimeTaskToolReportsTheFoldedPlan(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-replace", tasklist.ToolName,
			`{"plan":[{"step":"Inspect","status":"in_progress"},{"step":"Implement","status":"pending"}]}`),
		runtimeTextResponse("done"),
		runtimeToolResponse("call-merge", tasklist.ToolName,
			`{"merge":true,"plan":[{"id":"Inspect","status":"completed"}]}`),
		runtimeTextResponse("done again"),
	)
	runtime := openTestRuntime(t, model)

	promptCollect(t, runtime, "plan it")
	promptCollect(t, runtime, "flip it")

	for _, request := range model.Requests() {
		if text := toolResultText(request, "call-merge"); text != "" {
			assert.JSONEq(t, `{"updated":true,"completed":1,"total":2}`, text)

			return
		}
	}

	t.Fatal("no request carried the merge tool result")
}

// TestRuntimeTodoNudgeFiresAfterThreeTurnsWithoutAPlanWrite pins the periodic
// nudge: it appears on the fourth turn with no update_plan call and then not
// again within five turns.
func TestRuntimeTodoNudgeFiresAfterThreeTurnsWithoutAPlanWrite(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse("one"),
		runtimeTextResponse("two"),
		runtimeTextResponse("three"),
		runtimeTextResponse("four"),
		runtimeTextResponse("five"),
		runtimeTextResponse("six"),
		runtimeTextResponse("seven"),
		runtimeTextResponse("eight"),
		runtimeTextResponse("nine"),
	)
	runtime := openTestRuntime(t, model)

	for turn := 1; turn <= 9; turn++ {
		promptCollect(t, runtime, "turn")
	}

	requests := model.Requests()
	require.Len(t, requests, 9)

	for index := range 3 {
		assert.NotContains(t, freshTurnReminder(requests[index]), todoNudgeText,
			"the nudge must not appear before three turns without a plan write")
	}

	assert.Contains(t, freshTurnReminder(requests[3]), todoNudgeText,
		"the fourth turn without a plan write carries the nudge")

	for index := 4; index < 9; index++ {
		assert.NotContains(t, freshTurnReminder(requests[index]), todoNudgeText,
			"the nudge must not repeat within five turns")
	}
}

// TestRuntimeTodoNudgeResetsOnAPlanWrite pins the reset: a valid update_plan
// call restarts the count, so the next nudge waits for a full run of turns.
func TestRuntimeTodoNudgeResetsOnAPlanWrite(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse("one"),
		runtimeTextResponse("two"),
		todoPlanResponse("call-plan", "ship the fix"),
		runtimeTextResponse("four"),
		runtimeTextResponse("five"),
		runtimeTextResponse("six"),
	)
	runtime := openTestRuntime(t, model)

	for turn := 1; turn <= 5; turn++ {
		promptCollect(t, runtime, "turn")
	}

	requests := model.Requests()
	// The plan-writing turn adds its own request for the tool result.
	require.Len(t, requests, 6)
	assert.NotContains(t, freshTurnReminder(requests[4]), todoNudgeText,
		"the plan write resets the count, so the fourth turn carries no nudge")
}

// TestRuntimeTodoNudgeFiresWithoutAPlanWrite is the control for the reset test:
// the same turns without a plan write carry the nudge.
func TestRuntimeTodoNudgeFiresWithoutAPlanWrite(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse("one"),
		runtimeTextResponse("two"),
		runtimeTextResponse("three"),
		runtimeTextResponse("four"),
	)
	runtime := openTestRuntime(t, model)

	for turn := 1; turn <= 4; turn++ {
		promptCollect(t, runtime, "turn")
	}

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.Contains(t, freshTurnReminder(requests[3]), todoNudgeText,
		"without a plan write the fourth turn carries the nudge")
}

func gateTestRuntime(t *testing.T, enabled bool) (*runtimeModel, *Runtime) {
	t.Helper()

	model := newRuntimeModel(
		todoPlanResponse("call-plan", "ship the fix"),
		runtimeTextResponse("done"),
		runtimeTextResponse("still done"),
		runtimeTextResponse("really done"),
	)

	return model, openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil,
		false, config.SandboxWorkspaceWrite,
		func(value *config.Config) { value.TodoGate = enabled },
	)
}

// TestRuntimeTodoGateContinuesAFinishedPlanTwice pins the opt-in gate: a run
// that ends on a content-only assistant message with a pending plan item runs
// one more turn with the gate reminder, twice, and then stops.
func TestRuntimeTodoGateContinuesAFinishedPlanTwice(t *testing.T) {
	t.Parallel()

	model, runtime := gateTestRuntime(t, true)
	promptCollect(t, runtime, "plan and do it")

	requests := model.Requests()
	require.Len(t, requests, 4, "the gate runs exactly two extra turns")

	assert.NotContains(t, requestReminderText(requests[1]), "outstanding todos")
	require.Contains(t, requestReminderText(requests[2]), "outstanding todos")
	require.Contains(t, requestReminderText(requests[3]), "outstanding todos")
	assert.Contains(t, requestReminderText(requests[2]), "ship the fix",
		"the gate reminder names the affected todos")
}

// TestRuntimeTodoGateOffByDefault pins the opt-in stance: with the gate off the
// content-only end finishes the run with no extra turn.
func TestRuntimeTodoGateOffByDefault(t *testing.T) {
	t.Parallel()

	model, runtime := gateTestRuntime(t, false)
	promptCollect(t, runtime, "plan and do it")

	requests := model.Requests()
	require.Len(t, requests, 2, "the gate adds no turn while it is off")

	for _, request := range requests {
		assert.NotContains(t, requestReminderText(request), "outstanding todos")
	}
}

// TestRuntimeTodoGateYieldsToAnotherInputOwner pins the gate's precedence: it
// never fires while a question, an approval, or a plan review is pending, and a
// hook that ended the run with continue:false is not overridden by a gate turn.
func TestRuntimeTodoGateYieldsToAnotherInputOwner(t *testing.T) {
	t.Parallel()

	_, runtime := gateTestRuntime(t, true)
	current := &interaction{}
	runtime.state.Tasks = tasklist.Snapshot{
		Items: []tasklist.Item{{Step: "ship the fix", Status: tasklist.StatusPending}},
		Total: 1,
	}

	_, fire := runtime.todoGateReminder(current, agent.StopEndTurn)
	require.True(t, fire, "the gate fires on a content-only end with a pending item")

	runtime.todoGateFires = 0
	runtime.state.Question.Required = &question.Request{}
	_, fire = runtime.todoGateReminder(current, agent.StopEndTurn)
	assert.False(t, fire, "a pending question owns the turn")

	runtime.state.Question.Required = nil
	runtime.state.Approval.Kind = ApprovalReview
	_, fire = runtime.todoGateReminder(current, agent.StopEndTurn)
	assert.False(t, fire, "a pending approval owns the turn")

	runtime.state.Approval.Kind = ApprovalNone
	runtime.state.PlanReview.Required = &planreview.Request{}
	_, fire = runtime.todoGateReminder(current, agent.StopEndTurn)
	assert.False(t, fire, "a pending plan review owns the turn")

	// Last, because a hook stop latches for the rest of the interaction.
	runtime.state.PlanReview.Required = nil
	runtime.todoGateFires = 0

	current.requestHookStop()
	_, fire = runtime.todoGateReminder(current, agent.StopEndTurn)
	assert.False(t, fire, "a hook that stopped the run is not continued by the gate")
}

// TestRuntimeTodoNudgeFiresOncePerTurn pins the nudge's per-turn stance: the
// turn that carries it is the only one that does, so a turn that loops back to
// the harness cannot repeat it.
func TestRuntimeTodoNudgeFiresOncePerTurn(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("one"))
	runtime := openTestRuntime(t, model)
	current := &interaction{source: InteractionSourceUser}

	runtime.mu.Lock()
	runtime.planTurnsWithoutUpdate = planNudgeTurnsWithoutWrite
	runtime.planTurnsSinceNudge = planNudgeInterval
	runtime.mu.Unlock()

	require.Equal(t, todoNudgeText, runtime.planNudgeText(current))
	assert.Empty(t, runtime.planNudgeText(current), "the nudge fires once per turn")
}

// TestRuntimeTodoNudgeResetsOnCompaction pins the compaction reset: a compacted
// conversation dropped the plan context the count was measured against, so the
// next nudge needs a full run of turns again.
func TestRuntimeTodoNudgeResetsOnCompaction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntimeAt(
		t, t.TempDir(), SessionTarget{}, newRuntimeModel(checkpointResponse()),
	)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	runtime.config.Compaction.MinSummaryChars = 500
	runtime.contextFixedKnown, runtime.contextFixedTokens = true, 400
	appendRuntimeHistory(t, runtime, 2000, 2000, 500, 500)

	runtime.mu.Lock()
	runtime.planTurnsWithoutUpdate = planNudgeTurnsWithoutWrite + 2
	runtime.planNudgedThisTurn = true
	runtime.mu.Unlock()

	preview, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	require.True(t, preview.Available)
	collectRuntimeEvents(t, runtime.Compact(t.Context(), CompactionRequest{
		PreviewToken: preview.Token,
	}))

	runtime.mu.Lock()
	withoutUpdate := runtime.planTurnsWithoutUpdate
	sinceNudge := runtime.planTurnsSinceNudge
	nudged := runtime.planNudgedThisTurn
	runtime.mu.Unlock()

	assert.Zero(t, withoutUpdate, "compaction restarts the count since the last plan write")
	assert.Equal(t, uint32(planNudgeInterval), sinceNudge, "compaction restarts the nudge interval")
	assert.False(t, nudged)
}
