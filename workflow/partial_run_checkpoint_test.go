package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestResumePartialPreservesExecutedDataAndAttempts(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	definition, registry := partialRunLinearFixture(t, &firstCalls, &secondCalls)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("second")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("second")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-executed", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	assertPartialInterrupted(t, interrupted, err, "partial-executed")

	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("calls before resume = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}

	after, err := runner.ResumePartial(t.Context(), plan, "second", interrupted.RunID, nil)
	assertPartialInterrupted(t, after, err, "partial-executed")

	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls after first resume = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}

	completed, err := runner.ResumePartial(t.Context(), plan, "second", interrupted.RunID, nil)
	if err != nil {
		t.Fatalf("Runner.ResumePartial(second) error = %v", err)
	}

	assertPartialStringOutput(t, completed, "second:first:input")

	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls after resume = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}

	if completed.Nodes["first"].Origin != workflow.PartialDataExecuted ||
		completed.Nodes["first"].NodeRun.Attempts != 1 ||
		completed.Nodes["second"].Origin != workflow.PartialDataExecuted ||
		completed.Nodes["second"].NodeRun.Attempts != 1 {
		t.Fatalf("resumed node accounting = %#v", completed.Nodes)
	}
}

func TestResumePartialRestoresHandledFailureWithoutReusingIt(t *testing.T) {
	t.Parallel()

	var sourceCalls, handlerCalls atomic.Int32

	plan := interruptedFailurePlan(t, &sourceCalls, &handlerCalls)
	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) {
			return "partial-failure-resume", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.RunPartial(
		t.Context(),
		plan,
		"handler",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	assertPartialInterrupted(t, interrupted, err, "partial-failure-resume")

	if sourceCalls.Load() != 1 || handlerCalls.Load() != 0 ||
		interrupted.Nodes["unreliable"].NodeRun.Status != workflow.NodeStatusException {
		t.Fatalf(
			"interrupted result = %#v, calls = %d/%d",
			interrupted,
			sourceCalls.Load(),
			handlerCalls.Load(),
		)
	}

	resumed, err := runner.ResumePartial(
		t.Context(),
		plan,
		"handler",
		interrupted.RunID,
		nil,
	)
	if err != nil {
		t.Fatalf("Runner.ResumePartial() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusPartialSucceeded ||
		resumed.Outputs["result"].String() != `"checkpoint failure|error"` ||
		sourceCalls.Load() != 1 || handlerCalls.Load() != 1 {
		t.Fatalf(
			"resumed result = %#v, calls = %d/%d",
			resumed,
			sourceCalls.Load(),
			handlerCalls.Load(),
		)
	}

	if _, reusable := resumed.Data.Nodes["unreliable"]; reusable {
		t.Fatalf("exception node became reusable: %#v", resumed.Data)
	}
}

func TestResumePartialDynamicInterruptRestoresStateAndTargetData(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"partial-dynamic",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			wasInterrupted, hasState, state := workflow.GetInterruptState(ctx)
			if !wasInterrupted {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf("approve"),
					workflow.MustValueOf("waiting"),
				)
			}

			isTarget, hasData, data := workflow.GetResumeContext(ctx)
			if !hasState || state.String() != `"waiting"` || !isTarget || !hasData {
				return workflow.ActionOutput{}, errors.New("partial interrupt state was not restored")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": data}}, nil
		},
	}
	definition := dynamicSingleActionDefinition(t, "partial-dynamic", "partial-dynamic", stringSchema)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-dynamic", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.RunPartial(t.Context(), plan, "work", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	assertPartialInterrupted(t, interrupted, err, "partial-dynamic")

	contexts := interrupted.Interruption.Contexts
	if len(contexts) != 1 || contexts[0].ID == "" || calls.Load() != 1 {
		t.Fatalf("dynamic interruption = %#v, calls = %d", contexts, calls.Load())
	}

	completed, err := runner.ResumePartial(
		t.Context(),
		plan,
		"work",
		interrupted.RunID,
		[]workflow.ResumeTarget{{
			InterruptID: contexts[0].ID,
			Data:        workflow.MustValueOf("approved"),
		}},
	)
	if err != nil {
		t.Fatalf("Runner.ResumePartial() error = %v", err)
	}

	if completed.Outputs["result"].String() != `"approved"` || calls.Load() != 2 ||
		completed.Nodes["work"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("dynamic completion = %#v, calls = %d", completed, calls.Load())
	}
}

func TestResumePartialRejectsCorruptPartialCheckpointBeforeInvocation(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	definition, registry := partialRunLinearFixture(t, &firstCalls, &secondCalls)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("second")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-corrupt", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start":  {"value": workflow.MustValueOf("input")},
			"first":  {"value": workflow.MustValueOf("first:input")},
			"second": {"value": workflow.MustValueOf("second:input")},
		},
	})
	assertPartialInterrupted(t, interrupted, err, "partial-corrupt")

	original := store.value(interrupted.RunID)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "missing section",
			mutate: func(checkpoint map[string]any) {
				delete(checkpoint, "partial_run")
			},
		},
		{
			name: "unknown version",
			mutate: func(checkpoint map[string]any) {
				partialCheckpointObject(t, checkpoint)["version"] = float64(99)
			},
		},
		{
			name: "wrong destination",
			mutate: func(checkpoint map[string]any) {
				partialCheckpointObject(t, checkpoint)["destination"] = "first"
			},
		},
		{
			name: "invalid workflow input scope",
			mutate: func(checkpoint map[string]any) {
				partialCheckpointObject(t, checkpoint)["workflow_inputs"] = []any{"unknown"}
			},
		},
		{
			name: "invalid origin",
			mutate: func(checkpoint map[string]any) {
				partial := partialCheckpointObject(t, checkpoint)
				origins := partialCheckpointSlice(t, partial, "origins")
				origins[0] = "foreign"
			},
		},
		{
			name: "duplicate available index",
			mutate: func(checkpoint map[string]any) {
				partial := partialCheckpointObject(t, checkpoint)
				available := partialCheckpointSlice(t, partial, "available")
				partial["available"] = append(available, available[0])
			},
		},
		{
			name: "invalid available route",
			mutate: func(checkpoint map[string]any) {
				partial := partialCheckpointObject(t, checkpoint)
				available := partialCheckpointSlice(t, partial, "available")
				partialCheckpointMap(t, available[0])["route"] = "foreign"
			},
		},
		{
			name: "invalid available output",
			mutate: func(checkpoint map[string]any) {
				partial := partialCheckpointObject(t, checkpoint)
				available := partialCheckpointSlice(t, partial, "available")
				entry := partialCheckpointMap(t, available[0])
				outputs := partialCheckpointMap(t, entry["outputs"])
				outputs["value"] = float64(42)
			},
		},
		{
			name: "effective route edge mismatch",
			mutate: func(checkpoint map[string]any) {
				execution := partialCheckpointMap(t, checkpoint["execution"])
				execution["edges"] = "AgE="
			},
		},
	}

	for _, test := range tests {
		store.replace(interrupted.RunID, mutateCheckpointJSON(t, original, test.mutate))

		_, resumeErr := runner.ResumePartial(
			t.Context(),
			plan,
			"second",
			interrupted.RunID,
			nil,
		)
		if !errors.Is(resumeErr, workflow.ErrRun) {
			t.Fatalf("%s: Runner.ResumePartial() error = %v, want ErrRun", test.name, resumeErr)
		}

		if firstCalls.Load() != 0 || secondCalls.Load() != 0 {
			t.Fatalf(
				"%s: corrupt checkpoint invoked Actions: (%d, %d)",
				test.name,
				firstCalls.Load(),
				secondCalls.Load(),
			)
		}
	}
}

func TestResumePartialRejectsOrdinaryCheckpoint(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	definition, registry := partialRunLinearFixture(t, &firstCalls, &secondCalls)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("second")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "ordinary-resume", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ordinary, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	assertInterrupted(t, ordinary, err, "ordinary-resume")

	_, err = runner.ResumePartial(t.Context(), plan, "second", ordinary.RunID, nil)
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.ResumePartial(ordinary checkpoint) error = %v, want ErrRun", err)
	}

	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("ordinary checkpoint rejection calls = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestResumePartialAfterHostInterruptBeforeLaunch(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-host", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	runContext, interrupt := workflow.WithRunInterrupt(t.Context())
	interrupt()

	interrupted, err := runner.RunPartial(runContext, plan, "work", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	assertPartialInterrupted(t, interrupted, err, "partial-host")

	if calls.Load() != 0 || len(interrupted.Interruption.RerunNodes) != 1 {
		t.Fatalf("host interruption = %#v, calls = %d", interrupted.Interruption, calls.Load())
	}

	completed, err := runner.ResumePartial(t.Context(), plan, "work", interrupted.RunID, nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("host resume = %#v, error = %v, calls = %d", completed, err, calls.Load())
	}
}

func partialCheckpointObject(t *testing.T, checkpoint map[string]any) map[string]any {
	t.Helper()

	partial, ok := checkpoint["partial_run"].(map[string]any)
	if !ok {
		t.Fatalf("partial_run = %#v, want object", checkpoint["partial_run"])
	}

	return partial
}

func partialCheckpointSlice(t *testing.T, object map[string]any, key string) []any {
	t.Helper()

	value, ok := object[key].([]any)
	if !ok {
		t.Fatalf("partial checkpoint %s = %#v, want array", key, object[key])
	}

	return value
}

func partialCheckpointMap(t *testing.T, value any) map[string]any {
	t.Helper()

	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("partial checkpoint value = %#v, want object", value)
	}

	return object
}
