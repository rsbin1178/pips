package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin1178/pips/workflow"
)

func ExampleLoopNode() {
	integerSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"integer"}`))
	countSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"integer","minimum":1}`))
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	ready := workflow.MustValueOf("ready")

	conditionConfig, _ := json.Marshal(workflow.ConditionConfig{
		Predicate: workflow.Predicate{
			Op: workflow.PredicateEqual, Input: "state", Value: &ready,
		},
		TrueRoute: "stop", FalseRoute: "next",
	})
	setConfig, _ := json.Marshal(workflow.SetVariableConfig{
		Assignments: []workflow.SetVariableAssignment{{Target: "state", Input: "next"}},
	})
	body := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "feedback-body", Revision: "v1", Name: "Feedback Body",
		Inputs:  map[string]workflow.WorkflowInput{"index": {Schema: integerSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: conditionConfig,
				Inputs: map[string]workflow.Binding{
					"state": {Source: workflow.BindingLoopVariable, Port: "state"},
				},
			},
			{
				ID: "set", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: setConfig,
				Inputs: map[string]workflow.Binding{
					"next": {Source: workflow.BindingLiteral, Value: &ready},
				},
			},
			{ID: "break", Type: workflow.NodeTypeBreak, Version: workflow.BuiltinNodeVersion},
			{ID: "continue", Type: workflow.NodeTypeContinue, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "stop", "break"),
			edge("condition", "next", "set"),
			edge("set", workflow.RouteSuccess, "continue"),
			edge("break", workflow.RouteSuccess, "end"),
			edge("continue", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	loopConfig, _ := json.Marshal(workflow.LoopConfig{
		Body: body, Mode: workflow.LoopCount,
		Variables: []workflow.LoopVariable{{Name: "state", Schema: stringSchema}},
		Outputs: []workflow.LoopOutput{
			{Name: "final", Source: workflow.LoopOutputVariable, Port: "state"},
		},
		MaxIterations: 10,
	})
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "feedback", Revision: "v1", Name: "Feedback",
		Inputs: map[string]workflow.WorkflowInput{
			"count": {Schema: countSchema, Required: true}, "state": {Schema: stringSchema, Required: true},
		},
		Outputs: map[string]workflow.OutputBinding{
			"final": nodeOutput(stringSchema, "loop", "final"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "loop", Type: workflow.NodeTypeLoop, Version: workflow.BuiltinNodeVersion,
				Config: loopConfig,
				Inputs: map[string]workflow.Binding{
					"count": workflowInput("count"), "state": workflowInput("state"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "loop"),
			edge("loop", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, _ := workflow.NewDefaultRegistry()
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	runner, _ := workflow.NewRunner()
	result, _ := runner.Run(context.Background(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(5), "state": workflow.MustValueOf("pending"),
	})

	fmt.Println(result.Outputs["final"].String())
	// Output: "ready"
}
