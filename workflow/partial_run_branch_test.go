package workflow_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestRunPartialExecutesDestinationSliceInclusively(t *testing.T) {
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

	result, err := runner.RunPartial(t.Context(), plan, "first", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := result.Outputs["value"].String(); got != `"first:input"` {
		t.Fatalf("destination outputs = %s, want first:input", got)
	}

	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("destination calls = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}

	if len(result.Nodes) != 2 || result.Nodes["first"].NodeRun.Status != workflow.NodeStatusSucceeded {
		t.Fatalf("selected nodes = %#v, want start and first", result.Nodes)
	}

	if _, included := result.Nodes["second"]; included {
		t.Fatalf("downstream node was included: %#v", result.Nodes)
	}
}

func TestRunPartialRetainsPreviousRouteUntilBranchIsDirty(t *testing.T) {
	t.Parallel()

	var (
		acceptCalls atomic.Int32
		rejectCalls atomic.Int32
	)

	plan := partialRunBranchPlan(t, &acceptCalls, &rejectCalls)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	rejected, err := runner.RunPartial(t.Context(), plan, "merge", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"approved": workflow.MustValueOf(false)},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(reject) error = %v", err)
	}

	if got := rejected.Outputs["result"].String(); got != `"rejected"` {
		t.Fatalf("rejected result = %s", got)
	}

	reused, err := runner.RunPartial(t.Context(), plan, "merge", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{},
		Previous: &rejected.Data,
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(reuse) error = %v", err)
	}

	if got := reused.Outputs["result"].String(); got != `"rejected"` {
		t.Fatalf("reused result = %s", got)
	}

	if acceptCalls.Load() != 0 || rejectCalls.Load() != 1 {
		t.Fatalf("reuse branch calls = (%d, %d), want (0, 1)", acceptCalls.Load(), rejectCalls.Load())
	}

	accepted, err := runner.RunPartial(t.Context(), plan, "merge", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{"approved": workflow.MustValueOf(true)},
		Previous: &reused.Data,
		Dirty:    []workflow.NodeID{"condition"},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(dirty branch) error = %v", err)
	}

	if got := accepted.Outputs["result"].String(); got != `"accepted"` {
		t.Fatalf("accepted result = %s", got)
	}

	if acceptCalls.Load() != 1 || rejectCalls.Load() != 1 {
		t.Fatalf("dirty branch calls = (%d, %d), want (1, 1)", acceptCalls.Load(), rejectCalls.Load())
	}

	if accepted.Nodes["accept"].Origin != workflow.PartialDataExecuted ||
		accepted.Nodes["reject"].NodeRun.Status != workflow.NodeStatusSkipped ||
		accepted.Nodes["reject"].Origin != "" {
		t.Fatalf("dirty branch nodes = %#v", accepted.Nodes)
	}
}

func TestRunPartialPinDoesNotActivateInactiveBranch(t *testing.T) {
	t.Parallel()

	var (
		acceptCalls atomic.Int32
		rejectCalls atomic.Int32
	)

	plan := partialRunBranchPlan(t, &acceptCalls, &rejectCalls)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.RunPartial(t.Context(), plan, "merge", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"approved": workflow.MustValueOf(true)},
		Pins: workflow.PinData{
			"reject": {"result": workflow.MustValueOf("pinned-reject")},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := result.Outputs["result"].String(); got != `"accepted"` {
		t.Fatalf("result = %s, want accepted", got)
	}

	if acceptCalls.Load() != 1 || rejectCalls.Load() != 0 {
		t.Fatalf("inactive pin calls = (%d, %d), want (1, 0)", acceptCalls.Load(), rejectCalls.Load())
	}

	if result.Nodes["reject"].NodeRun.Status != workflow.NodeStatusSkipped ||
		result.Nodes["reject"].Origin != "" {
		t.Fatalf("inactive pinned node = %#v", result.Nodes["reject"])
	}
}

func partialRunBranchPlan(
	t *testing.T,
	acceptCalls *atomic.Int32,
	rejectCalls *atomic.Int32,
) *workflow.Plan {
	t.Helper()

	boolSchema := mustSchema(t, `{"type":"boolean"}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	accept := partialCountingConstantAction("partial-accept", "accepted", stringSchema, acceptCalls)
	reject := partialCountingConstantAction("partial-reject", "rejected", stringSchema, rejectCalls)
	trueValue := workflow.MustValueOf(true)
	conditionConfig := workflow.ConditionConfig{
		Predicate:  workflow.Predicate{Op: workflow.PredicateEqual, Input: "approved", Value: &trueValue},
		TrueRoute:  "yes",
		FalseRoute: "no",
	}
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"accepted", "rejected"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "partial-branch", Revision: "v1", Name: "Partial Branch",
		Inputs: map[string]workflow.PortSchema{"approved": boolSchema},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, conditionConfig),
				Inputs: map[string]workflow.Binding{"approved": workflowInput("approved")},
			},
			{
				ID: "accept", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial-accept"),
			},
			{
				ID: "reject", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial-reject"),
			},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"accepted": nodeBinding("accept", "result"),
					"rejected": nodeBinding("reject", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "yes", "accept"),
			edge("condition", "no", "reject"),
			edge("accept", workflow.RouteSuccess, "merge"),
			edge("reject", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return compileRoundTrip(t, definition, accept, reject)
}

func partialCountingConstantAction(
	key workflow.ActionKey,
	value string,
	schema workflow.PortSchema,
	calls *atomic.Int32,
) *fakeAction {
	return &fakeAction{
		spec: actionSpec(key, map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"result": schema,
		}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(value),
			}}, nil
		},
	}
}
