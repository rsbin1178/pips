package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/workflow"
)

func TestNodeErrorBindingRoundTripsAndUsesFinalFailure(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)

	var attempts atomic.Int32

	unreliable := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			attempt := attempts.Add(1)

			return workflow.Value{}, fmt.Errorf("attempt %d failed", attempt)
		},
	)
	success := constantAction("typed_success", "success", stringSchema)
	handler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{
				"message": stringSchema,
				"type":    errorTypeSchema,
			},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			message, err := workflow.DecodeValue[string](input.Values["message"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			failureType, err := workflow.DecodeValue[string](input.Values["type"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{
				Values: map[string]workflow.Value{
					"result": workflow.MustValueOf(message + "|" + failureType),
				},
			}, nil
		},
	}
	definition := failureBranchDefinition(t, stringSchema, 2)

	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	decoded, err := workflow.DecodeDefinition(encoded)
	if err != nil {
		t.Fatalf("DecodeDefinition() error = %v", err)
	}

	handlerDefinition := decoded.Nodes[3]
	if handlerDefinition.Inputs["message"].Source != workflow.BindingNodeError ||
		handlerDefinition.Inputs["type"].Port != workflow.NodeErrorTypePort {
		t.Fatalf("node error bindings changed during round trip: %#v", handlerDefinition.Inputs)
	}

	plan := compileRoundTrip(t, decoded, unreliable, success, handler)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded || attempts.Load() != 2 ||
		result.Nodes["unreliable"].Status != workflow.NodeStatusException ||
		result.Nodes["unreliable"].Failure != workflow.FailureError ||
		result.Nodes["handler"].Status != workflow.NodeStatusSucceeded ||
		result.Outputs["result"].String() != `"attempt 2 failed|error"` {
		t.Fatalf("result = %#v, attempts = %d", result, attempts.Load())
	}
}

func TestDecodeDefinitionRejectsInvalidNodeErrorBinding(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	tests := []struct {
		name   string
		mutate func(*workflow.Binding)
	}{
		{
			name: "unknown port",
			mutate: func(binding *workflow.Binding) {
				binding.Port = "stack_trace"
			},
		},
		{
			name: "literal value",
			mutate: func(binding *workflow.Binding) {
				value := workflow.MustValueOf("fabricated")
				binding.Value = &value
			},
		},
		{
			name: "missing node",
			mutate: func(binding *workflow.Binding) {
				binding.Node = ""
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition := failureBranchDefinition(t, stringSchema, 1)
			binding := definition.Nodes[3].Inputs["message"]
			test.mutate(&binding)
			definition.Nodes[3].Inputs["message"] = binding

			data, err := json.Marshal(definition)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}

			_, err = workflow.DecodeDefinition(data)
			if !errors.Is(err, workflow.ErrInvalidDefinition) {
				t.Fatalf("DecodeDefinition() error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
}

func TestCompileRejectsUnavailableNodeErrorBindings(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	unreliable := constantAction("typed_failure", "success", stringSchema)
	success := constantAction("typed_success", "success", stringSchema)
	validHandler := &fakeAction{spec: actionSpec(
		"typed_handler",
		map[string]workflow.PortSchema{
			"message": stringSchema,
			"type": mustSchema(
				t,
				`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
			),
		},
		map[string]workflow.PortSchema{"result": stringSchema},
	)}

	tests := []struct {
		name       string
		mutate     func(*workflow.Definition)
		handler    workflow.Action
		wantDetail string
	}{
		{
			name: "success path",
			mutate: func(definition *workflow.Definition) {
				definition.Edges[1].From.Route = workflow.RouteError
				definition.Edges[2].From.Route = workflow.RouteSuccess
			},
			wantDetail: "not guaranteed",
		},
		{
			name: "reconverged success and error paths",
			mutate: func(definition *workflow.Definition) {
				definition.Edges = append(
					definition.Edges,
					edge("unreliable", workflow.RouteSuccess, "handler"),
				)
			},
		},
		{
			name: "unknown source",
			mutate: func(definition *workflow.Definition) {
				binding := definition.Nodes[3].Inputs["message"]
				binding.Node = "missing"
				definition.Nodes[3].Inputs["message"] = binding
			},
			wantDetail: "unknown source node",
		},
		{
			name:   "incompatible target schema",
			mutate: func(*workflow.Definition) {},
			handler: &fakeAction{spec: actionSpec(
				"typed_handler",
				map[string]workflow.PortSchema{"message": numberSchema, "type": validHandler.spec.Inputs["type"]},
				map[string]workflow.PortSchema{"result": stringSchema},
			)},
			wantDetail: "incompatible schema",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition := failureBranchDefinition(t, stringSchema, 1)
			test.mutate(&definition)

			handler := test.handler
			if handler == nil {
				handler = validHandler
			}

			registry, err := workflow.NewDefaultRegistry(unreliable, success, handler)
			if err != nil {
				t.Fatalf("NewDefaultRegistry() error = %v", err)
			}

			_, err = workflow.Compile(t.Context(), definition, registry)
			if !errors.Is(err, workflow.ErrCompile) {
				t.Fatalf("Compile() error = %v, want ErrCompile", err)
			}

			if test.wantDetail != "" && !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("Compile() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestNodeErrorBindingSupportsRouteAwareMerge(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	failing := resultAction(
		"merge_source",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("merge failure")
		},
	)

	exclusive := directErrorMergeDefinition(t, stringSchema, workflow.MergeExclusive)
	exclusivePlan := compileRoundTrip(t, exclusive, failing)
	runner := mustRunner(t)

	exclusiveResult, err := runner.Run(t.Context(), exclusivePlan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run(exclusive) error = %v", err)
	}

	if exclusiveResult.Status != workflow.RunStatusPartialSucceeded ||
		exclusiveResult.Outputs["result"].String() != `"merge failure"` {
		t.Fatalf("exclusive result = %#v", exclusiveResult)
	}

	parallel := guaranteedParallelErrorMergeDefinition(t, stringSchema)
	parallelPlan := compileRoundTrip(
		t,
		parallel,
		failing,
		constantAction("merge_success", "success", stringSchema),
		constantAction("merge_side", "side", stringSchema),
	)

	parallelResult, err := runner.Run(t.Context(), parallelPlan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run(parallel) error = %v", err)
	}

	if parallelResult.Status != workflow.RunStatusPartialSucceeded ||
		parallelResult.Outputs["result"].String() != `"merge failure"` {
		t.Fatalf("parallel result = %#v", parallelResult)
	}
}

func TestCompileRejectsParallelMergeWithConditionalNodeError(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := directErrorMergeDefinition(t, stringSchema, workflow.MergeParallel)

	registry, err := workflow.NewDefaultRegistry(
		constantAction("merge_source", "success", stringSchema),
	)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(t.Context(), definition, registry)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "not guaranteed") {
		t.Fatalf("Compile() error = %v, want non-guaranteed parallel error input", err)
	}
}

func failureBranchDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
	maxAttempts int,
) workflow.Definition {
	t.Helper()

	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {
				Schema:  stringSchema,
				Sources: []string{"success", "failure"},
			},
		},
	}

	return workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "typed-failure",
		Revision: "v1",
		Name:     "Typed Failure",
		Inputs:   map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID:      "unreliable",
				Type:    workflow.NodeTypeAction,
				Version: workflow.BuiltinNodeVersion,
				Config:  actionConfig(t, "typed_failure"),
				Policy: workflow.NodePolicy{
					Retry: workflow.RetryPolicy{MaxAttempts: maxAttempts},
					Error: workflow.ErrorRoute,
				},
			},
			{
				ID:      "success",
				Type:    workflow.NodeTypeAction,
				Version: workflow.BuiltinNodeVersion,
				Config:  actionConfig(t, "typed_success"),
			},
			{
				ID:      "handler",
				Type:    workflow.NodeTypeAction,
				Version: workflow.BuiltinNodeVersion,
				Config:  actionConfig(t, "typed_handler"),
				Inputs: map[string]workflow.Binding{
					"message": nodeErrorBinding("unreliable", workflow.NodeErrorMessagePort),
					"type":    nodeErrorBinding("unreliable", workflow.NodeErrorTypePort),
				},
			},
			{
				ID:      "merge",
				Type:    workflow.NodeTypeMerge,
				Version: workflow.BuiltinNodeVersion,
				Config:  mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"success": nodeBinding("success", "result"),
					"failure": nodeBinding("handler", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "unreliable"),
			edge("unreliable", workflow.RouteSuccess, "success"),
			edge("unreliable", workflow.RouteError, "handler"),
			edge("success", workflow.RouteSuccess, "merge"),
			edge("handler", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func directErrorMergeDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
	mode workflow.MergeMode,
) workflow.Definition {
	t.Helper()

	outputs := map[string]workflow.MergeOutputConfig{
		"result": {
			Schema:  stringSchema,
			Sources: []string{"success", "failure"},
		},
	}
	workflowOutput := "result"

	if mode == workflow.MergeParallel {
		outputs = map[string]workflow.MergeOutputConfig{
			"success": {Schema: stringSchema, Sources: []string{"success"}},
			"failure": {Schema: stringSchema, Sources: []string{"failure"}},
		}
		workflowOutput = "failure"
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "direct-error-merge", Revision: "v1",
		Name:   "Direct Error Merge",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", workflowOutput),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "source", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_source"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{Mode: mode, Outputs: outputs}),
				Inputs: map[string]workflow.Binding{
					"success": nodeBinding("source", "result"),
					"failure": nodeErrorBinding("source", workflow.NodeErrorMessagePort),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "source"),
			edge("source", workflow.RouteSuccess, "merge"),
			edge("source", workflow.RouteError, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func guaranteedParallelErrorMergeDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "parallel-error-merge", Revision: "v1",
		Name:   "Parallel Error Merge",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "final", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "source", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_source"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{
				ID: "error_merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeParallel,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"failure"}},
						"side":   {Schema: stringSchema, Sources: []string{"side"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"failure": nodeErrorBinding("source", workflow.NodeErrorMessagePort),
					"side":    nodeBinding("error_side", "result"),
				},
			},
			{
				ID: "error_side", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_side"),
			},
			{
				ID: "success", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_success"),
			},
			{
				ID: "final", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"success", "failure"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"success": nodeBinding("success", "result"),
					"failure": nodeBinding("error_merge", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "source"),
			edge("source", workflow.RouteSuccess, "success"),
			edge("source", workflow.RouteError, "error_merge"),
			edge("source", workflow.RouteError, "error_side"),
			edge("error_side", workflow.RouteSuccess, "error_merge"),
			edge("success", workflow.RouteSuccess, "final"),
			edge("error_merge", workflow.RouteSuccess, "final"),
			edge("final", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func nodeErrorBinding(node workflow.NodeID, port string) workflow.Binding {
	return workflow.Binding{Source: workflow.BindingNodeError, Node: node, Port: port}
}
