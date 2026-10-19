//nolint:wsl_v5 // Concurrent execution fixtures keep actions beside assertions.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRunner is an in-package Runner double. It never calls a model; the
// manager's lifecycle, admission, and journal behavior are what these tests
// exercise.
type stubRunner struct {
	once      sync.Once
	onStart   func()
	onEvent   func(context.Context, agent.Event)
	child     *session.Handle
	block     bool
	value     any
	text      string
	toolCalls int
	run       *agent.RunResult
	err       error
	closeErr  error
	closes    atomic.Int32
	// skipTranscript leaves the child Session with no model-visible message,
	// which is the shape a resume source must not accept.
	skipTranscript bool
	// deadlineSeen receives whether the spawn wrapped the run in a deadline.
	// It is buffered, so recording never blocks the run.
	deadlineSeen chan bool
}

func (r *stubRunner) Run(ctx context.Context, task string) (RunnerResult, error) {
	if r.deadlineSeen != nil {
		_, seen := ctx.Deadline()
		r.deadlineSeen <- seen
	}
	if r.onStart != nil {
		r.once.Do(r.onStart)
	}
	if r.child != nil && !r.skipTranscript {
		// Persist a minimal transcript so a completed child stays inspectable.
		_, _ = r.child.Session().AppendMessage(ai.UserText(task), nil)
		_, _ = r.child.Session().AppendMessage(ai.AssistantText(r.text), nil)
	}
	if r.onEvent != nil {
		if event, err := agent.NewEvent(
			agent.RunMetadata{RunID: "stub-run", Agent: "subagent/stub"},
			time.Now().UTC(),
			agent.RunStarted{},
		); err == nil {
			r.onEvent(ctx, event)
		}
	}
	if r.block {
		<-ctx.Done()

		return RunnerResult{}, ctx.Err()
	}
	run := r.run
	if run == nil {
		run = &agent.RunResult{Stop: agent.StopEndTurn, Turns: 1}
	}

	return RunnerResult{Value: r.value, Text: r.text, ToolCalls: r.toolCalls, Run: run}, r.err
}

func (r *stubRunner) Close(context.Context) error {
	r.closes.Add(1)

	return r.closeErr
}

// stubDispatcher is an in-package Dispatcher double that compiles a builtin
// plan and opens a stub runner. It stands in for the coding package's control
// scope, which cannot be imported from this package.
type stubDispatcher struct {
	limits    Limits
	ids       []string
	onCompile func(Request)
	onOpen    func(DispatchInput)
	newRunner func() *stubRunner
}

func (d stubDispatcher) AgentIDs() []string { return d.ids }

func (d stubDispatcher) Compile(_ context.Context, request Request) (ExecutionPlan, error) {
	if d.onCompile != nil {
		d.onCompile(request)
	}
	identity, err := BuiltinIdentity(Role(request.AgentID))
	if err != nil {
		return ExecutionPlan{}, fmt.Errorf("%w: unknown subagent type %q", ErrInvalid, request.AgentID)
	}
	output, err := NewOutputContract(OutputFormatText, "subagent-"+request.AgentID+"-result", nil)
	if err != nil {
		return ExecutionPlan{}, err
	}
	instructions := "stub instructions for " + request.AgentID

	return ExecutionPlan{
		Schema:             ExecutionPlanSchema,
		Identity:           identity,
		Delivery:           request.Delivery,
		Model:              "openai/subagent-test",
		Instructions:       instructions,
		InstructionsDigest: InstructionsDigest(instructions),
		Isolation:          request.Isolation,
		Cwd:                request.Cwd,
		ResumeFrom:         request.ResumeFrom,
		Limits:             d.limits,
		Output:             output,
	}, nil
}

func (d stubDispatcher) Open(_ context.Context, input DispatchInput) (Runner, error) {
	if d.onOpen != nil {
		d.onOpen(input)
	}
	runner := &stubRunner{value: "stub summary", text: "stub summary"}
	if d.newRunner != nil {
		runner = d.newRunner()
	}
	runner.onEvent = input.OnEvent
	runner.child = input.Child

	return runner, nil
}

type failingOpenDispatcher struct {
	plan ExecutionPlan
}

func (d failingOpenDispatcher) AgentIDs() []string { return []string{d.plan.Identity.ID} }

func (d failingOpenDispatcher) Compile(context.Context, Request) (ExecutionPlan, error) {
	return d.plan.Clone(), nil
}

func (failingOpenDispatcher) Open(context.Context, DispatchInput) (Runner, error) {
	return nil, errTestBindingFailed
}

var errTestBindingFailed = errors.New("test binding failed")

type testModel struct {
	capabilities *ai.Capabilities
}

func (*testModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("test model must not be called")
}

func (*testModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("test model must not be called"))
	}
}

func (*testModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*testModel) ModelID() string       { return "subagent-test" }
func (m *testModel) Capabilities() ai.Capabilities {
	if m.capabilities != nil {
		return *m.capabilities
	}

	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

type managerFixture struct {
	root       string
	manager    *Manager
	repository *session.Repository
	parent     *session.Handle
	tree       *workspace.Tree
	dispatcher stubDispatcher
	started    chan struct{}
	startedOne sync.Once
}

func (f *managerFixture) start(
	ctx context.Context,
	request Request,
	observer Observer,
) (*Execution, error) {
	return f.manager.StartWithDispatcher(ctx, request, observer, f.dispatcher)
}

func (f *managerFixture) blockChildren() {
	f.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{block: true, onStart: func() { f.startedOne.Do(func() { close(f.started) }) }}
	}
}

func newManagerFixture(t *testing.T, model ai.LanguageModel) *managerFixture {
	t.Helper()

	return newManagerFixtureWithOptions(t, model, ExecutionOptions{})
}

func newManagerFixtureWithOptions(
	t *testing.T,
	model ai.LanguageModel,
	options ExecutionOptions,
) *managerFixture {
	t.Helper()

	return newManagerFixtureWithConfig(t, model, options, nil)
}

func newManagerFixtureWithConfig(
	t *testing.T,
	model ai.LanguageModel,
	options ExecutionOptions,
	configure func(*Config),
) *managerFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(root, 0o700))
	value, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(value)
	require.NoError(t, err)
	repository, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: value.Identity().Key(), WorkspacePath: value.Root(),
	})
	require.NoError(t, err)
	managerConfig := Config{
		Repository: repository, Parent: parent, Tree: tree, Model: model,
		Options: options,
	}
	if configure != nil {
		configure(&managerConfig)
	}
	manager, err := New(managerConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A background child may still be queued for a slot when the test ends,
		// so the close budget stays generous; it costs nothing on the happy path.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		require.NoError(t, manager.Close(ctx))
		require.NoError(t, parent.Close())
		require.NoError(t, tree.Close())
	})

	return &managerFixture{
		root: root, manager: manager, repository: repository, parent: parent, tree: tree,
		dispatcher: stubDispatcher{limits: manager.limits},
		started:    make(chan struct{}),
	}
}

func TestSummarizeToolActivityExposesOnlyBoundedSemanticFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call ai.ToolCallPart
		want ActivitySummary
	}{
		{
			name: "read",
			call: ai.ToolCallPart{Name: readToolName, Args: ai.JSON(`{"path":"internal/coding/runtime.go"}`)},
			want: ActivitySummary{Action: ActivityActionRead, Target: "internal/coding/runtime.go"},
		},
		{
			name: "grep",
			call: ai.ToolCallPart{Name: searchToolName, Args: ai.JSON(`{"pattern":"func main","path":"cmd"}`)},
			want: ActivitySummary{Action: ActivityActionSearch, Target: "func main in cmd"},
		},
		{
			name: "glob",
			call: ai.ToolCallPart{Name: globToolName, Args: ai.JSON(`{"pattern":"**/*.go"}`)},
			want: ActivitySummary{Action: ActivityActionGlob, Target: "**/*.go"},
		},
		{
			name: "ls",
			call: ai.ToolCallPart{Name: listToolName, Args: ai.JSON(`{}`)},
			want: ActivitySummary{Action: ActivityActionList, Target: "."},
		},
		{
			name: "absolute path is dropped",
			call: ai.ToolCallPart{Name: readToolName, Args: ai.JSON(`{"path":"/etc/passwd"}`)},
			want: ActivitySummary{},
		},
		{
			name: "escaping path is dropped",
			call: ai.ToolCallPart{Name: readToolName, Args: ai.JSON(`{"path":"../../secret"}`)},
			want: ActivitySummary{},
		},
		{
			name: "unknown tool has no projection",
			call: ai.ToolCallPart{Name: "shell", Args: ai.JSON(`{"command":"rm -rf /"}`)},
			want: ActivitySummary{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, summarizeToolActivity(test.call))
		})
	}
}

func TestSummarizeToolActivityBoundsMultibyteTarget(t *testing.T) {
	t.Parallel()

	target := strings.Repeat("路径", 400)
	summary := summarizeToolActivity(ai.ToolCallPart{
		Name: readToolName, Args: ai.JSON(`{"path":"` + target + `"}`),
	})
	assert.LessOrEqual(t, len(summary.Target), 512)
	assert.LessOrEqual(t, len([]rune(summary.Target)), 160)
	assert.True(t, strings.HasSuffix(summary.Target, "…"))
}

func TestRunTrackerProjectsDefensiveLiveActivity(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.July, 23, 10, 0, 0, 0, time.UTC)
	tracker := newRunTracker(startedAt, DefaultLimits().MaxActivityTools)
	call := ai.ToolCallPart{
		ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"internal/coding/runtime.go"}`),
	}

	events := []agent.Event{
		mustAgentEvent(t, startedAt, agent.RunStarted{}),
		mustAgentEvent(t, startedAt.Add(time.Second), agent.TurnStarted{Turn: 1}),
		mustAgentEvent(t, startedAt.Add(2*time.Second), agent.ToolStarted{Turn: 1, Call: call}),
		mustAgentEvent(t, startedAt.Add(3*time.Second), agent.ToolUpdated{
			Turn: 1, Call: call, Update: []ai.Part{ai.TextPart{Text: "reading"}},
		}),
	}
	for _, event := range events {
		trackChildEvent(tracker, event)
	}

	activity := tracker.activitySnapshot()
	assert.Equal(t, ActivityPhaseWorking, activity.Phase)
	assert.Equal(t, "run-1", activity.RunID)
	assert.Equal(t, 1, activity.Turn)
	assert.Equal(t, uint64(5), activity.Revision)
	require.Len(t, activity.Tools, 1)
	assert.Equal(t, ToolStatusRunning, activity.Tools[0].Status)
	assert.Equal(t, "reading", partsText(activity.Tools[0].Update))

	activity.Tools[0].Call.Args[0] = '['
	activity.Tools[0].Update[0] = ai.TextPart{Text: "mutated"}

	second := tracker.activitySnapshot()
	assert.JSONEq(t, `{"path":"internal/coding/runtime.go"}`, string(second.Tools[0].Call.Args))
	assert.Equal(t, "reading", partsText(second.Tools[0].Update))
}

func mustAgentEvent(t *testing.T, occurredAt time.Time, payload agent.EventPayload) agent.Event {
	t.Helper()

	event, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run-1", Agent: "subagent"},
		occurredAt,
		payload,
	)
	require.NoError(t, err)

	return event
}

func TestRunTrackerBoundsActivityWithoutFreezingCurrentAction(t *testing.T) {
	t.Parallel()

	tracker := newRunTracker(time.Now().UTC(), 1)
	for index, target := range []string{"first.txt", "second.txt"} {
		call := ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(fmt.Sprintf(`{"path":%q}`, target)),
		}
		trackChildEvent(tracker, mustAgentEvent(
			t,
			time.Now().UTC(),
			agent.ToolStarted{Turn: index + 1, Call: call},
		))
	}

	snapshot, activity := tracker.snapshot()
	require.Len(t, activity.Tools, 1)
	assert.Equal(t, "first.txt", summarizeToolActivity(activity.Tools[0].Call).Target)
	assert.Equal(t, "second.txt", snapshot.activity.Target)
	assert.Equal(t, ActivityPhaseWorking, activity.Phase)
}

func TestManagerIsSerialCancelableAndLeavesWorkspaceUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{
		MaxConcurrent: 1, LimitBehavior: LimitBehaviorFail,
	})
	fixture.blockChildren()
	path := filepath.Join(fixture.root, "sentinel.txt")
	require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0o600))

	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Wait."}, nil,
	)
	require.NoError(t, err)
	<-fixture.started

	_, err = fixture.start(
		t.Context(), Request{AgentID: "plan", Task: "Second."}, nil,
	)
	require.ErrorIs(t, err, ErrConcurrentLimit)

	execution.Cancel()
	result, err := execution.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, OutcomeCanceled, result.Outcome)
	assert.Equal(t, "canceled", result.Code)

	content, err := fs.ReadFile(fixture.tree.FileSystem(), "sentinel.txt")
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(content))
}

func TestManagerEnforcesConcurrentCapacity(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{
		MaxConcurrent: 4, LimitBehavior: LimitBehaviorFail,
	})
	fixture.blockChildren()
	executions := make([]*Execution, 0, 4)
	for index := range 4 {
		execution, err := fixture.start(t.Context(), Request{
			AgentID: "explore", Task: fmt.Sprintf("Wait %d.", index),
		}, nil)
		require.NoError(t, err)
		executions = append(executions, execution)
	}

	_, err := fixture.start(
		t.Context(), Request{AgentID: "plan", Task: "Over capacity."}, nil,
	)
	require.ErrorIs(t, err, ErrConcurrentLimit)

	for _, execution := range executions {
		execution.Cancel()
	}
	for _, execution := range executions {
		result, waitErr := execution.Wait(t.Context())
		require.ErrorIs(t, waitErr, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
	}
}

func TestManagerObservesBoundedAdmissionDecisions(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		events []AdmissionEvent
	)
	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{MaxConcurrent: 2, LimitBehavior: LimitBehaviorFail},
		func(config *Config) {
			config.AdmissionObserver = func(_ context.Context, event AdmissionEvent) {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
		},
	)
	fixture.blockChildren()
	executions := make([]*Execution, 0, 2)
	for range 2 {
		execution, err := fixture.start(t.Context(), Request{
			AgentID: "explore", Task: "Wait without disclosing this task.",
		}, nil)
		require.NoError(t, err)
		executions = append(executions, execution)
	}
	_, err := fixture.start(t.Context(), Request{
		AgentID: "plan", Task: "Rejected task must not reach telemetry.",
	}, nil)
	require.ErrorIs(t, err, ErrConcurrentLimit)

	mu.Lock()
	observed := slices.Clone(events)
	mu.Unlock()
	require.Len(t, observed, 3)
	for _, event := range observed {
		assert.Equal(t, DeliveryForeground, event.Delivery)
		assert.Zero(t, event.DelegationDepth)
	}
	assert.Equal(t, AdmissionOutcomeAccepted, observed[0].Outcome)
	assert.Empty(t, observed[0].Reason)
	assert.Equal(t, AdmissionOutcomeAccepted, observed[1].Outcome)
	assert.Equal(t, AdmissionOutcomeRejected, observed[2].Outcome)
	assert.Equal(t, AdmissionReasonCapacity, observed[2].Reason)

	for _, execution := range executions {
		execution.Cancel()
	}
	for _, execution := range executions {
		_, waitErr := execution.Wait(t.Context())
		require.ErrorIs(t, waitErr, context.Canceled)
	}
}

func TestManagerAdmissionObservationIsPanicIsolated(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{},
		func(config *Config) {
			config.AdmissionObserver = func(context.Context, AdmissionEvent) {
				panic("private observer failure")
			}
		},
	)
	execution, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Complete despite observer failure.",
	}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
}

func TestManagerObservesCapacityAndInvalidAdmissionReasons(t *testing.T) {
	t.Parallel()

	events := make([]AdmissionEvent, 0, 3)
	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{MaxConcurrent: 1, LimitBehavior: LimitBehaviorFail},
		func(config *Config) {
			config.AdmissionObserver = func(_ context.Context, event AdmissionEvent) {
				events = append(events, event)
			}
		},
	)
	permit, err := fixture.manager.reserveStart(t.Context(), Request{
		Delivery: DeliveryForeground,
	})
	require.NoError(t, err)
	_, err = fixture.manager.reserveStart(t.Context(), Request{
		Delivery: DeliveryForeground,
	})
	require.ErrorIs(t, err, ErrConcurrentLimit)
	fixture.manager.finishStarting(permit, false)
	_, err = fixture.manager.reserveStart(t.Context(), Request{
		Delivery: Delivery("unexpected"),
	})
	require.ErrorIs(t, err, ErrInvalid)

	require.Len(t, events, 3)
	assert.Equal(t, AdmissionOutcomeAccepted, events[0].Outcome)
	assert.Equal(t, AdmissionReasonCapacity, events[1].Reason)
	assert.Equal(t, AdmissionReasonInvalid, events[2].Reason)
}

func TestManagerObservesSpawnLimitAndClosedRejections(t *testing.T) {
	t.Parallel()

	events := make([]AdmissionEvent, 0, 3)
	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{MaxConcurrent: 1, MaxSpawnedPerRootInteraction: 1},
		func(config *Config) {
			config.AdmissionObserver = func(_ context.Context, event AdmissionEvent) {
				events = append(events, event)
			}
		},
	)
	request := Request{
		AgentID: "explore", Task: "Run once.", Delivery: DeliveryBackground,
		Ownership: Ownership{
			ParentInteractionID: "root-1", ParentRunID: "parent-run",
			ParentToolCallID: "parent-call", RootInteractionID: "root-1",
		},
	}
	execution, err := fixture.start(t.Context(), request, nil)
	require.NoError(t, err)
	_, err = execution.Wait(t.Context())
	require.NoError(t, err)
	_, err = fixture.start(t.Context(), request, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
	require.NoError(t, fixture.manager.Close(t.Context()))
	_, err = fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Closed.",
	}, nil)
	require.ErrorIs(t, err, ErrClosed)

	require.Len(t, events, 3)
	assert.Equal(t, AdmissionOutcomeAccepted, events[0].Outcome)
	assert.Equal(t, DeliveryBackground, events[0].Delivery)
	assert.Equal(t, AdmissionReasonSpawnLimit, events[1].Reason)
	assert.Equal(t, AdmissionReasonClosed, events[2].Reason)
}

// TestManagerSpawnsBeyondTheOldPerRootDefault pins Phase M: the per-root spawn
// count is unlimited by default, so one root spawns far more than the old
// built-in default of 8. An explicit positive cap still refuses.
func TestManagerSpawnsBeyondTheOldPerRootDefault(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	request := Request{
		AgentID: "explore", Task: "Run.", Delivery: DeliveryBackground,
		Ownership: Ownership{
			ParentInteractionID: "root-1", ParentRunID: "parent-run",
			ParentToolCallID: "parent-call", RootInteractionID: "root-1",
		},
	}
	for index := range 12 {
		execution, err := fixture.start(t.Context(), request, nil)
		require.NoError(t, err, "spawn %d", index)
		_, err = execution.Wait(t.Context())
		require.NoError(t, err)
	}
	_, spawned := fixture.manager.admission.snapshot()
	assert.Empty(t, spawned, "an unlimited cap keeps no per-root count")
}

func TestManagerExplicitSpawnCapStillRefuses(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(
		t, &testModel{}, ExecutionOptions{MaxConcurrent: 1, MaxSpawnedPerRootInteraction: 2},
	)
	request := Request{
		AgentID: "explore", Task: "Run.", Delivery: DeliveryBackground,
		Ownership: Ownership{
			ParentInteractionID: "root-1", ParentRunID: "parent-run",
			ParentToolCallID: "parent-call", RootInteractionID: "root-1",
		},
	}
	for index := range 2 {
		execution, err := fixture.start(t.Context(), request, nil)
		require.NoError(t, err, "spawn %d", index)
		_, err = execution.Wait(t.Context())
		require.NoError(t, err)
	}
	_, err := fixture.start(t.Context(), request, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestManagerConcurrentCloseReleasesEveryAdmissionPermit(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{MaxConcurrent: 2})
	fixture.blockChildren()
	executions := make([]*Execution, 0, 2)
	for index := range 2 {
		execution, err := fixture.start(t.Context(), Request{
			AgentID: "explore", Task: fmt.Sprintf("Wait %d.", index),
		}, nil)
		require.NoError(t, err)
		executions = append(executions, execution)
	}
	<-fixture.started

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- fixture.manager.Close(context.Background())
		}()
	}
	close(start)
	for range 2 {
		require.NoError(t, <-errs)
	}
	for _, execution := range executions {
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
	}
	active, _ := fixture.manager.admission.snapshot()
	assert.Zero(t, active)
}

func TestManagerBindingFailureReleasesPermitAndSpawnReservation(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(
		t,
		&testModel{},
		ExecutionOptions{MaxConcurrent: 1, MaxSpawnedPerRootInteraction: 1},
	)
	output, err := NewOutputContract(OutputFormatText, "subagent-explore-result", nil)
	require.NoError(t, err)
	plan := ExecutionPlan{
		Schema: ExecutionPlanSchema, Delivery: DeliveryBackground,
		Model: "openai/subagent-test", Instructions: "instructions",
		InstructionsDigest: InstructionsDigest("instructions"),
		Limits:             fixture.manager.limits, Output: output,
		Identity: AgentIdentity{
			Schema: AgentIdentitySchema, ID: "binding-failure", Kind: AgentKindCustom,
			Name: "Binding failure", DefinitionSchema: "pips.agent/v1alpha1",
			DefinitionDigest: strings.Repeat("a", 64), DefinitionSource: "test",
		},
	}
	dispatcher := failingOpenDispatcher{plan: plan}
	request := Request{
		AgentID: "binding-failure", Task: "Fail before execution ownership.",
		Delivery: DeliveryBackground,
		Ownership: Ownership{
			ParentInteractionID: "root-1", ParentRunID: "parent-run",
			ParentToolCallID: "parent-call", RootInteractionID: "root-1",
		},
	}
	for range 2 {
		_, err = fixture.manager.StartWithDispatcher(t.Context(), request, nil, dispatcher)
		require.ErrorIs(t, err, errTestBindingFailed)
		active, spawned := fixture.manager.admission.snapshot()
		assert.Zero(t, active)
		assert.Empty(t, spawned)
	}
}

func TestManagerWaitReconstructsCompletedChild(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	completed, err := execution.Wait(t.Context())
	require.NoError(t, err)

	reconstructed, err := fixture.manager.Wait(t.Context(), completed.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, completed.ChildSessionID, reconstructed.ChildSessionID)
	assert.Equal(t, OutcomeSucceeded, reconstructed.Outcome)
	assert.Equal(t, "stub summary", reconstructed.Value)
}

func TestManagerCancelRootOnlyCancelsMatchingBackgroundChildren(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{MaxConcurrent: 4})
	fixture.blockChildren()
	start := func(root, callID string) *Execution {
		execution, err := fixture.start(t.Context(), Request{
			AgentID: "explore", Task: "Wait.", Delivery: DeliveryBackground,
			Ownership: Ownership{
				ParentInteractionID: root, ParentRunID: "parent-run",
				ParentToolCallID: callID, RootInteractionID: root,
			},
		}, nil)
		require.NoError(t, err)

		return execution
	}

	first := start("root-1", "call-1")
	second := start("root-1", "call-2")
	other := start("root-2", "call-3")
	<-fixture.started

	assert.Equal(t, 2, fixture.manager.CancelRoot("root-1"))
	for _, execution := range []*Execution{first, second} {
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
	}
	select {
	case <-other.done:
		require.FailNow(t, "unrelated child was canceled")
	case <-time.After(25 * time.Millisecond):
	}
	require.NoError(t, fixture.manager.Cancel(t.Context(), other.child.Metadata().ID))
	_, err := other.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
}

func TestManagerLifecycleAddsContextAndContinuesChild(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		starts  []LifecycleStart
		stops   []LifecycleStop
		opened  []DispatchInput
		stopped bool
	)
	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{},
		func(config *Config) {
			config.Lifecycle = Lifecycle{
				BeforeStart: func(_ context.Context, info LifecycleStart) string {
					mu.Lock()
					starts = append(starts, info)
					mu.Unlock()

					return "child lifecycle context"
				},
				BeforeStop: func(_ context.Context, info LifecycleStop) LifecycleStopDecision {
					mu.Lock()
					stops = append(stops, info)
					shouldContinue := !stopped
					stopped = true
					mu.Unlock()
					if shouldContinue {
						return LifecycleStopDecision{Continue: true, Reason: "inspect one more detail"}
					}

					return LifecycleStopDecision{}
				},
			}
		},
	)
	fixture.dispatcher.onOpen = func(input DispatchInput) {
		mu.Lock()
		opened = append(opened, input)
		mu.Unlock()
	}

	execution, err := fixture.start(t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil)
	require.NoError(t, err)
	_, err = execution.Wait(t.Context())
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, starts, 1)
	assert.Equal(t, "Inspect.", starts[0].Task)
	assert.Equal(t, "explore", starts[0].Identity.ID)
	require.Len(t, opened, 1)
	assert.Equal(t, "child lifecycle context", opened[0].SystemSuffix)
	require.Len(t, stops, 2)
	assert.False(t, stops[0].StopHookActive)
	assert.True(t, stops[1].StopHookActive)
	assert.Equal(t, "stub summary", stops[0].LastAssistantMessage)
}

func TestSpawnToolSurvivesCallContextAndEnforcesRootLimit(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{
		MaxConcurrent: 4, MaxSpawnedPerRootInteraction: 2,
	})
	fixture.blockChildren()
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, fixture.dispatcher)

	callCtx, cancelCall := context.WithCancel(t.Context())
	parts, err := tool.Exec(callCtx, agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"Wait for cancellation.","description":"wait"}`),
	})
	require.NoError(t, err)
	cancelCall()

	var first spawnResult
	require.NoError(t, json.Unmarshal([]byte(firstJSONObject(partsText(parts))), &first))
	assert.Equal(t, SpawnResultSchema, first.Schema)
	firstExecution := fixture.manager.activeExecution(first.AgentID)
	require.NotNil(t, firstExecution)
	select {
	case <-firstExecution.done:
		require.FailNow(t, "background execution was canceled with its Tool context")
	case <-time.After(25 * time.Millisecond):
	}

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-2", Name: ToolName,
		Args: ai.JSON(`{"prompt":"Wait too.","description":"wait too"}`),
	})
	require.NoError(t, err)
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-3", Name: ToolName,
		Args: ai.JSON(`{"prompt":"Exceed the root limit.","description":"exceed"}`),
	})
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestSpawnToolSchemaAndEnum(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	dispatcher := fixture.dispatcher
	dispatcher.ids = []string{"go-checker", "reviewer"}
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{}, nil, dispatcher)
	declaration := tool.Decl()
	assert.Equal(t, ToolName, declaration.Name)
	assert.Equal(t, []string{"prompt", "description"}, declaration.InputSchema.Required)
	assert.Equal(t, []any{"go-checker", "reviewer"}, declaration.InputSchema.Properties["subagent_type"].Enum)
	assert.Contains(t, declaration.Description, "reports back")
	assert.Contains(t, declaration.Description, "resume_from")
	assert.Contains(t, declaration.Description, "BEFORE concluding")
	// A persona is resolved inside the child from the agent type name, so it is
	// deliberately absent from the spawn surface.
	_, hasPersona := declaration.InputSchema.Properties["persona"]
	assert.False(t, hasPersona)
	assert.Contains(t, declaration.InputSchema.Properties, "model")

	// Builtins are never advertised: with no custom agents the enum is absent.
	plain := fixture.manager.SpawnToolForDispatcher(Ownership{}, nil, fixture.dispatcher)
	assert.Empty(t, plain.Decl().InputSchema.Properties["subagent_type"].Enum)
}

func TestSpawnToolDefaultsSentinelsAndUnsupportedArguments(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	var compiled []Request
	dispatcher := fixture.dispatcher
	dispatcher.onCompile = func(request Request) { compiled = append(compiled, request) }
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, dispatcher)

	// Background is the default, an omitted type resolves to general-purpose,
	// and sentinels are treated as absent. The lenient bool accepts a string.
	_, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"do it","description":"do it","subagent_type":"null","run_in_background":"true"}`),
	})
	require.NoError(t, err)
	require.Len(t, compiled, 1)
	assert.Equal(t, "general-purpose", compiled[0].AgentID)
	assert.Equal(t, DeliveryBackground, compiled[0].Delivery)

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-2", Name: ToolName,
		Args: ai.JSON(`{"prompt":"do it","description":"do it","subagent_type":"explore","run_in_background":0}`),
	})
	require.NoError(t, err)
	require.Len(t, compiled, 2)
	assert.Equal(t, "explore", compiled[1].AgentID)
	assert.Equal(t, DeliveryForeground, compiled[1].Delivery)

	for _, args := range []string{
		`{"prompt":"x","description":"y","resume_from":"abc"}`,
		`{"prompt":"x","description":"y","cwd":"/pips/definitely/missing/directory"}`,
		`{"prompt":"x","description":"y","isolation":"container"}`,
		`{"prompt":"x","description":"y","run_in_background":"maybe"}`,
		`{"prompt":"x"}`,
		`{"prompt":"x","description":"y","unknown":true}`,
	} {
		_, err = tool.Exec(t.Context(), agent.ToolCall{
			ID: "spawn-3", Name: ToolName, Args: ai.JSON(args),
		})
		require.ErrorIs(t, err, ErrInvalid, args)
	}

	// isolation "none" is accepted.
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-4", Name: ToolName,
		Args: ai.JSON(`{"prompt":"x","description":"y","isolation":"none"}`),
	})
	require.NoError(t, err)

	// An explicit worktree isolation and an existing cwd are accepted and
	// forwarded; the dispatcher resolves the definition default and the child
	// binding owns the directory.
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-5", Name: ToolName,
		Args: ai.JSON(`{"prompt":"x","description":"y","isolation":"worktree"}`),
	})
	require.NoError(t, err)
	require.NotEmpty(t, compiled)
	assert.Equal(t, IsolationWorktree, compiled[len(compiled)-1].Isolation)

	directory := t.TempDir()
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-6", Name: ToolName,
		Args: ai.JSON(fmt.Sprintf(`{"prompt":"x","description":"y","cwd":%q}`, directory)),
	})
	require.NoError(t, err)
	assert.Equal(t, directory, compiled[len(compiled)-1].Cwd)

	// An explicit model is accepted and forwarded; the dispatcher owns the
	// model precedence chain and the fallback to the parent model.
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-7", Name: ToolName,
		Args: ai.JSON(`{"prompt":"x","description":"y","model":"openai/gpt"}`),
	})
	require.NoError(t, err)
	require.NotEmpty(t, compiled)
	assert.Equal(t, "openai/gpt", compiled[len(compiled)-1].Model)
}

func TestSpawnToolUnknownTypeFails(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, fixture.dispatcher)

	_, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"x","description":"y","subagent_type":"nope"}`),
	})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "unknown subagent type")
}

func TestSpawnToolForegroundReturnsEnvelopeAndMeta(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, fixture.dispatcher)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"do it","description":"do it","run_in_background":false}`),
	})
	require.NoError(t, err)
	text := partsText(parts)
	assert.Contains(t, text, `"schema":"`+ResultSchema+`"`)
	assert.Contains(t, text, `"outcome":"succeeded"`)
	assert.Contains(t, text, "<subagent_meta>id=")
	assert.Contains(t, text, "tool_calls=0")
	assert.Contains(t, text, "<subagent_result>")
	assert.Contains(t, text, `resume_from="`)
	assert.Contains(t, text, "stub summary")
}

func TestSpawnToolBackgroundReturnsIDImmediately(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	fixture.blockChildren()
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, fixture.dispatcher)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"do it","description":"do it"}`),
	})
	require.NoError(t, err)
	var result spawnResult
	require.NoError(t, json.Unmarshal([]byte(firstJSONObject(partsText(parts))), &result))
	assert.Equal(t, SpawnResultSchema, result.Schema)
	assert.NotEmpty(t, result.AgentID)
	assert.Equal(t, "general-purpose", result.ProfileID)
	assert.Contains(t, partsText(parts), "started in background")
}

func TestManagerInspectUsesPersistedLimitsAndRejectsOtherParent(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	require.NoError(t, fixture.manager.Close(t.Context()))

	narrow := DefaultLimits()
	narrow.MaxResultBytes = 1
	narrow.MaxFieldBytes = 1
	reopened, err := New(Config{
		Repository: fixture.repository, Parent: fixture.parent, Tree: fixture.tree,
		Model: &testModel{}, Options: ExecutionOptions{Limits: narrow},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
	detail, err := reopened.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, "stub summary", detail.Result)

	otherParent, err := fixture.repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   fixture.parent.Metadata().WorkspaceID,
		WorkspacePath: fixture.parent.Metadata().WorkspacePath,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherParent.Close()) })

	other, err := New(Config{
		Repository: fixture.repository, Parent: otherParent, Tree: fixture.tree, Model: &testModel{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close(context.Background())) })
	_, err = other.Inspect(t.Context(), result.ChildSessionID)
	require.ErrorIs(t, err, ErrInvalid)
}

// TestManagerInstallsSpawnDeadlineOnlyForAnExplicitDuration pins Phase L's
// wall-clock behaviour: the default child policy carries no MaxDuration, so the
// spawn installs no deadline, while an explicit duration still does. The
// deadline is observed inside Run rather than waited on.
func TestManagerInstallsSpawnDeadlineOnlyForAnExplicitDuration(t *testing.T) {
	t.Parallel()

	unboundedSeen := make(chan bool, 1)
	unbounded := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{Limits: DefaultLimits()})
	unbounded.dispatcher.limits = DefaultLimits()
	unbounded.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{deadlineSeen: unboundedSeen, text: "stub summary", value: "stub summary"}
	}

	execution, err := unbounded.start(t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.False(t, <-unboundedSeen, "the default policy installs no wall-clock deadline")

	boundedSeen := make(chan bool, 1)
	bounded := DefaultLimits()
	bounded.MaxDuration = time.Minute
	limited := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{Limits: bounded})
	limited.dispatcher.limits = bounded
	limited.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{deadlineSeen: boundedSeen, text: "stub summary", value: "stub summary"}
	}

	execution, err = limited.start(t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil)
	require.NoError(t, err)
	result, err = execution.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.True(t, <-boundedSeen, "an explicit duration installs a spawn deadline")
}

// TestManagerTokenBudgetOnlyAppliesWhenThePlanDeclaresOne pins Phase L's
// cumulative-token behaviour: usage above the old 256,000-token budget no longer
// fails a child whose policy sets no token cap, while an explicit cap still
// fails it with the max_tokens code.
func TestManagerTokenBudgetOnlyAppliesWhenThePlanDeclaresOne(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, limits Limits) (Result, error) {
		t.Helper()

		fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{Limits: limits})
		fixture.dispatcher.limits = limits
		fixture.dispatcher.newRunner = func() *stubRunner {
			return &stubRunner{
				run: &agent.RunResult{
					Stop: agent.StopEndTurn, Turns: 1,
					Usage: ai.Usage{InputTokens: 300_000, OutputTokens: 10_000},
				},
				text: "stub summary", value: "stub summary",
			}
		}
		execution, err := fixture.start(t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil)
		require.NoError(t, err)

		return execution.Wait(t.Context())
	}

	unbounded, err := run(t, DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, unbounded.Outcome)
	assert.Equal(t, "ok", unbounded.Code)

	capped := DefaultLimits()
	capped.MaxTokens = 256_000
	limited, err := run(t, capped)
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, limited.Outcome)
	assert.Equal(t, "max_tokens", limited.Code)
}

func TestManagerFailsMalformedResultWithDurableReference(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	fixture.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{err: fmt.Errorf("%w: text is required", ErrInvalidResult)}
	}
	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	assert.Equal(t, "invalid_result", result.Code)
	assert.NotEmpty(t, result.ChildSessionID)

	// A durable reference survives even when the live result is unusable.
	_, err = fixture.manager.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, err)
}

func partsText(parts []ai.Part) string {
	var text strings.Builder
	for _, part := range parts {
		if value, ok := part.(ai.TextPart); ok {
			text.WriteString(value.Text)
		}
	}

	return text.String()
}

// firstJSONObject returns the leading JSON object of a tool result, ignoring
// the model-facing text appended after it.
func firstJSONObject(value string) string {
	start := strings.IndexByte(value, '{')
	if start < 0 {
		return value
	}
	depth := 0
	for index := start; index < len(value); index++ {
		switch value[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return value[start : index+1]
			}
		}
	}

	return value[start:]
}

var (
	_ ai.LanguageModel = (*testModel)(nil)
	_ Runner           = (*stubRunner)(nil)
	_ Dispatcher       = stubDispatcher{}
)

func TestManagerQueuesAtCapacityAndStartsOnRelease(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		events []AdmissionEvent
	)
	fixture := newManagerFixtureWithConfig(
		t,
		&testModel{},
		ExecutionOptions{MaxConcurrent: 1, LimitBehavior: LimitBehaviorQueue},
		func(config *Config) {
			config.AdmissionObserver = func(_ context.Context, event AdmissionEvent) {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
		},
	)
	fixture.blockChildren()
	first, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Hold the only slot.", Delivery: DeliveryBackground,
		Ownership: Ownership{
			ParentInteractionID: "root-1", RootInteractionID: "root-1", ParentToolCallID: "call-1",
		},
	}, nil)
	require.NoError(t, err)
	<-fixture.started

	started := make(chan error, 1)
	go func() {
		_, startErr := fixture.start(t.Context(), Request{
			AgentID: "plan", Task: "Wait for the slot.",
		}, nil)
		started <- startErr
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, event := range events {
			if event.Outcome == AdmissionOutcomeQueued {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)

	first.Cancel()
	_, err = first.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, <-started)
}

func TestSpawnToolHandsQueuedForegroundCallerOffToBackground(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixtureWithOptions(t, &testModel{}, ExecutionOptions{
		MaxConcurrent: 1, LimitBehavior: LimitBehaviorQueue, ForegroundBudget: 20 * time.Millisecond,
	})
	fixture.blockChildren()
	tool := fixture.manager.SpawnToolForDispatcher(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil, fixture.dispatcher)

	// The first child holds the only slot.
	holder, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-1", Name: ToolName,
		Args: ai.JSON(`{"prompt":"hold","description":"hold"}`),
	})
	require.NoError(t, err)
	require.Contains(t, partsText(holder), "started in background")
	// A background spawn returns before its slot is granted, so wait until the
	// holder is actually running; otherwise the foreground caller can take the
	// free slot and block on the blocked child forever.
	<-fixture.started

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-2", Name: ToolName,
		Args: ai.JSON(`{"prompt":"wait","description":"wait","run_in_background":false}`),
	})
	require.NoError(t, err)
	var result spawnResult
	require.NoError(t, json.Unmarshal([]byte(firstJSONObject(partsText(parts))), &result))
	assert.Equal(t, SpawnResultSchema, result.Schema)
	assert.NotEmpty(t, result.AgentID)
	assert.Contains(t, partsText(parts), "started in background")
}

// TestStopCauseFromRunnerMapsTheRepeatStopToNoProgress pins the Phase M
// classification seam: the child runner can only report [agent.StopWhen], so the
// repeat guard's own stop cause carries the no_progress code through.
func TestStopCauseFromRunnerMapsTheRepeatStopToNoProgress(t *testing.T) {
	t.Parallel()

	assert.Equal(t, runStopNoProgress, stopCauseFromRunner(StopCauseNoProgress))
	assert.Equal(t, runStopNone, stopCauseFromRunner(""))
	assert.Equal(t, runStopNone, stopCauseFromRunner("something-else"))
}

// TestManagerRejectsAnAutoFollowUpBoundAboveTheHardLimit pins the validation that
// survives Phase M: zero is unlimited, but an explicit bound stays bounded.
func TestManagerRejectsAnAutoFollowUpBoundAboveTheHardLimit(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(root, 0o700))
	value, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(value)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	repository, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: value.Identity().Key(), WorkspacePath: value.Root(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close()) })
	config := Config{Repository: repository, Parent: parent, Tree: tree, Model: &testModel{}}

	manager, err := New(config)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		require.NoError(t, manager.Close(ctx))
	})
	assert.Zero(t, manager.MaxAutoFollowUps(), "zero is unlimited")

	config.Options = ExecutionOptions{MaxAutoFollowUps: 33}
	_, err = New(config)
	require.ErrorIs(t, err, ErrInvalid)
}
