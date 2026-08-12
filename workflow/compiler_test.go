package workflow_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestCompileRejectsInvalidDefinitions(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	validAction := &fakeAction{
		spec: actionSpec("action", map[string]workflow.PortSchema{"value": stringSchema}, map[string]workflow.PortSchema{"result": stringSchema}),
	}
	wrongInputAction := &fakeAction{
		spec: actionSpec("action", map[string]workflow.PortSchema{"value": numberSchema}, map[string]workflow.PortSchema{"result": stringSchema}),
	}

	base := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "compile", Revision: "v1", Name: "Compile",
		Inputs:  map[string]workflow.WorkflowInput{"input": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "action", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "action"), Inputs: map[string]workflow.Binding{"value": workflowInput("input")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	tests := []struct {
		name       string
		definition workflow.Definition
		action     workflow.Action
	}{
		{
			name: "unknown action version",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Nodes[1].Config = mustJSON(t, workflow.ActionConfig{Action: "action", Version: "v2"})
			}),
			action: validAction,
		},
		{
			name: "unknown node version",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Nodes[1].Version = "v2"
			}),
			action: validAction,
		},
		{
			name: "unknown route",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Edges[0].From.Route = "unknown"
			}),
			action: validAction,
		},
		{
			name: "unreachable node",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Nodes = append(definition.Nodes, workflow.NodeDefinition{
					ID:      "orphan",
					Type:    workflow.NodeTypeAction,
					Version: workflow.BuiltinNodeVersion,
					Config:  actionConfig(t, "action"),
					Inputs:  map[string]workflow.Binding{"value": workflowInput("input")},
				})
			}),
			action: validAction,
		},
		{
			name:       "port schema mismatch",
			definition: base,
			action:     wrongInputAction,
		},
		{
			name: "undeclared binding path",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Nodes[1].Inputs = map[string]workflow.Binding{
					"value": {
						Source: workflow.BindingWorkflowInput,
						Port:   "input",
						Path:   []string{"missing"},
					},
				}
			}),
			action: validAction,
		},
		{
			name: "invalid default output",
			definition: mutateDefinition(base, func(definition *workflow.Definition) {
				definition.Nodes[1].Policy = workflow.NodePolicy{
					Error: workflow.ErrorContinueWithDefault,
					DefaultOutputs: map[string]workflow.Value{
						"result": workflow.MustValueOf(42),
					},
				}
			}),
			action: validAction,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			registry, err := workflow.NewDefaultRegistry(test.action)
			if err != nil {
				t.Fatalf("NewDefaultRegistry() error = %v", err)
			}

			_, err = workflow.Compile(t.Context(), test.definition, registry)
			if !errors.Is(err, workflow.ErrCompile) {
				t.Fatalf("Compile() error = %v, want ErrCompile", err)
			}
		})
	}
}

func TestCompileRejectsImplicitJoin(t *testing.T) {
	t.Parallel()

	boolSchema := mustSchema(t, `{"type":"boolean"}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	trueValue := workflow.MustValueOf(true)
	conditionConfig := workflow.ConditionConfig{
		Predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "value", Value: &trueValue},
		TrueRoute: "yes", FalseRoute: "no",
	}
	action := constantAction("accept", "accepted", stringSchema)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "implicit", Revision: "v1", Name: "Implicit Join",
		Inputs:  map[string]workflow.WorkflowInput{"value": {Schema: boolSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "accept", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, conditionConfig), Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{ID: "accept", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "accept")},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "yes", "accept"),
			edge("condition", "no", "end"),
			edge("accept", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(t.Context(), definition, registry)
	if !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("Compile() error = %v, want ErrCompile", err)
	}
}

func TestCompileRejectsOutputOnErrorRoute(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	unreliable := constantAction("unreliable", "primary", stringSchema)
	fallback := &fakeAction{spec: actionSpec(
		"fallback",
		map[string]workflow.PortSchema{"value": stringSchema},
		map[string]workflow.PortSchema{"result": stringSchema},
	)}
	merge := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"primary", "fallback"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "error-binding", Revision: "v1", Name: "Error Binding",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "merge", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "unreliable", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "unreliable"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{
				ID: "fallback", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "fallback"),
				Inputs: map[string]workflow.Binding{"value": nodeBinding("unreliable", "result")},
			},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, merge),
				Inputs: map[string]workflow.Binding{
					"primary":  nodeBinding("unreliable", "result"),
					"fallback": nodeBinding("fallback", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "unreliable"),
			edge("unreliable", workflow.RouteSuccess, "merge"),
			edge("unreliable", workflow.RouteError, "fallback"),
			edge("fallback", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, err := workflow.NewDefaultRegistry(unreliable, fallback)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(t.Context(), definition, registry)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "not guaranteed") {
		t.Fatalf("Compile() error = %v, want non-guaranteed output failure", err)
	}
}

func TestCompileRejectsCycle(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	actionA := constantAction("a", "a", stringSchema)
	actionB := constantAction("b", "b", stringSchema)
	mergeConfig := func(first, second string) workflow.MergeConfig {
		return workflow.MergeConfig{
			Mode: workflow.MergeParallel,
			Outputs: map[string]workflow.MergeOutputConfig{
				"first":  {Schema: stringSchema, Sources: []string{first}},
				"second": {Schema: stringSchema, Sources: []string{second}},
			},
		}
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "cycle", Revision: "v1", Name: "Cycle",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "merge-b", "first")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "a", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "a")},
			{ID: "b", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "b")},
			{
				ID: "merge-a", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig("a", "cycle")),
				Inputs: map[string]workflow.Binding{
					"a":     nodeBinding("a", "result"),
					"cycle": nodeBinding("merge-b", "first"),
				},
			},
			{
				ID: "merge-b", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig("b", "cycle")),
				Inputs: map[string]workflow.Binding{
					"b":     nodeBinding("b", "result"),
					"cycle": nodeBinding("merge-a", "first"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "a"),
			edge("start", workflow.RouteSuccess, "b"),
			edge("a", workflow.RouteSuccess, "merge-a"),
			edge("merge-b", workflow.RouteSuccess, "merge-a"),
			edge("b", workflow.RouteSuccess, "merge-b"),
			edge("merge-a", workflow.RouteSuccess, "merge-b"),
			edge("merge-b", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, err := workflow.NewDefaultRegistry(actionA, actionB)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(t.Context(), definition, registry)
	if !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("Compile() error = %v, want ErrCompile", err)
	}

	if !strings.Contains(err.Error(), "DAG") {
		t.Fatalf("Compile() error = %v, want DAG cycle failure", err)
	}
}

func mutateDefinition(
	definition workflow.Definition,
	mutate func(*workflow.Definition),
) workflow.Definition {
	cloned := definition
	cloned.Nodes = append([]workflow.NodeDefinition(nil), definition.Nodes...)
	cloned.Edges = append([]workflow.ControlEdge(nil), definition.Edges...)
	mutate(&cloned)

	return cloned
}

type testCompileContext struct {
	inputs  map[string]workflow.PortSchema
	outputs map[string]workflow.PortSchema
}

func (c testCompileContext) WorkflowInputs() map[string]workflow.PortSchema {
	return c.inputs
}

func (c testCompileContext) WorkflowOutputs() map[string]workflow.PortSchema {
	return c.outputs
}

func (testCompileContext) Action(workflow.ActionKey, string) (workflow.Action, bool) {
	return nil, false
}

var _ workflow.CompileContext = testCompileContext{}

func compileCondition(
	t *testing.T,
	predicate workflow.Predicate,
	inputs map[string]workflow.Value,
) bool {
	t.Helper()

	bindings := make(map[string]workflow.Binding, len(inputs))
	for name, value := range inputs {
		bindings[name] = workflow.Binding{Source: workflow.BindingLiteral, Value: &value}
	}

	definition := workflow.NodeDefinition{
		ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
		Config: mustJSON(t, workflow.ConditionConfig{Predicate: predicate, TrueRoute: "yes", FalseRoute: "no"}),
		Inputs: bindings,
	}

	executor, err := (workflow.ConditionNode{}).Compile(t.Context(), testCompileContext{}, definition)
	if err != nil {
		t.Fatalf("ConditionNode.Compile() error = %v", err)
	}

	output, err := executor.Invoke(t.Context(), workflow.NodeInput{Values: inputs})
	if err != nil {
		t.Fatalf("Condition Invoke() error = %v", err)
	}

	return output.Route == "yes"
}

func TestConditionPredicateOperators(t *testing.T) {
	t.Parallel()

	five := workflow.MustValueOf(5)
	six := workflow.MustValueOf(6)
	seven := workflow.MustValueOf(7)
	name := workflow.MustValueOf("Ada")
	inputs := map[string]workflow.Value{
		"number": five,
		"object": workflow.MustValueOf(map[string]any{"name": "Ada"}),
	}

	tests := []struct {
		name      string
		predicate workflow.Predicate
		expected  bool
	}{
		{name: "exists", predicate: workflow.Predicate{Op: workflow.PredicateExists, Input: "object", Path: []string{"name"}}, expected: true},
		{name: "equal", predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "number", Value: &five}, expected: true},
		{name: "not equal", predicate: workflow.Predicate{Op: workflow.PredicateNotEqual, Input: "number", Value: &six}, expected: true},
		{name: "greater", predicate: workflow.Predicate{Op: workflow.PredicateGreater, Input: "number", Value: &five}, expected: false},
		{name: "greater or equal", predicate: workflow.Predicate{Op: workflow.PredicateGreaterOrEqual, Input: "number", Value: &five}, expected: true},
		{name: "less", predicate: workflow.Predicate{Op: workflow.PredicateLess, Input: "number", Value: &six}, expected: true},
		{name: "less or equal", predicate: workflow.Predicate{Op: workflow.PredicateLessOrEqual, Input: "number", Value: &five}, expected: true},
		{name: "in", predicate: workflow.Predicate{Op: workflow.PredicateIn, Input: "number", Values: []workflow.Value{six, five}}, expected: true},
		{
			name: "all", expected: true,
			predicate: workflow.Predicate{Op: workflow.PredicateAll, Args: []workflow.Predicate{
				{Op: workflow.PredicateEqual, Input: "number", Value: &five},
				{Op: workflow.PredicateEqual, Input: "object", Path: []string{"name"}, Value: &name},
			}},
		},
		{
			name: "any", expected: true,
			predicate: workflow.Predicate{Op: workflow.PredicateAny, Args: []workflow.Predicate{
				{Op: workflow.PredicateEqual, Input: "number", Value: &seven},
				{Op: workflow.PredicateEqual, Input: "number", Value: &five},
			}},
		},
		{
			name: "not", expected: true,
			predicate: workflow.Predicate{Op: workflow.PredicateNot, Args: []workflow.Predicate{
				{Op: workflow.PredicateEqual, Input: "number", Value: &six},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if actual := compileCondition(t, test.predicate, inputs); actual != test.expected {
				t.Fatalf("predicate result = %v, want %v", actual, test.expected)
			}
		})
	}
}

func TestConditionRejectsStringExpression(t *testing.T) {
	t.Parallel()

	definition := workflow.NodeDefinition{
		ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
		Config: []byte(`{"expression":"value == true","true_route":"yes","false_route":"no"}`),
		Inputs: map[string]workflow.Binding{},
	}

	_, err := (workflow.ConditionNode{}).Compile(t.Context(), testCompileContext{}, definition)
	if !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("ConditionNode.Compile() error = %v, want ErrCompile", err)
	}
}

func TestMergeExclusiveRejectsMultipleValues(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	first := workflow.MustValueOf("first")
	second := workflow.MustValueOf("second")
	definition := workflow.NodeDefinition{
		ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
		Config: mustJSON(t, workflow.MergeConfig{
			Mode: workflow.MergeExclusive,
			Outputs: map[string]workflow.MergeOutputConfig{
				"result": {Schema: stringSchema, Sources: []string{"first", "second"}},
			},
		}),
		Inputs: map[string]workflow.Binding{
			"first":  {Source: workflow.BindingLiteral, Value: &first},
			"second": {Source: workflow.BindingLiteral, Value: &second},
		},
	}

	executor, err := (workflow.MergeNode{}).Compile(t.Context(), testCompileContext{}, definition)
	if err != nil {
		t.Fatalf("MergeNode.Compile() error = %v", err)
	}

	_, err = executor.Invoke(t.Context(), workflow.NodeInput{Values: map[string]workflow.Value{
		"first": first, "second": second,
	}})
	if err == nil {
		t.Fatal("Merge Invoke() error = nil, want conflict")
	}
}
