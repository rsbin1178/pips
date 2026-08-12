package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rsbin/pips/workflow"
)

type nodeExecutionExampleAction struct {
	schema workflow.PortSchema
}

func (a nodeExecutionExampleAction) Spec() workflow.ActionSpec {
	return workflow.ActionSpec{
		Key: "record-example", Version: "v1",
		Inputs:  map[string]workflow.PortSchema{},
		Outputs: map[string]workflow.PortSchema{"result": a.schema},
	}
}

func (nodeExecutionExampleAction) Run(
	context.Context,
	workflow.ActionInput,
) (workflow.ActionOutput, error) {
	return workflow.ActionOutput{Values: map[string]workflow.Value{
		"result": workflow.MustValueOf("stored"),
	}}, nil
}

func ExampleWithNodeExecutionRecorder() {
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "record-example", Revision: "v1", Name: "Record example",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": {
				Schema: stringSchema,
				Binding: workflow.Binding{
					Source: workflow.BindingNodeOutput, Node: "action", Port: "result",
				},
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"record-example","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "action"},
			{From: workflow.NodeRoute{Node: "action", Route: workflow.RouteSuccess}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}
	registry, _ := workflow.NewDefaultRegistry(nodeExecutionExampleAction{schema: stringSchema})
	plan, _ := workflow.Compile(context.Background(), definition, registry)

	// The host's recorder can upsert by RunID + Address. It handles its own
	// storage timeout and persistence errors without returning them to Runner.
	runner, _ := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "history-run", nil }),
		workflow.WithNodeExecutionRecorder(func(ctx context.Context, record workflow.NodeExecution) {
			storeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()

			if record.Address.NodeID != "action" || record.NodeRun.Status == workflow.NodeStatusRunning {
				return
			}

			// A real application would call its repository here and log any error.
			_ = storeCtx

			fmt.Println(record.RunID, record.Address.NodeID, record.NodeRun.Status, record.Route)
		}),
	)
	_, _ = runner.Run(context.Background(), plan, map[string]workflow.Value{})

	// Output: history-run action succeeded success
}
