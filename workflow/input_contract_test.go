package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestRunnerNormalizesWorkflowInputContractsBeforeStart(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	defaultValue := workflow.MustValueOf("default")
	definition := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Default: &defaultValue,
	})
	plan := compileRoundTrip(t, definition)
	runner := mustRunner(t)

	for _, test := range []struct {
		name   string
		inputs map[string]workflow.Value
		want   string
	}{
		{name: "missing", inputs: map[string]workflow.Value{}, want: `"default"`},
		{name: "empty", inputs: map[string]workflow.Value{"value": workflow.MustValueOf("")}, want: `"default"`},
		{name: "provided", inputs: map[string]workflow.Value{"value": workflow.MustValueOf("provided")}, want: `"provided"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result, err := runner.Run(t.Context(), plan, test.inputs)
			if err != nil {
				t.Fatalf("Runner.Run() error = %v", err)
			}

			if got := result.Outputs["result"].String(); got != test.want {
				t.Fatalf("result = %s, want %s", got, test.want)
			}
		})
	}

	_, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"unknown": workflow.MustValueOf("value"),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run(unknown) error = %v, want ErrRun", err)
	}
}

func TestRunnerRejectsMissingRequiredAndInvalidSuppliedWorkflowInput(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Required: true,
	})
	plan := compileRoundTrip(t, definition)
	runner := mustRunner(t)

	for _, inputs := range []map[string]workflow.Value{
		{},
		{"value": workflow.MustValueOf(42)},
	} {
		if _, err := runner.Run(t.Context(), plan, inputs); !errors.Is(err, workflow.ErrRun) {
			t.Fatalf("Runner.Run(%v) error = %v, want ErrRun", inputs, err)
		}
	}
}

func TestRunnerCompletesOptionalNullableInputWithNull(t *testing.T) {
	t.Parallel()

	nullableSchema := mustSchema(t, `{"type":["string","null"]}`)
	definition := inputContractDefinition(t, nullableSchema, workflow.WorkflowInput{
		Schema: nullableSchema,
	})
	plan := compileRoundTrip(t, definition)

	result := runWorkflow(t, plan, map[string]workflow.Value{})
	if got := result.Outputs["result"].Kind(); got != workflow.ValueNull {
		t.Fatalf("result kind = %v, want ValueNull", got)
	}
}

func TestCompileSnapshotsWorkflowInputDefaultPointer(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	defaultValue := workflow.MustValueOf("before")
	definition := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Default: &defaultValue,
	})
	plan := compileRoundTrip(t, definition)
	defaultValue = workflow.MustValueOf("after")
	definition.Inputs["value"] = workflow.WorkflowInput{
		Schema: stringSchema, Default: &defaultValue,
	}

	result := runWorkflow(t, plan, map[string]workflow.Value{})
	if got := result.Outputs["result"].String(); got != `"before"` {
		t.Fatalf("snapshotted default result = %s, want before", got)
	}
}

func TestSubWorkflowOmitsOptionalChildInputAndUsesChildDefault(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	defaultValue := workflow.MustValueOf("child-default")
	child := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Default: &defaultValue,
	})
	child.ID = "optional-child"
	parent := workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "optional-parent", Revision: "v1", Name: "Optional Parent",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "child", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "child", Type: workflow.NodeTypeSubWorkflow, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.SubWorkflowConfig{Workflow: workflowRef(t, child)}),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "child"),
			edge("child", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(), parent, registry, workflow.WithDefinitionResolver(staticResolver(child)),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	result := runWorkflow(t, plan, map[string]workflow.Value{})
	if got := result.Outputs["result"].String(); got != `"child-default"` {
		t.Fatalf("result = %s, want child-default", got)
	}

	parent.Inputs["value"] = workflow.WorkflowInput{Schema: stringSchema, Required: true}
	parent.Nodes[1].Inputs = map[string]workflow.Binding{"value": workflowInput("value")}

	boundPlan, err := workflow.Compile(
		t.Context(), parent, registry, workflow.WithDefinitionResolver(staticResolver(child)),
	)
	if err != nil {
		t.Fatalf("Compile(bound optional input) error = %v", err)
	}

	boundResult := runWorkflow(t, boundPlan, map[string]workflow.Value{
		"value": workflow.MustValueOf("parent-value"),
	})
	if got := boundResult.Outputs["result"].String(); got != `"parent-value"` {
		t.Fatalf("bound result = %s, want parent-value", got)
	}
}

func TestBatchAndLoopNormalizeOmittedOptionalBodyInputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	resultsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	defaultValue := workflow.MustValueOf("body-default")

	body := workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "optional-body", Revision: "v1", Name: "Optional Body",
		Inputs: map[string]workflow.WorkflowInput{
			"item":   {Schema: stringSchema, Required: true},
			"index":  {Schema: integerSchema, Required: true},
			"prefix": {Schema: stringSchema, Default: &defaultValue},
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": {Schema: stringSchema, Binding: workflowInput("prefix")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges:  []workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
		Limits: workflow.DefaultLimits(),
	}

	batchDefinition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "optional-batch", Revision: "v1", Name: "Optional Batch",
		Inputs: map[string]workflow.WorkflowInput{
			"items": {Schema: itemsSchema, Required: true},
		},
		Outputs: map[string]workflow.OutputBinding{
			"results": nodeOutput(resultsSchema, "batch", "results"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "batch", Type: workflow.NodeTypeBatch, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.BatchConfig{
					Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
					ErrorMode: workflow.BatchTerminate, MaxItems: 10,
				}),
				Inputs: map[string]workflow.Binding{"items": workflowInput("items")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "batch"),
			edge("batch", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	batchPlan := compileRoundTrip(t, batchDefinition)

	batchResult := runWorkflow(t, batchPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b"}),
	})
	if got := batchResult.Outputs["results"].String(); got != `["body-default","body-default"]` {
		t.Fatalf("batch results = %s", got)
	}

	batchDefinition.Inputs["prefix"] = workflow.WorkflowInput{Schema: stringSchema, Required: true}
	batchDefinition.Nodes[1].Inputs["prefix"] = workflowInput("prefix")
	boundBatchPlan := compileRoundTrip(t, batchDefinition)

	boundBatchResult := runWorkflow(t, boundBatchPlan, map[string]workflow.Value{
		"items":  workflow.MustValueOf([]string{"a", "b"}),
		"prefix": workflow.MustValueOf("batch-bound"),
	})
	if got := boundBatchResult.Outputs["results"].String(); got != `["batch-bound","batch-bound"]` {
		t.Fatalf("bound batch results = %s", got)
	}

	loopBody := body
	delete(loopBody.Inputs, "item")
	loopDefinition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "optional-loop", Revision: "v1", Name: "Optional Loop",
		Inputs: map[string]workflow.WorkflowInput{
			"count": {Schema: mustSchema(t, `{"type":"integer","minimum":1}`), Required: true},
		},
		Outputs: map[string]workflow.OutputBinding{
			"results": nodeOutput(resultsSchema, "loop", "results"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "loop", Type: workflow.NodeTypeLoop, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.LoopConfig{
					Body: loopBody, Mode: workflow.LoopCount, MaxIterations: 2,
					Outputs: []workflow.LoopOutput{
						{Name: "results", Source: workflow.LoopOutputBody, Port: "result"},
					},
				}),
				Inputs: map[string]workflow.Binding{"count": workflowInput("count")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "loop"),
			edge("loop", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	loopPlan := compileRoundTrip(t, loopDefinition)

	loopResult := runWorkflow(t, loopPlan, map[string]workflow.Value{"count": workflow.MustValueOf(2)})
	if got := loopResult.Outputs["results"].String(); got != `["body-default","body-default"]` {
		t.Fatalf("loop results = %s", got)
	}

	loopDefinition.Inputs["prefix"] = workflow.WorkflowInput{Schema: stringSchema, Required: true}
	loopDefinition.Nodes[1].Inputs["prefix"] = workflowInput("prefix")
	boundLoopPlan := compileRoundTrip(t, loopDefinition)

	boundLoopResult := runWorkflow(t, boundLoopPlan, map[string]workflow.Value{
		"count":  workflow.MustValueOf(2),
		"prefix": workflow.MustValueOf("loop-bound"),
	})
	if got := boundLoopResult.Outputs["results"].String(); got != `["loop-bound","loop-bound"]` {
		t.Fatalf("bound loop results = %s", got)
	}
}

func TestInputContractChangeRejectsCheckpointResume(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	firstDefault := workflow.MustValueOf("first")
	secondDefault := workflow.MustValueOf("second")
	first := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Default: &firstDefault,
	})
	second := inputContractDefinition(t, stringSchema, workflow.WorkflowInput{
		Schema: stringSchema, Default: &secondDefault,
	})

	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	firstPlan, err := workflow.Compile(
		t.Context(), first, registry, workflow.WithInterruptBeforeNodes(workflow.NewNodePath("end")),
	)
	if err != nil {
		t.Fatalf("Compile(first) error = %v", err)
	}

	secondPlan, err := workflow.Compile(
		t.Context(), second, registry, workflow.WithInterruptBeforeNodes(workflow.NewNodePath("end")),
	)
	if err != nil {
		t.Fatalf("Compile(second) error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "input-contract", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), firstPlan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "input-contract")

	resumed, err := runner.Resume(t.Context(), firstPlan, "input-contract", nil)
	if err != nil {
		t.Fatalf("Runner.Resume(same contract) error = %v", err)
	}

	if got := resumed.Outputs["result"].String(); got != `"first"` {
		t.Fatalf("resumed result = %s, want first", got)
	}

	_, err = runner.Resume(t.Context(), secondPlan, "input-contract", nil)
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Resume() error = %v, want ErrRun", err)
	}
}

func TestPartialRunNormalizesOnlyInputsUsedByUnmaterializedSlice(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "partial-input-scope", Revision: "v1", Name: "Partial Input Scope",
		Inputs: map[string]workflow.WorkflowInput{
			"used":   {Schema: stringSchema, Required: true},
			"unused": {Schema: stringSchema, Required: true},
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "used"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "used_action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial_used"),
				Inputs: map[string]workflow.Binding{"value": workflowInput("used")},
			},
			{
				ID: "unused_action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "partial_unused"),
				Inputs: map[string]workflow.Binding{"value": workflowInput("unused")},
			},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeParallel,
					Outputs: map[string]workflow.MergeOutputConfig{
						"used":   {Schema: stringSchema, Sources: []string{"used"}},
						"unused": {Schema: stringSchema, Sources: []string{"unused"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"used":   nodeBinding("used_action", "result"),
					"unused": nodeBinding("unused_action", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "used_action"),
			edge("start", workflow.RouteSuccess, "unused_action"),
			edge("used_action", workflow.RouteSuccess, "merge"),
			edge("unused_action", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(
		t,
		definition,
		passthroughAction("partial_used", stringSchema),
		passthroughAction("partial_unused", stringSchema),
	)
	runner := mustRunner(t)

	result, err := runner.RunPartial(t.Context(), plan, "used_action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"used": workflow.MustValueOf("selected")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := result.Outputs["result"].String(); got != `"selected"` {
		t.Fatalf("partial output = %s, want selected", got)
	}
}

func inputContractDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
	input workflow.WorkflowInput,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha2, ID: "input-contract", Revision: "v1", Name: "Input Contract",
		Inputs: map[string]workflow.WorkflowInput{"value": input},
		Outputs: map[string]workflow.OutputBinding{
			"result": {Schema: stringSchema, Binding: workflowInput("value")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges:  []workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
		Limits: workflow.DefaultLimits(),
	}
}

func passthroughAction(key string, schema workflow.PortSchema) *fakeAction {
	return &fakeAction{
		spec: actionSpec(
			workflow.ActionKey(key),
			map[string]workflow.PortSchema{"value": schema},
			map[string]workflow.PortSchema{"result": schema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
}
