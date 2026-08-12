package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func ExampleRunner_DebugNode() {
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	action := &fakeAction{
		spec: actionSpec("preview", map[string]workflow.PortSchema{
			"prompt": stringSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["prompt"],
			}}, nil
		},
	}
	registry, _ := workflow.NewDefaultRegistry(action)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "preview-flow", Revision: "v1", Name: "Preview Flow",
		Inputs: map[string]workflow.WorkflowInput{"prompt": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "preview", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "preview", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"preview","version":"v1"}`),
				Inputs: map[string]workflow.Binding{"prompt": workflowInput("prompt")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "preview"),
			edge("preview", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("preview"))
	runner, _ := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "preview-run", nil }),
	)
	result, _ := runner.DebugNode(context.Background(), debugPlan, map[string]workflow.Value{
		"prompt": workflow.MustValueOf("paper city"),
	})

	fmt.Println(result.RunID, result.Status, result.Execution.Route)
	fmt.Println(result.Execution.Outputs["result"].String())
	// Output:
	// preview-run succeeded success
	// "paper city"
}

func ExampleNodeDebugResult_composite() {
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	integerSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"integer"}`))
	itemsSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"array","items":{"type":"string"}}`))
	action := &fakeAction{
		spec: actionSpec("map-item", map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "map-body", Revision: "v1", Name: "Map Body",
		Inputs: map[string]workflow.WorkflowInput{"item": {Schema: stringSchema, Required: true}, "index": {Schema: integerSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "map", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "map", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"map-item","version":"v1"}`),
				Inputs: map[string]workflow.Binding{
					"item": workflowInput("item"), "index": workflowInput("index"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "map"),
			edge("map", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	config, _ := json.Marshal(workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10, MaxConcurrency: 2,
	})
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "batch-preview", Revision: "v1", Name: "Batch Preview",
		Inputs: map[string]workflow.WorkflowInput{"items": {Schema: itemsSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"results": nodeOutput(itemsSchema, "batch", "results"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "batch", Type: workflow.NodeTypeBatch, Version: workflow.BuiltinNodeVersion,
				Config: config, Inputs: map[string]workflow.Binding{"items": workflowInput("items")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "batch"),
			edge("batch", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	registry, _ := workflow.NewDefaultRegistry(action)
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("batch"))
	runner, _ := workflow.NewRunner()
	result, _ := runner.DebugNode(context.Background(), debugPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b"}),
	})

	fmt.Println(result.Execution.Outputs["results"].String())

	for _, inner := range result.InnerExecutions {
		fmt.Println(inner.Address.NodeID, inner.Address.Scope[0].Index)
	}
	// Output:
	// ["a","b"]
	// map 0
	// map 1
}

func ExampleRunner_ResumeNodeDebug() {
	action := &fakeAction{
		spec: actionSpec("resume-preview", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{}),
	}
	registry, _ := workflow.NewDefaultRegistry(action)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "resume-preview", Revision: "v1", Name: "Resume Preview",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"resume-preview","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "work"),
			edge("work", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan, _ := workflow.Compile(
		context.Background(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("work"))
	store := &memoryCheckpointStore{}
	runner, _ := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "resume-preview-run", nil }),
	)
	paused, _ := runner.DebugNode(context.Background(), debugPlan, map[string]workflow.Value{})
	completed, _ := runner.ResumeNodeDebug(
		context.Background(), debugPlan, paused.RunID, nil,
	)

	fmt.Println(paused.Status, completed.Status, completed.RunID)
	// Output:
	// interrupted succeeded resume-preview-run
}
