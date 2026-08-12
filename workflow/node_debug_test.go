package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestDebugNodeIsolatesTargetAndPreservesLiteralInputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var (
		upstreamCalls atomic.Int64
		targetCalls   atomic.Int64
	)

	upstream := &fakeAction{
		spec: actionSpec("upstream", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"value": stringSchema,
		}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			upstreamCalls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"value": workflow.MustValueOf("upstream"),
			}}, nil
		},
	}
	target := &fakeAction{
		spec: actionSpec("target", map[string]workflow.PortSchema{
			"dynamic": stringSchema,
			"fixed":   stringSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			targetCalls.Add(1)

			if input.Values["dynamic"].String() != `"debug"` ||
				input.Values["fixed"].String() != `"literal"` {
				t.Fatalf("Action inputs = %#v", input.Values)
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["dynamic"],
			}}, nil
		},
	}
	literal := workflow.MustValueOf("literal")
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "node-debug", Revision: "v1", Name: "Node Debug",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "target", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "upstream", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "upstream"),
			},
			{
				ID: "target", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "target"),
				Inputs: map[string]workflow.Binding{
					"dynamic": {Source: workflow.BindingNodeOutput, Node: "upstream", Port: "value"},
					"fixed":   {Source: workflow.BindingLiteral, Value: &literal},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "upstream"),
			edge("upstream", workflow.RouteSuccess, "target"),
			edge("target", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, upstream, target)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("target"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	spec := debugPlan.Spec()
	if len(spec.Inputs) != 1 || !spec.Inputs["dynamic"].IsValid() {
		t.Fatalf("NodeDebugPlan.Spec().Inputs = %#v", spec.Inputs)
	}

	if _, exposed := spec.Inputs["fixed"]; exposed {
		t.Fatal("literal input was exposed in NodeDebugSpec")
	}

	if debugPlan.SourcePlanFingerprint() != plan.Fingerprint() || debugPlan.Fingerprint() == "" {
		t.Fatal("NodeDebugPlan fingerprints are incomplete")
	}

	var idCalls atomic.Int64

	runner, err := workflow.NewRunner(workflow.WithRunIDSource(func(time.Time) (string, error) {
		idCalls.Add(1)

		return "node-debug-run", nil
	}))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("DebugNode(missing input) error = %v, want ErrRun", err)
	}

	if idCalls.Load() != 0 || targetCalls.Load() != 0 {
		t.Fatal("invalid debug input created a Run or invoked the target")
	}

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"dynamic": workflow.MustValueOf("debug"),
	})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if result.Status != workflow.RunStatusSucceeded || result.Execution.NodeRun.Status != workflow.NodeStatusSucceeded {
		t.Fatalf("DebugNode() statuses = %s, %s", result.Status, result.Execution.NodeRun.Status)
	}

	if result.Execution.Route != workflow.RouteSuccess ||
		result.Execution.Outputs["result"].String() != `"debug"` ||
		result.Execution.Inputs["fixed"].String() != `"literal"` {
		t.Fatalf("DebugNode() execution = %#v", result.Execution)
	}

	if upstreamCalls.Load() != 0 || targetCalls.Load() != 1 {
		t.Fatalf("Action calls upstream=%d target=%d", upstreamCalls.Load(), targetCalls.Load())
	}

	if len(result.InnerExecutions) != 0 {
		t.Fatalf("DebugNode() inner executions = %#v", result.InnerExecutions)
	}
}

func TestDebugNodeBatchReturnsStableScopedInnerExecutions(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	resultsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	body := batchBodyDefinition(t, stringSchema, stringSchema, "map", false)
	definition := batchParentDefinition(t, itemsSchema, resultsSchema, workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10, MaxConcurrency: 3,
	}, workflow.PortSchema{})
	action := &fakeAction{
		spec: actionSpec("map", map[string]workflow.PortSchema{
			"item":  stringSchema,
			"index": mustSchema(t, `{"type":"integer"}`),
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	plan := compileRoundTrip(t, definition, action)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("batch"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	runner, _ := workflow.NewRunner(workflow.WithRunIDSource(func(time.Time) (string, error) {
		return "batch-debug", nil
	}))

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b", "c"}),
	})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if len(result.InnerExecutions) != 3 {
		t.Fatalf("len(InnerExecutions) = %d, want 3: %#v", len(result.InnerExecutions), result.InnerExecutions)
	}

	for index, record := range result.InnerExecutions {
		if record.Address.NodeID != "map" || len(record.Address.Scope) != 1 ||
			record.Address.Scope[0].Kind != workflow.ScopeBatchItem ||
			record.Address.Scope[0].Index != index {
			t.Fatalf("InnerExecutions[%d].Address = %#v", index, record.Address)
		}
	}
}

func TestDebugNodeNestedBatchLeafRunsOnce(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	resultsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	body := batchBodyDefinition(t, stringSchema, stringSchema, "map-once", false)
	definition := batchParentDefinition(t, itemsSchema, resultsSchema, workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}, workflow.PortSchema{})

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("map-once", map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	plan := compileRoundTrip(t, definition, action)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("batch", "map"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	if len(debugPlan.Spec().Inputs) != 2 {
		t.Fatalf("nested leaf inputs = %#v", debugPlan.Spec().Inputs)
	}

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"item": workflow.MustValueOf("single"), "index": workflow.MustValueOf(9),
	})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if calls.Load() != 1 || result.Execution.Outputs["result"].String() != `"single"` {
		t.Fatalf("nested leaf calls=%d execution=%#v", calls.Load(), result.Execution)
	}
}
