package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestPrepareNodeDebugRejectsInvalidAndContextOnlyTargets(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "debug-target", stringSchema, workflow.NodePolicy{}),
		constantAction("debug-target", "ok", stringSchema),
	)

	tests := []workflow.NodePath{
		workflow.NewNodePath(),
		workflow.NewNodePath("missing"),
		workflow.NewNodePath("start"),
		workflow.NewNodePath("end"),
		workflow.NewNodePath("action", "child"),
	}
	for _, target := range tests {
		if _, err := workflow.PrepareNodeDebug(plan, target); !errors.Is(err, workflow.ErrCompile) {
			t.Fatalf("PrepareNodeDebug(%v) error = %v, want ErrCompile", target.Nodes(), err)
		}
	}

	if _, err := workflow.PrepareNodeDebug(nil, workflow.NewNodePath("action")); !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("PrepareNodeDebug(nil) error = %v, want ErrCompile", err)
	}
}

func TestPrepareNodeDebugRejectsStandaloneLoopContextNodes(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	integerArraySchema := mustSchema(t, `{"type":"array","items":{"type":"integer"}}`)
	increment := &fakeAction{
		spec: actionSpec("increment_loop_variable", map[string]workflow.PortSchema{
			"value": integerSchema,
		}, map[string]workflow.PortSchema{"next": integerSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"next": input.Values["value"],
			}}, nil
		},
	}
	body := statefulLoopBody(t, integerSchema)
	definition := loopParentDefinition(
		t,
		"debug-loop-context",
		map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
		map[string]workflow.Binding{
			"count": workflowInput("count"), "counter": workflowInput("counter"),
		},
		map[string]workflow.OutputBinding{
			"values": nodeOutput(integerArraySchema, "loop", "values"),
			"final":  nodeOutput(integerSchema, "loop", "final"),
		},
		workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount,
			Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
			Outputs: []workflow.LoopOutput{
				{Name: "values", Source: workflow.LoopOutputBody, Port: "value"},
				{Name: "final", Source: workflow.LoopOutputVariable, Port: "counter"},
			},
			MaxIterations: 10,
		},
	)
	plan := compileRoundTrip(t, definition, increment)

	for _, nodeID := range []workflow.NodeID{"set", "break", "continue"} {
		_, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("loop", nodeID))
		if !errors.Is(err, workflow.ErrCompile) {
			t.Fatalf("PrepareNodeDebug(loop/%s) error = %v, want ErrCompile", nodeID, err)
		}
	}
}

func TestNodeDebugPlanGettersAreDetachedAndDeterministic(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "debug-detach", stringSchema, workflow.NodePolicy{}),
		constantAction("debug-detach", "ok", stringSchema),
	)

	first, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	second, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug(second) error = %v", err)
	}

	if first.Fingerprint() != second.Fingerprint() || first.Fingerprint() == plan.Fingerprint() {
		t.Fatalf("fingerprints first=%s second=%s source=%s", first.Fingerprint(), second.Fingerprint(), plan.Fingerprint())
	}

	spec := first.Spec()
	delete(spec.Outputs, "result")
	spec.Routes[0] = "mutated"
	path := first.Target().Nodes()
	path[0] = "mutated"

	detached := first.Spec()
	if !detached.Outputs["result"].IsValid() || detached.Routes[0] != workflow.RouteSuccess ||
		first.Target().Nodes()[0] != "action" {
		t.Fatalf("NodeDebugPlan getters were not detached: %#v %#v", detached, first.Target().Nodes())
	}
}

func TestDebugNodeBindingPathAcceptsFinalPortValue(t *testing.T) {
	t.Parallel()

	objectSchema := mustSchema(t, `{
		"type":"object",
		"properties":{"name":{"type":"string"}},
		"required":["name"],
		"additionalProperties":false
	}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	upstream := &fakeAction{
		spec: actionSpec("debug-object", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"object": objectSchema,
		}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"object": workflow.MustValueOf(map[string]string{"name": "upstream"}),
			}}, nil
		},
	}
	target := &fakeAction{
		spec: actionSpec("debug-path", map[string]workflow.PortSchema{
			"name": stringSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["name"],
			}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-path", Revision: "v1", Name: "Debug Path",
		Inputs: map[string]workflow.PortSchema{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "target", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "upstream", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "debug-object")},
			{
				ID: "target", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "debug-path"), Inputs: map[string]workflow.Binding{
					"name": {
						Source: workflow.BindingNodeOutput, Node: "upstream", Port: "object",
						Path: []string{"name"},
					},
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

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"name": workflow.MustValueOf("final-value"),
	})
	if err != nil || result.Execution.Outputs["result"].String() != `"final-value"` {
		t.Fatalf("DebugNode() result=%#v error=%v", result, err)
	}
}

func TestPrepareNodeDebugSupportsRegisteredCustomNode(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	nodeType := debugCustomNodeType{schema: stringSchema}
	nodeTypes := append(workflow.BuiltinNodeTypes(), nodeType)

	registry, err := workflow.NewRegistry(nodeTypes, nil)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-custom", Revision: "v1", Name: "Debug Custom",
		Inputs: map[string]workflow.PortSchema{"value": stringSchema},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "custom", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "custom", Type: "debug_custom", Version: "v1",
				Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "custom"),
			edge("custom", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("custom"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"value": workflow.MustValueOf("custom"),
	})
	if err != nil || result.Execution.Outputs["result"].String() != `"custom"` {
		t.Fatalf("DebugNode() result=%#v error=%v", result, err)
	}
}

type debugCustomNodeType struct {
	schema workflow.PortSchema
}

func (n debugCustomNodeType) Spec() workflow.NodeTypeSpec {
	return workflow.NodeTypeSpec{Key: "debug_custom", Version: "v1", DisplayName: "Debug Custom"}
}

func (n debugCustomNodeType) Compile(
	context.Context,
	workflow.CompileContext,
	workflow.NodeDefinition,
) (workflow.CompiledNode, error) {
	return debugCustomNode(n), nil
}

type debugCustomNode struct {
	schema workflow.PortSchema
}

func (n debugCustomNode) Spec() workflow.NodeSpec {
	return workflow.NodeSpec{
		Inputs:  map[string]workflow.PortSchema{"value": n.schema},
		Outputs: map[string]workflow.PortSchema{"result": n.schema},
		Routes:  []string{workflow.RouteSuccess},
	}
}

func (n debugCustomNode) Invoke(
	_ context.Context,
	input workflow.NodeInput,
) (workflow.NodeOutput, error) {
	return workflow.NodeOutput{
		Values: map[string]workflow.Value{"result": input.Values["value"]},
		Route:  workflow.RouteSuccess,
	}, nil
}
