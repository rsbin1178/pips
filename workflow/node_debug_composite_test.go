package workflow_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestDebugNodeSubWorkflowRetainsChildExecution(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec("debug-sub-child", map[string]workflow.PortSchema{
			"value": stringSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "debug-sub-child")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	plan := compileWithResolver(t, parent, staticResolver(child), action)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("sub"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"value": workflow.MustValueOf("child-value"),
	})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if result.Execution.Outputs["result"].String() != `"child-value"` ||
		len(result.InnerExecutions) != 1 {
		t.Fatalf("DebugNode() result = %#v", result)
	}

	record := result.InnerExecutions[0]
	if record.Address.NodeID != "child_action" || len(record.Address.Scope) != 1 ||
		record.Address.Scope[0].Kind != workflow.ScopeSubWorkflow ||
		record.Address.Scope[0].NodeID != "sub" || record.Address.Scope[0].Index != -1 {
		t.Fatalf("child execution address = %#v", record.Address)
	}
}

func TestDebugNodeLoopRetainsIterationExecutions(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	stringsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	action := &fakeAction{
		spec: actionSpec("join_loop_arrays", map[string]workflow.PortSchema{
			"left": stringSchema, "right": stringSchema,
			"prefix": stringSchema, "index": integerSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			left, _ := workflow.DecodeValue[string](input.Values["left"])
			right, _ := workflow.DecodeValue[string](input.Values["right"])
			prefix, _ := workflow.DecodeValue[string](input.Values["prefix"])
			index, _ := workflow.DecodeValue[int](input.Values["index"])

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(fmt.Sprintf("%s:%s:%s:%d", prefix, left, right, index)),
			}}, nil
		},
	}
	body := arrayLoopBody(t, stringSchema, integerSchema)
	definition := loopParentDefinition(
		t,
		"debug-array-loop",
		map[string]workflow.PortSchema{
			"left": stringsSchema, "right": stringsSchema, "prefix": stringSchema,
		},
		map[string]workflow.Binding{
			"left": workflowInput("left"), "right": workflowInput("right"),
			"prefix": workflowInput("prefix"),
		},
		map[string]workflow.OutputBinding{
			"results": nodeOutput(stringsSchema, "loop", "results"),
		},
		workflow.LoopConfig{
			Body: body, Mode: workflow.LoopArray, Arrays: []string{"left", "right"},
			Outputs: []workflow.LoopOutput{
				{Name: "results", Source: workflow.LoopOutputBody, Port: "result"},
			},
			MaxIterations: 10,
		},
	)
	plan := compileRoundTrip(t, definition, action)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("loop"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"left":   workflow.MustValueOf([]string{"a", "b", "c"}),
		"right":  workflow.MustValueOf([]string{"x", "y"}),
		"prefix": workflow.MustValueOf("p"),
	})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if result.Execution.Outputs["results"].String() != `["p:a:x:0","p:b:y:1"]` ||
		len(result.InnerExecutions) != 2 {
		t.Fatalf("DebugNode() result = %#v", result)
	}

	for index, record := range result.InnerExecutions {
		if record.Address.NodeID != "join" || len(record.Address.Scope) != 1 ||
			record.Address.Scope[0].Kind != workflow.ScopeLoopIteration ||
			record.Address.Scope[0].Index != index {
			t.Fatalf("loop record[%d] = %#v", index, record)
		}
	}
}
