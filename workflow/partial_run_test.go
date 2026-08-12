package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestRunPartialReusesPreviousAndInvalidatesDirtyDescendants(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	plan := partialRunLinearPlan(t, &firstCalls, &secondCalls)

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(sequentialRunIDSource()),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	first, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	assertPartialStringOutput(t, first, "second:first:input")

	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("initial calls = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}

	reused, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{},
		Previous: &first.Data,
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(reuse) error = %v", err)
	}

	assertPartialStringOutput(t, reused, "second:first:input")

	if firstCalls.Load() != 1 || secondCalls.Load() != 2 {
		t.Fatalf("reuse calls = (%d, %d), want (1, 2)", firstCalls.Load(), secondCalls.Load())
	}

	if reused.Nodes["start"].Origin != workflow.PartialDataReused ||
		reused.Nodes["first"].Origin != workflow.PartialDataReused ||
		reused.Nodes["second"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("reuse origins = %#v", reused.Nodes)
	}

	dirty, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
		Previous: &reused.Data,
		Dirty:    []workflow.NodeID{"first", "first"},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(dirty) error = %v", err)
	}

	assertPartialStringOutput(t, dirty, "second:first:input")

	if firstCalls.Load() != 2 || secondCalls.Load() != 3 {
		t.Fatalf("dirty calls = (%d, %d), want (2, 3)", firstCalls.Load(), secondCalls.Load())
	}

	if dirty.Nodes["start"].Origin != workflow.PartialDataReused ||
		dirty.Nodes["first"].Origin != workflow.PartialDataExecuted ||
		dirty.Nodes["second"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("dirty origins = %#v", dirty.Nodes)
	}
}

func TestPartialRunFingerprintUsesScopedSourceIdentity(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := singleActionDefinition(
		t,
		"partial-scoped",
		stringSchema,
		workflow.NodePolicy{},
	)
	baseAction := constantAction("partial-scoped", "base", stringSchema)

	baseRegistry, err := workflow.NewDefaultRegistry(baseAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	expandedRegistry := newCheckpointExpandedRegistry(t, baseAction)
	basePlan := compileFingerprintPlan(t, definition, baseRegistry)
	expandedPlan := compileFingerprintPlan(t, definition, expandedRegistry)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	baseResult, err := runner.RunPartial(
		t.Context(),
		basePlan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	if err != nil {
		t.Fatalf("RunPartial(base) error = %v", err)
	}

	expandedResult, err := runner.RunPartial(
		t.Context(),
		expandedPlan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	if err != nil {
		t.Fatalf("RunPartial(expanded) error = %v", err)
	}

	if baseResult.SourcePlanFingerprint != expandedResult.SourcePlanFingerprint ||
		baseResult.PlanFingerprint != expandedResult.PlanFingerprint {
		t.Fatalf(
			"unused contracts changed Partial Run identity: source=%q/%q partial=%q/%q",
			baseResult.SourcePlanFingerprint,
			expandedResult.SourcePlanFingerprint,
			baseResult.PlanFingerprint,
			expandedResult.PlanFingerprint,
		)
	}

	changedAction := &fakeAction{
		spec: actionSpec(
			"partial-scoped",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{
				"result": stringSchema,
				"extra":  stringSchema,
			},
		),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("changed"),
				"extra":  workflow.MustValueOf("changed"),
			}}, nil
		},
	}

	changedRegistry, err := workflow.NewDefaultRegistry(changedAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry(changed) error = %v", err)
	}

	changedPlan := compileFingerprintPlan(t, definition, changedRegistry)

	changedResult, err := runner.RunPartial(
		t.Context(),
		changedPlan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	if err != nil {
		t.Fatalf("RunPartial(changed) error = %v", err)
	}

	if baseResult.PlanFingerprint == changedResult.PlanFingerprint {
		t.Fatal("used contract change did not change Partial Run identity")
	}
}

func TestRunPartialPinPrecedenceAndDetachedResult(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	plan := partialRunLinearPlan(t, &firstCalls, &secondCalls)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start": {"value": workflow.MustValueOf("pinned-input")},
			"first": {"value": workflow.MustValueOf("pinned-first")},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	assertPartialStringOutput(t, result, "second:pinned-first")

	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("pin calls = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
	}

	if result.Nodes["start"].Origin != workflow.PartialDataPinned ||
		result.Nodes["first"].Origin != workflow.PartialDataPinned {
		t.Fatalf("pin origins = %#v", result.Nodes)
	}

	if result.Nodes["first"].NodeRun.Attempts != 0 ||
		!result.Nodes["first"].NodeRun.StartedAt.IsZero() ||
		!result.Nodes["first"].NodeRun.EndedAt.IsZero() {
		t.Fatalf("pinned node accounting = %#v", result.Nodes["first"])
	}

	result.Data.Nodes["first"] = workflow.PartialNodeData{}

	result.Outputs["value"] = workflow.MustValueOf("mutated")

	if got := result.Data.Nodes["second"].Outputs["value"].String(); got != `"second:pinned-first"` {
		t.Fatalf("mutating result changed sibling data: %s", got)
	}
}

func TestResumePartialRestoresPendingPinWithoutInvocation(t *testing.T) {
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-resume", nil }),
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
	assertPartialInterrupted(t, interrupted, err, "partial-resume")

	if firstCalls.Load() != 0 || secondCalls.Load() != 0 {
		t.Fatalf("calls before resume = (%d, %d), want (0, 0)", firstCalls.Load(), secondCalls.Load())
	}

	if _, err := runner.Resume(t.Context(), plan, interrupted.RunID, nil); !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Resume(partial checkpoint) error = %v, want ErrRun", err)
	}

	completed, err := runner.ResumePartial(t.Context(), plan, "second", interrupted.RunID, nil)
	if err != nil {
		t.Fatalf("Runner.ResumePartial() error = %v", err)
	}

	assertPartialStringOutput(t, completed, "second:input")

	if completed.Nodes["second"].Origin != workflow.PartialDataPinned ||
		completed.Nodes["second"].NodeRun.Attempts != 0 {
		t.Fatalf("resumed destination = %#v", completed.Nodes["second"])
	}

	if firstCalls.Load() != 0 || secondCalls.Load() != 0 {
		t.Fatalf("calls after resume = (%d, %d), want (0, 0)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestRunPartialConcurrentSourcePlanReuse(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	plan := partialRunLinearPlan(t, &firstCalls, &secondCalls)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	const runs = 32

	errorsChannel := make(chan error, runs)
	for index := range runs {
		go func() {
			input := fmt.Sprintf("input-%d", index)

			result, runErr := runner.RunPartial(
				t.Context(),
				plan,
				"second",
				workflow.PartialRunInput{Inputs: map[string]workflow.Value{
					"value": workflow.MustValueOf(input),
				}},
			)
			if runErr != nil {
				errorsChannel <- runErr

				return
			}

			want := workflow.MustValueOf("second:first:" + input).String()
			if got := result.Outputs["value"].String(); got != want {
				errorsChannel <- fmt.Errorf("output = %s, want %s", got, want)

				return
			}

			errorsChannel <- nil
		}()
	}

	for range runs {
		if runErr := <-errorsChannel; runErr != nil {
			t.Fatal(runErr)
		}
	}

	if firstCalls.Load() != runs || secondCalls.Load() != runs {
		t.Fatalf("concurrent calls = (%d, %d), want (%d, %d)", firstCalls.Load(), secondCalls.Load(), runs, runs)
	}
}

func partialRunLinearPlan(
	t *testing.T,
	firstCalls *atomic.Int32,
	secondCalls *atomic.Int32,
) *workflow.Plan {
	t.Helper()

	definition, registry := partialRunLinearFixture(t, firstCalls, secondCalls)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return plan
}

func partialRunLinearFixture(
	t *testing.T,
	firstCalls *atomic.Int32,
	secondCalls *atomic.Int32,
) (workflow.Definition, *workflow.Registry) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	first := &fakeAction{
		spec: actionSpec(
			"partial-first",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"value": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			firstCalls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"value": workflow.MustValueOf("first:" + mustDecodeString(t, input.Values["value"])),
			}}, nil
		},
	}
	second := &fakeAction{
		spec: actionSpec(
			"partial-second",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"value": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			secondCalls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"value": workflow.MustValueOf("second:" + mustDecodeString(t, input.Values["value"])),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(first, second)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "partial-linear", Revision: "v1", Name: "Partial Linear",
		Inputs: map[string]workflow.WorkflowInput{"value": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"value": nodeOutput(stringSchema, "second", "value"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "first", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial-first"),
				Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{
				ID: "second", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial-second"),
				Inputs: map[string]workflow.Binding{"value": nodeBinding("first", "value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "first"),
			edge("first", workflow.RouteSuccess, "second"),
			edge("second", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}, registry
}

func sequentialRunIDSource() workflow.RunIDSource {
	var next atomic.Int32

	return func(time.Time) (string, error) {
		return fmt.Sprintf("partial-run-%d", next.Add(1)), nil
	}
}

func mustDecodeString(t *testing.T, value workflow.Value) string {
	t.Helper()

	decoded, err := workflow.DecodeValue[string](value)
	if err != nil {
		t.Fatalf("DecodeValue() error = %v", err)
	}

	return decoded
}

func assertPartialStringOutput(t *testing.T, result workflow.PartialRunResult, want string) {
	t.Helper()

	if result.Status != workflow.RunStatusSucceeded {
		t.Fatalf("PartialRunResult.Status = %s, want succeeded", result.Status)
	}

	if got := result.Outputs["value"].String(); got != workflow.MustValueOf(want).String() {
		t.Fatalf("PartialRunResult.Outputs[value] = %s, want %q", got, want)
	}
}

func assertPartialInterrupted(
	t *testing.T,
	result workflow.PartialRunResult,
	err error,
	runID string,
) {
	t.Helper()

	var interruptError *workflow.InterruptError
	if !errors.As(err, &interruptError) || result.Status != workflow.RunStatusInterrupted ||
		result.RunID != runID {
		t.Fatalf("RunPartial() = (%#v, %v), want interrupted %q", result, err, runID)
	}
}
