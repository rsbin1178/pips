//nolint:wsl_v5 // Keep the scripted child model adjacent to its behavioral assertions.
package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goalSpawnedModel scripts a Goal run whose verification panel spawns real
// foreground skeptic children. It answers the assessment itself, answers each
// child skeptic's task with the scripted verdict, and serves the work segments
// from its response script.
type goalSpawnedModel struct {
	*runtimeModel

	mu sync.Mutex
	// verdicts are the panel's scripted verdicts, one per verification round
	// applied to every skeptic of that round; once they run out,
	// goalVerifiedVerdict answers every further round.
	verdicts []string
	// positionVerdicts overrides one one-based panel position for every round,
	// so a test can script a split panel.
	positionVerdicts map[int]string
	// childFailure fails every child skeptic call, so a test can pin a child
	// failure becoming a skeptic failure.
	childFailure error
	// childShell makes every skeptic's first act one shell command, so a test can
	// drive a child into its approval boundary. childShellInput overrides the
	// call's arguments, so the command can carry an elevated permission request.
	childShell      string
	childShellInput string
	childShellCalls int
	// skepticStarted and blockSkeptic hold the panel inside a child, so a test can
	// observe a pause while the panel runs.
	skepticStarted chan struct{}
	blockSkeptic   chan struct{}
	// tasks records every child skeptic task in call order.
	tasks []string
	// taskMessages records each child skeptic request's message count, so a test
	// can pin that a resumed child carries its source's transcript.
	taskMessages []int
	// taskPositions records each child skeptic call's one-based panel position.
	taskPositions []int
	// childCalls counts the child verdict calls, so a scripted verdict applies to
	// a whole panel rather than to one call.
	childCalls int
}

func newGoalSpawnedModel(work ...*ai.Response) *goalSpawnedModel {
	return &goalSpawnedModel{runtimeModel: newRuntimeModel(work...)}
}

// scriptGoalSpawnedRounds appends count work segments, the smallest real segment
// that records tool evidence and then answers.
func scriptGoalSpawnedRounds(model *goalSpawnedModel, count int) {
	for index := range count {
		model.responses = append(model.responses,
			runtimeToolResponse(fmt.Sprintf("proof-read-%d", index), "read", `{"path":"proof.txt"}`),
			runtimeTextResponse("candidate complete"),
		)
	}
}

func (m *goalSpawnedModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

func (m *goalSpawnedModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if request.ResponseFormat != nil && request.ResponseFormat.Name == "goal_evaluation" {
		return runtimeTextResponse(
			`{"decision":"candidate_complete","evidence":"the recorded read proves the condition",` +
				`"next_step":"verify the candidate","blocker_key":""}`,
		), nil
	}

	if task, messages, ok := goalSkepticTask(request); ok {
		if (m.childShell != "" || m.childShellInput != "") && !requestCarriesToolResult(request) {
			return m.shellProbe()
		}

		return m.audit(task, messages)
	}

	return m.runtimeModel.Generate(ctx, request)
}

// audit answers one child skeptic's task with its panel position's scripted
// verdict.
func (m *goalSpawnedModel) audit(task string, messages int) (*ai.Response, error) {
	position, total := goalPanelPosition(task)

	m.mu.Lock()
	m.tasks = append(m.tasks, task)
	m.taskMessages = append(m.taskMessages, messages)
	m.taskPositions = append(m.taskPositions, position)
	m.childCalls++
	round := (m.childCalls - 1) / max(total, 1)
	scripted := m.verifierVerdictFor(round, position)
	failure := m.childFailure
	started, block := m.skepticStarted, m.blockSkeptic
	m.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	if block != nil {
		<-block
	}

	if failure != nil {
		return nil, failure
	}

	return runtimeTextResponse(scripted), nil
}

// shellProbe answers a skeptic's first call with one shell command.
func (m *goalSpawnedModel) shellProbe() (*ai.Response, error) {
	m.mu.Lock()
	m.childShellCalls++
	call := m.childShellCalls
	input := m.childShellInput
	command := m.childShell
	m.mu.Unlock()

	if input == "" {
		input = fmt.Sprintf(`{"command":%q}`, command)
	}

	return runtimeToolResponse(fmt.Sprintf("verifier-shell-%d", call), "shell", input), nil
}

// requestCarriesToolResult reports whether a request already carries a tool
// result, which is how the fixture tells a child's first call from its follow-up.
func requestCarriesToolResult(request ai.Request) bool {
	for _, message := range request.Messages {
		if _, ok := message.(ai.ToolMessage); ok {
			return true
		}
	}

	return false
}

func (m *goalSpawnedModel) verifierVerdictFor(round, position int) string {
	if verdict, ok := m.positionVerdicts[position]; ok {
		return verdict
	}

	if round < len(m.verdicts) {
		return m.verdicts[round]
	}

	return goalVerifiedVerdict
}

func (m *goalSpawnedModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (m *goalSpawnedModel) skepticTasks() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.tasks...)
}

// taskMessageCounts returns every child skeptic request's message count in call
// order, paired with its one-based panel position.
func (m *goalSpawnedModel) taskMessageCounts() ([]int, []int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]int(nil), m.taskMessages...), append([]int(nil), m.taskPositions...)
}

// goalSkepticTask reports whether a request is a child skeptic's, returning its
// task and message count. A resumed child's request carries its source's
// transcript too, so the newest task is the last message that names the panel.
func goalSkepticTask(request ai.Request) (string, int, bool) {
	task := ""

	for _, message := range request.Messages {
		text := runtimeMessageText(message)
		if strings.Contains(text, "independent Coding Goal evidence auditor") {
			task = text
		}
	}

	return task, len(request.Messages), task != ""
}

// openSpawnedGoalTestRuntime opens a test Runtime whose Goal verification panel
// spawns real foreground skeptic children instead of auditing in-process.
func openSpawnedGoalTestRuntime(t *testing.T, model ai.LanguageModel) *Runtime {
	t.Helper()

	runtime := openTestRuntime(t, model)
	runtime.opts.GoalVerifierInProcess = false

	return runtime
}

// openFullAccessSpawnedGoalTestRuntime opens a spawned-panel Runtime whose
// sandbox lets the fixture prepare a Git repository in the workspace.
func openFullAccessSpawnedGoalTestRuntime(t *testing.T, model ai.LanguageModel) *Runtime {
	t.Helper()

	runtime := openFullAccessTestRuntimeConfiguredWithTrust(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
	)
	runtime.opts.GoalVerifierInProcess = false

	return runtime
}

// goalPersistedState decodes the durable Coding Goal wrapper state, so a test
// can inspect a field the projection does not carry.
func goalPersistedState(t *testing.T, runtime *Runtime) goalflow.State {
	t.Helper()

	runtime.goalControl.mu.Lock()
	id := runtime.goalControl.id
	runtime.goalControl.mu.Unlock()
	require.NotEmpty(t, id)

	execution, err := runtime.goalControl.engine.Get(t.Context(), id)
	require.NoError(t, err)

	state, err := goalflow.Decode(execution.ControllerState)
	require.NoError(t, err)

	return state
}

// goalVerifierPanelReport reads the harness's diagnostic panel report.
func goalVerifierPanelReport(t *testing.T, runtime *Runtime, goalID string) string {
	t.Helper()

	//nolint:gosec // The path is the fixture's own owner-only state directory.
	report, err := os.ReadFile(filepath.Join(
		runtime.handle.StateDir(), goalStateDirName, goalVerifierDirName, goalID, goalVerifierPanelFile,
	))
	require.NoError(t, err)

	return string(report)
}

// writeGoalSpawnedProof writes the file every spawned-panel fixture reads.
func writeGoalSpawnedProof(t *testing.T, runtime *Runtime) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600,
	))
}

// TestGoalVerifierDispatcherCompilesTheGeneralPurposeInventory pins the panel's
// capability surface: the goal-scoped dispatcher is non-interactive and compiles
// general-purpose with the coding tools, the background controls and history —
// and without the question or plan catalogs a verifier must not use.
func TestGoalVerifierDispatcherCompilesTheGeneralPurposeInventory(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	runtime := openSpawnedGoalTestRuntime(t, model)

	dispatcher, err := runtime.goalVerifierDispatcher(t.Context())
	require.NoError(t, err)

	custom, ok := dispatcher.(*customSubagentDispatcher)
	require.True(t, ok, "the panel's dispatcher is the custom one")
	assert.True(t, custom.factory.failOnInput,
		"a skeptic that reaches an approval or question must fail fast, not hang the panel")

	plan, err := dispatcher.Compile(t.Context(), subagent.Request{
		AgentID: "general-purpose", Task: "audit the recorded evidence", Delivery: subagent.DeliveryForeground,
	})
	require.NoError(t, err)

	names := make([]string, 0, len(plan.Capabilities))
	for _, capability := range plan.Capabilities {
		names = append(names, capability.WireName)
	}

	assert.ElementsMatch(t, []string{
		"read", "ls", "glob", "grep", "apply_patch", "shell",
		tools.MonitorName, tools.KillBackgroundName, tools.HistoryName,
	}, names)

	for _, absent := range []string{"ask_user", "ask_user_text", "todo_write", "update_plan", "enter_plan_mode", "spawn_subagent"} {
		assert.NotContains(t, names, absent, "a verifier must not ask the user or enter plan review")
	}
}

// TestRuntimeGoalSpawnedPanelChildRunsATool pins the panel's capability surface
// end to end: a skeptic really reaches for the workspace with a command of its
// own, so the tool inventory is live rather than prompt-only.
func TestRuntimeGoalSpawnedPanelChildRunsATool(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.childShell = "echo verifier-probe"
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())

	model.mu.Lock()
	shellCalls := model.childShellCalls
	model.mu.Unlock()
	assert.Equal(t, testPanelSize, shellCalls, "every skeptic ran one command of its own")
}

// TestRuntimeGoalSpawnedPanelChildApprovalFailsTheSkeptic pins the
// non-interactive boundary end to end: a skeptic that reaches an approval
// boundary fails fast, and that failure is the skeptic's synthetic refute rather
// than a hung panel or a fail-closed infrastructure pause.
func TestRuntimeGoalSpawnedPanelChildApprovalFailsTheSkeptic(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 2)
	model.childShellInput = `{"command":"echo verifier-probe","permissions":{"network":true},"justification":"probe"}`
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.False(t, state.Completed(), "%+v", state)
	require.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, state.Reason, "reported the same gaps")
	assert.Contains(t, strings.Join(state.Gaps, " "), "skeptic failure")
	assert.NotContains(t, state.Reason, "Goal verification infrastructure failed")

	model.mu.Lock()
	shellCalls := model.childShellCalls
	model.mu.Unlock()
	assert.Equal(t, 2*testPanelSize, shellCalls, "every skeptic of both panels reached the approval boundary")
}

// TestRuntimeGoalPauseDuringTheSpawnedPanelStopsIt pins the cancellation path:
// an operator pause cancels the driver context, which cancels the foreground
// children the panel is waiting on, so no additional work starts.
func TestRuntimeGoalPauseDuringTheSpawnedPanelStopsIt(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.skepticStarted = make(chan struct{}, testPanelSize)
	model.blockSkeptic = make(chan struct{})
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	done := make(chan error, 1)

	go func() {
		_, err := collectRuntimeResult(runtime.StartGoal(context.Background(), GoalRequest{Condition: "inspect proof.txt"}))
		done <- err
	}()

	select {
	case <-model.skepticStarted:
	case <-time.After(30 * time.Second):
		require.Fail(t, "the panel never spawned a skeptic")
	}

	require.NoError(t, runtime.PauseGoal(t.Context()))
	close(model.blockSkeptic)

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		require.Fail(t, "the paused panel never stopped")
	}

	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.False(t, runtime.GoalSnapshot().Completed())

	// No further panel runs once the Goal is paused.
	model.mu.Lock()
	tasks := len(model.tasks)
	model.mu.Unlock()
	assert.LessOrEqual(t, tasks, testPanelSize, "a paused Goal starts no second panel")
}

// TestRuntimeGoalClearDuringTheSpawnedPanelStopsIt pins the clear path: clearing
// the Goal cancels the driver context, which cancels the foreground children the
// panel waits on even though the synthetic verifier root is deliberately absent
// from the ledger's roots, so no CancelRoot reaches it.
func TestRuntimeGoalClearDuringTheSpawnedPanelStopsIt(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.skepticStarted = make(chan struct{}, testPanelSize)
	model.blockSkeptic = make(chan struct{})
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	done := make(chan error, 1)

	go func() {
		_, err := collectRuntimeResult(runtime.StartGoal(context.Background(), GoalRequest{Condition: "inspect proof.txt"}))
		done <- err
	}()

	select {
	case <-model.skepticStarted:
	case <-time.After(30 * time.Second):
		require.Fail(t, "the panel never spawned a skeptic")
	}

	require.NoError(t, runtime.ClearGoal(t.Context()))
	close(model.blockSkeptic)

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		require.Fail(t, "the cleared panel never stopped")
	}

	assert.False(t, runtime.GoalSnapshot().Completed())

	// No further panel runs once the Goal is cleared.
	model.mu.Lock()
	tasks := len(model.tasks)
	model.mu.Unlock()
	assert.LessOrEqual(t, tasks, testPanelSize, "a cleared Goal starts no second panel")
}

// TestRuntimeGoalSpawnedPanelAchieves pins the spawned panel end to end: three
// real foreground general-purpose children carry the evidence packet, their
// verdicts aggregate, and the Goal completes.
func TestRuntimeGoalSpawnedPanelAchieves(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, state.Attempts)

	children, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, children, testPanelSize, "one durable child per skeptic")

	for _, child := range children {
		assert.Equal(t, subagent.AgentKindBuiltin, child.Identity.Kind)
		assert.Equal(t, "general-purpose", child.Identity.ID)
		assert.Equal(t, goalVerifierRootPrefix+state.ID, child.Ownership.RootInteractionID)
		assert.Equal(t, subagent.DeliveryForeground, child.Delivery)
		assert.Equal(t, subagent.StateSucceeded, child.State)
	}

	// The verifier root is deliberately not Goal background work, so the child
	// usage accounting and the background guard never see it.
	runtime.goalControl.mu.Lock()
	roots := append([]string(nil), runtime.goalControl.ledger.Roots...)
	childUsage := runtime.goalControl.ledger.ChildUsage
	runtime.goalControl.mu.Unlock()

	for _, root := range roots {
		assert.NotContains(t, root, goalVerifierRootPrefix)
	}

	assert.Zero(t, childUsage, "a verifier child is never accounted as Goal background usage")

	tasks := model.skepticTasks()
	require.Len(t, tasks, testPanelSize)

	for _, task := range tasks {
		assert.Contains(t, task, "OBJECTIVE:\ninspect proof.txt")
		assert.Contains(t, task, "CHANGED_FILES:")
		assert.Contains(t, task, "FINAL_RESPONSE:\ncandidate complete")
		assert.Contains(t, task, "PLAN_FILE: (unavailable)")
		assert.Contains(t, task, "read, ls, glob, grep, apply_patch, shell and monitor")
	}

	report := goalVerifierPanelReport(t, runtime, state.ID)
	assert.Contains(t, report, "# Goal verification panel report")
	assert.Contains(t, report, "## Skeptic 1")
	assert.Contains(t, report, "## Skeptic 3")
	assert.Contains(t, report, "- refuted: false")

	// A one-skeptic-per-position panel records the gatekeeper's session for the
	// next verification, and an achieved panel clears the prior gaps.
	persisted := goalPersistedState(t, runtime)
	assert.NotEmpty(t, persisted.Skeptic0Session)
	assert.Empty(t, persisted.VerifyGaps)
}

// TestRuntimeGoalSpawnedPanelRejects pins the rejection route: every spawned
// skeptic refutes, the aggregate gaps reach the next work segment, and the
// identical second rejection trips the gap-stall guard.
func TestRuntimeGoalSpawnedPanelRejects(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 2)
	model.verdicts = []string{goalRefutingVerdict, goalRefutingVerdict}
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.False(t, state.Completed(), "%+v", state)
	require.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, state.Reason, "reported the same gaps")
	assert.Contains(t, strings.Join(state.Gaps, " "), "record a check that proves the condition")

	// Each work segment spends two scripted responses, so the second segment's
	// first request is the third.
	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
	assert.True(t, requestContainsText(requests[2], "record a check that proves the condition"))
}

// TestRuntimeGoalSpawnedPanelHighConfidenceGatekeeperRefuteVetoes pins the
// decisive veto over the spawned panel: the gatekeeper's high-confidence refute
// overrides the cold quorum.
func TestRuntimeGoalSpawnedPanelHighConfidenceGatekeeperRefuteVetoes(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 3)
	model.positionVerdicts = map[int]string{1: goalHighGatekeeperVerdict}
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.False(t, state.Completed(), "%+v", state)
	require.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, strings.Join(state.Gaps, " "), "the gatekeeper is certain")
}

// TestRuntimeGoalSpawnedPanelBlocksOnAnAllBlockingVerdict pins the blocked
// route over the spawned panel: every refuter is a non-model-fixable blocker, so
// the Goal blocks for a user decision and an explicit resume verifies.
func TestRuntimeGoalSpawnedPanelBlocksOnAnAllBlockingVerdict(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 2)
	model.verdicts = []string{goalBlockingVerdict, goalVerifiedVerdict}
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.Equal(t, GoalBlocked, state.Status, "%+v", state)
	assert.Contains(t, state.Reason, "Goal verification blocked on evidence that iteration cannot resolve")
	assert.Contains(t, state.Reason, "the objective contradicts itself")

	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())
}

// TestRuntimeGoalSpawnedPanelChildFailureIsASkepticFailure pins the failure
// split at the runtime boundary: a child that fails is that skeptic's synthetic
// refute, so the Goal reports gaps and never the fail-closed infrastructure
// pause.
func TestRuntimeGoalSpawnedPanelChildFailureIsASkepticFailure(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 2)
	model.childFailure = errors.New("provider unavailable")
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.False(t, state.Completed(), "%+v", state)
	require.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, state.Reason, "reported the same gaps")
	assert.Contains(t, strings.Join(state.Gaps, " "), "skeptic failure")
	assert.NotContains(t, state.Reason, "Goal verification infrastructure failed")
}

// TestRuntimeGoalSpawnedPanelResumesSkeptic0 pins the resume: the second
// verification resumes the gatekeeper with the reference's resume prompt, the
// resumed child carries its source's transcript, and the panel records the new
// session for the round after it.
func TestRuntimeGoalSpawnedPanelResumesSkeptic0(t *testing.T) {
	t.Parallel()

	model := newGoalSpawnedModel()
	scriptGoalSpawnedRounds(model, 2)
	model.verdicts = []string{goalRefutingVerdict, goalVerifiedVerdict}
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)

	tasks := model.skepticTasks()
	require.Len(t, tasks, 2*testPanelSize)

	// The first round's gatekeeper is cold; the second round's is the resumed
	// one and renders the reference's resume contract.
	assert.NotContains(t, tasks[0], "You are the SAME adversarial verifier")
	assert.NotContains(t, tasks[1], "You are the SAME adversarial verifier")
	assert.NotContains(t, tasks[2], "You are the SAME adversarial verifier")

	resumed := ""
	for _, task := range tasks[testPanelSize:] {
		if strings.Contains(task, "You are the SAME adversarial verifier") {
			resumed = task
		}
	}

	require.NotEmpty(t, resumed, "the second round resumes its gatekeeper")
	assert.True(t, strings.HasPrefix(resumed, "You are skeptic 1 of 3"), resumed)
	assert.Contains(t, resumed, "PRIOR_GAPS:\n")
	assert.Contains(t, resumed, "record a check that proves the condition")

	// A resumed child's request carries its source's own transcript, so it is
	// longer than the cold one it continues.
	counts, positions := model.taskMessageCounts()
	require.Len(t, counts, 2*testPanelSize)

	coldGatekeeper, resumedGatekeeper := 0, 0

	for index, position := range positions {
		if position != 1 {
			continue
		}

		if coldGatekeeper == 0 {
			coldGatekeeper = counts[index]
		} else {
			resumedGatekeeper = counts[index]
		}
	}

	assert.Positive(t, coldGatekeeper)
	assert.Greater(t, resumedGatekeeper, coldGatekeeper, "the resumed skeptic continues its source's session")

	// The panel report of the round that ran last names the resumed skeptic and
	// the session the state now records.
	persisted := goalPersistedState(t, runtime)
	assert.NotEmpty(t, persisted.Skeptic0Session)
	assert.Contains(t, goalVerifierPanelReport(t, runtime, state.ID), "## Skeptic 1 (resumed)")
	assert.Contains(t, goalVerifierPanelReport(t, runtime, state.ID), persisted.Skeptic0Session)
}

// TestGoalVerifierSpawnerFallsBackToAColdSpawn pins the resume fallback: a
// resume that cannot start retries the same skeptic once as a cold spawn with the
// cold contract, and a cold spawn that fails is not retried a second time.
func TestGoalVerifierSpawnerFallsBackToAColdSpawn(t *testing.T) {
	t.Parallel()

	attempts := []goalflow.SkepticRequest{}
	resumeFailure := errors.New("the source session is not resumable")

	result, err := spawnWithColdFallback(goalflow.SkepticRequest{
		Index: 0, Total: 3, Prompt: "resume task", ResumeFrom: "child-source", ColdPrompt: "cold task",
	}, func(attempt goalflow.SkepticRequest) (goalflow.SkepticResult, error) {
		attempts = append(attempts, attempt)

		if attempt.ResumeFrom != "" {
			return goalflow.SkepticResult{}, resumeFailure
		}

		return goalflow.SkepticResult{ChildSessionID: "child-cold", Terminal: "Not Refuted"}, nil
	})
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	assert.Equal(t, "child-source", attempts[0].ResumeFrom)
	assert.Equal(t, "resume task", attempts[0].Prompt)
	assert.Empty(t, attempts[1].ResumeFrom)
	assert.Equal(t, "cold task", attempts[1].Prompt, "a cold judge gets the cold contract, not the resume one")
	assert.Equal(t, "child-cold", result.ChildSessionID)

	coldFailure := errors.New("the child session could not start")
	attempts = attempts[:0]

	_, err = spawnWithColdFallback(goalflow.SkepticRequest{ResumeFrom: "child-source", ColdPrompt: "cold task"},
		func(attempt goalflow.SkepticRequest) (goalflow.SkepticResult, error) {
			attempts = append(attempts, attempt)

			return goalflow.SkepticResult{}, coldFailure
		})
	require.ErrorIs(t, err, coldFailure)
	require.Len(t, attempts, 2, "a failed cold spawn is the skeptic's own failure")

	attempts = attempts[:0]

	result, err = spawnWithColdFallback(goalflow.SkepticRequest{Prompt: "cold task"},
		func(attempt goalflow.SkepticRequest) (goalflow.SkepticResult, error) {
			attempts = append(attempts, attempt)

			return goalflow.SkepticResult{ChildSessionID: "child-first"}, nil
		})
	require.NoError(t, err)
	require.Len(t, attempts, 1, "a cold spawn is never retried")
	assert.Equal(t, "child-first", result.ChildSessionID)
}

// TestGoalVerifierSkeptic0UsageIsADelta pins the resumed skeptic's usage// accounting: a resumed child reports cumulative usage, so the panel reports
// only what the resumed run spent.
func TestGoalVerifierSkeptic0UsageIsADelta(t *testing.T) {
	t.Parallel()

	spawner := &goalVerifierSpawner{runtime: &Runtime{goalControl: &runtimeGoalControl{}}}

	cold := spawner.usageDelta(0, "", subagent.Result{
		ChildSessionID: "child-1", Usage: ai.Usage{InputTokens: 100, OutputTokens: 10},
	})
	assert.Equal(t, ai.Usage{InputTokens: 100, OutputTokens: 10}, cold)

	resumed := spawner.usageDelta(0, "child-1", subagent.Result{
		ChildSessionID: "child-2", Usage: ai.Usage{InputTokens: 160, OutputTokens: 16, ReasoningTokens: 3},
	})
	assert.Equal(t, ai.Usage{InputTokens: 60, OutputTokens: 6, ReasoningTokens: 3}, resumed)

	// A resume whose source this process never observed reports the child's own
	// cumulative usage: the panel has no baseline to subtract.
	unknown := spawner.usageDelta(0, "child-9", subagent.Result{
		ChildSessionID: "child-3", Usage: ai.Usage{InputTokens: 20, OutputTokens: 2},
	})
	assert.Equal(t, ai.Usage{InputTokens: 20, OutputTokens: 2}, unknown)

	// A cold re-spawn is its own run, not a continuation.
	fresh := spawner.usageDelta(0, "", subagent.Result{
		ChildSessionID: "child-4", Usage: ai.Usage{InputTokens: 5, OutputTokens: 1},
	})
	assert.Equal(t, ai.Usage{InputTokens: 5, OutputTokens: 1}, fresh)

	// Only the gatekeeper can be resumed, so only it carries a baseline.
	second := spawner.usageDelta(1, "child-4", subagent.Result{Usage: ai.Usage{InputTokens: 7, OutputTokens: 1}})
	assert.Equal(t, ai.Usage{InputTokens: 7, OutputTokens: 1}, second)

	// An impossible provider report can never go negative.
	clamped := spawner.usageDelta(0, "child-4", subagent.Result{
		ChildSessionID: "child-5", Usage: ai.Usage{InputTokens: 1, OutputTokens: 1},
	})
	assert.Equal(t, ai.Usage{InputTokens: 0, OutputTokens: 0}, clamped)
}

// TestRuntimeGoalBaselineCommitIsCaptured pins the Goal's baseline: a repository
// records its commit at Goal creation, and a workspace without one records
// nothing rather than failing the Goal.
func TestRuntimeGoalBaselineCommitIsCaptured(t *testing.T) {
	t.Parallel()

	t.Run("a repository records its commit", func(t *testing.T) {
		t.Parallel()

		model := newGoalSpawnedModel(
			runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
			runtimeTextResponse("candidate complete"),
		)
		runtime := openFullAccessSpawnedGoalTestRuntime(t, model)
		writeGoalSpawnedProof(t, runtime)
		initTestGitRepository(t, runtime)

		// A change after the baseline is what the packet must show, and an
		// untracked file is part of the changed-file list.
		require.NoError(t, os.WriteFile(
			filepath.Join(runtime.workspace.Root(), "proof.txt"),
			[]byte("actual implementation\nchanged after the baseline\n"), 0o600,
		))
		require.NoError(t, os.WriteFile(
			filepath.Join(runtime.workspace.Root(), "added.txt"), []byte("new file\n"), 0o600,
		))

		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
		require.NoError(t, err)

		runtime.goalControl.mu.Lock()
		baseline := runtime.goalControl.ledger.BaselineCommit
		runtime.goalControl.mu.Unlock()
		assert.Len(t, baseline, 40)

		// The packet's change evidence is captured from that baseline.
		for _, task := range model.skepticTasks() {
			assert.Contains(t, task, "CHANGES_FILE:\ndiff --git a/proof.txt b/proof.txt")
			assert.Contains(t, task, "+changed after the baseline")
			assert.Contains(t, task, "CHANGED_FILES:\n- added.txt\n- proof.txt")
		}
	})

	t.Run("a workspace without a repository records nothing", func(t *testing.T) {
		t.Parallel()

		model := newGoalSpawnedModel(
			runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
			runtimeTextResponse("candidate complete"),
		)
		runtime := openSpawnedGoalTestRuntime(t, model)
		writeGoalSpawnedProof(t, runtime)

		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
		require.NoError(t, err)

		runtime.goalControl.mu.Lock()
		baseline := runtime.goalControl.ledger.BaselineCommit
		runtime.goalControl.mu.Unlock()
		assert.Empty(t, baseline)
		require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())

		for _, task := range model.skepticTasks() {
			assert.Contains(t, task, "CHANGES_FILE: (unavailable)")
			assert.Contains(t, task, "CHANGED_FILES:\n(none captured)")
		}
	})
}

// TestRuntimeGoalSpawnedPanelReportIsBounded pins the diagnostic report's bound:
// a runaway skeptic's details are cut at 512 KiB with an elision marker, and the
// report never gates the decision.
func TestRuntimeGoalSpawnedPanelReportIsBounded(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", goalVerifierPanelMaxBytes)

	model := newGoalSpawnedModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.verdicts = []string{`{"refuted":false,"evidence":"the recorded evidence is sufficient",` +
		`"confidence":"high","blocking":"none","findings":[],"details_md":"` + huge + `"}`}
	runtime := openSpawnedGoalTestRuntime(t, model)
	writeGoalSpawnedProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)

	report := goalVerifierPanelReport(t, runtime, state.ID)
	assert.LessOrEqual(t, len(report), goalVerifierPanelMaxBytes+len("\n\n[the panel report is truncated at the diagnostic bound]\n"))
	assert.Contains(t, report, "[the panel report is truncated at the diagnostic bound]")
}

// initTestGitRepository turns a test workspace into a Git repository with one
// commit, so the Goal can resolve a baseline. The fixture runs Git through the
// Runtime's own executor, because no code outside the execution boundary
// starts a process.
func initTestGitRepository(t *testing.T, runtime *Runtime) {
	t.Helper()

	for _, arguments := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Pips Test"},
		{"config", "user.email", "pips@example.invalid"},
		{"add", "--all", "--", "."},
		{"-c", "user.name=Pips Test", "-c", "user.email=pips@example.invalid", "commit", "--quiet", "-m", "initial"},
	} {
		runGitTestCommand(t, runtime, arguments...)
	}
}
