package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin1178/pips/workflow"
)

type partialRunExampleAction struct {
	schema workflow.PortSchema
}

func (a partialRunExampleAction) Spec() workflow.ActionSpec {
	return workflow.ActionSpec{
		Key: "render-preview", Version: "v1",
		Inputs:  map[string]workflow.PortSchema{"prompt": a.schema},
		Outputs: map[string]workflow.PortSchema{"image": a.schema},
	}
}

func (a partialRunExampleAction) Run(
	_ context.Context,
	input workflow.ActionInput,
) (workflow.ActionOutput, error) {
	prompt, err := workflow.DecodeValue[string](input.Values["prompt"])
	if err != nil {
		return workflow.ActionOutput{}, err
	}

	return workflow.ActionOutput{Values: map[string]workflow.Value{
		"image": workflow.MustValueOf("preview:" + prompt),
	}}, nil
}

func ExampleRunner_RunPartial() {
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	config, _ := json.Marshal(workflow.ActionConfig{Action: "render-preview", Version: "v1"})
	registry, _ := workflow.NewDefaultRegistry(partialRunExampleAction{schema: stringSchema})
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "preview", Revision: "v1", Name: "Preview",
		Inputs: map[string]workflow.WorkflowInput{"prompt": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"image": {
				Schema: stringSchema,
				Binding: workflow.Binding{
					Source: workflow.BindingNodeOutput, Node: "render", Port: "image",
				},
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "render", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: config,
				Inputs: map[string]workflow.Binding{
					"prompt": {
						Source: workflow.BindingNodeOutput, Node: "start", Port: "prompt",
					},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "render"},
			{From: workflow.NodeRoute{Node: "render", Route: workflow.RouteSuccess}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	runner, _ := workflow.NewRunner()

	first, _ := runner.RunPartial(context.Background(), plan, "render", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"prompt": workflow.MustValueOf("paper city")},
	})
	second, _ := runner.RunPartial(context.Background(), plan, "render", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{},
		Previous: &first.Data,
	})

	fmt.Println(second.Outputs["image"].String())
	fmt.Println(second.Nodes["start"].Origin, second.Nodes["render"].Origin)

	// Output:
	// "preview:paper city"
	// reused executed
}
