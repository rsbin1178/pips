package workflow_test

import (
	"errors"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestNodePathReturnsDetachedNodes(t *testing.T) {
	t.Parallel()

	nodes := []workflow.NodeID{"first", "second"}
	path := workflow.NewNodePath(nodes...)
	nodes[0] = "changed"

	got := path.Nodes()
	if got[0] != "first" {
		t.Fatalf("NodePath.Nodes()[0] = %q, want first", got[0])
	}

	got[0] = "changed-again"

	if detached := path.Nodes(); detached[0] != "first" {
		t.Fatalf("NodePath.Nodes()[0] after caller mutation = %q, want first", detached[0])
	}
}

func TestCompileInterruptPoliciesAffectPlanFingerprint(t *testing.T) {
	t.Parallel()

	definition, registry := interruptCompileFixture(t)
	compile := func(options ...workflow.CompileOption) *workflow.Plan {
		t.Helper()

		plan, err := workflow.Compile(t.Context(), definition, registry, options...)
		if err != nil {
			t.Fatalf("Compile() error = %v", err)
		}

		return plan
	}

	base := compile()
	before := compile(workflow.WithInterruptBeforeNodes(workflow.NewNodePath("first")))
	after := compile(workflow.WithInterruptAfterNodes(workflow.NewNodePath("first")))
	both := compile(
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("first")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("first")),
	)

	fingerprints := []string{
		base.Fingerprint(), before.Fingerprint(), after.Fingerprint(), both.Fingerprint(),
	}
	for left := range fingerprints {
		for right := left + 1; right < len(fingerprints); right++ {
			if fingerprints[left] == fingerprints[right] {
				t.Fatalf("fingerprints[%d] == fingerprints[%d] = %q", left, right, fingerprints[left])
			}
		}
	}

	ordered := compile(workflow.WithInterruptBeforeNodes(
		workflow.NewNodePath("first"),
		workflow.NewNodePath("second"),
	))

	reversed := compile(workflow.WithInterruptBeforeNodes(
		workflow.NewNodePath("second"),
		workflow.NewNodePath("first"),
	))
	if ordered.Fingerprint() != reversed.Fingerprint() {
		t.Fatalf(
			"path order changed fingerprint: %q != %q",
			ordered.Fingerprint(),
			reversed.Fingerprint(),
		)
	}
}

func TestCompileRejectsInvalidInterruptPaths(t *testing.T) {
	t.Parallel()

	definition, registry := interruptCompileFixture(t)
	tests := []struct {
		name    string
		options []workflow.CompileOption
	}{
		{
			name: "empty path",
			options: []workflow.CompileOption{
				workflow.WithInterruptBeforeNodes(workflow.NewNodePath()),
			},
		},
		{
			name: "invalid node id",
			options: []workflow.CompileOption{
				workflow.WithInterruptBeforeNodes(workflow.NewNodePath("not valid")),
			},
		},
		{
			name: "unknown root node",
			options: []workflow.CompileOption{
				workflow.WithInterruptAfterNodes(workflow.NewNodePath("missing")),
			},
		},
		{
			name: "non composite intermediate node",
			options: []workflow.CompileOption{
				workflow.WithInterruptBeforeNodes(workflow.NewNodePath("first", "nested")),
			},
		},
		{
			name: "duplicate in one option",
			options: []workflow.CompileOption{
				workflow.WithInterruptBeforeNodes(
					workflow.NewNodePath("first"),
					workflow.NewNodePath("first"),
				),
			},
		},
		{
			name: "duplicate across options",
			options: []workflow.CompileOption{
				workflow.WithInterruptAfterNodes(workflow.NewNodePath("second")),
				workflow.WithInterruptAfterNodes(workflow.NewNodePath("second")),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.Compile(t.Context(), definition, registry, test.options...)
			if !errors.Is(err, workflow.ErrCompile) {
				t.Fatalf("Compile() error = %v, want ErrCompile", err)
			}
		})
	}
}

func TestCompileInterruptPathsTraverseCompositeNodes(t *testing.T) {
	t.Parallel()

	t.Run("sub workflow", func(t *testing.T) {
		t.Parallel()

		child := emptyLeafDefinition("interrupt-child")
		parent := emptySubWorkflowDefinition(
			t,
			"interrupt-parent",
			workflowRef(t, child),
		)

		registry, err := workflow.NewDefaultRegistry()
		if err != nil {
			t.Fatalf("NewDefaultRegistry() error = %v", err)
		}

		_, err = workflow.Compile(
			t.Context(),
			parent,
			registry,
			workflow.WithDefinitionResolver(staticResolver(child)),
			workflow.WithInterruptBeforeNodes(workflow.NewNodePath("sub", "end")),
		)
		if err != nil {
			t.Fatalf("Compile() error = %v", err)
		}

		_, err = workflow.Compile(
			t.Context(),
			parent,
			registry,
			workflow.WithDefinitionResolver(staticResolver(child)),
			workflow.WithInterruptBeforeNodes(workflow.NewNodePath("sub", "missing")),
		)
		if !errors.Is(err, workflow.ErrCompile) {
			t.Fatalf("Compile() unknown child error = %v, want ErrCompile", err)
		}
	})

	t.Run("batch", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		integerSchema := mustSchema(t, `{"type":"integer"}`)
		itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
		action := &fakeAction{spec: actionSpec(
			"interrupt_batch_action",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		)}
		body := batchBodyDefinition(t, stringSchema, stringSchema, "interrupt_batch_action", false)
		parent := batchParentDefinition(
			t,
			itemsSchema,
			itemsSchema,
			workflow.BatchConfig{
				Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
				ErrorMode: workflow.BatchTerminate, MaxItems: 10,
			},
			workflow.PortSchema{},
		)

		registry, err := workflow.NewDefaultRegistry(action)
		if err != nil {
			t.Fatalf("NewDefaultRegistry() error = %v", err)
		}

		_, err = workflow.Compile(
			t.Context(),
			parent,
			registry,
			workflow.WithInterruptAfterNodes(workflow.NewNodePath("batch", "map")),
		)
		if err != nil {
			t.Fatalf("Compile() error = %v", err)
		}
	})

	t.Run("loop", func(t *testing.T) {
		t.Parallel()

		integerSchema := mustSchema(t, `{"type":"integer"}`)
		countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
		action := &fakeAction{spec: actionSpec(
			"interrupt_loop_action",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		)}
		body := singleActionLoopBody(
			t,
			integerSchema,
			"interrupt_loop_action",
			"work",
		)
		parent := loopParentDefinition(
			t,
			"interrupt-loop-parent",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			workflow.LoopConfig{
				Body: body, Mode: workflow.LoopCount, MaxIterations: 10,
			},
		)

		registry, err := workflow.NewDefaultRegistry(action)
		if err != nil {
			t.Fatalf("NewDefaultRegistry() error = %v", err)
		}

		_, err = workflow.Compile(
			t.Context(),
			parent,
			registry,
			workflow.WithInterruptBeforeNodes(workflow.NewNodePath("loop", "work")),
		)
		if err != nil {
			t.Fatalf("Compile() error = %v", err)
		}
	})
}

func interruptCompileFixture(t *testing.T) (workflow.Definition, *workflow.Registry) {
	t.Helper()

	action := &fakeAction{spec: actionSpec(
		"interrupt_compile_action",
		map[string]workflow.PortSchema{},
		map[string]workflow.PortSchema{},
	)}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "interrupt-compile", Revision: "v1",
		Name:   "Interrupt Compile",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "first", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "interrupt_compile_action"),
			},
			{
				ID: "second", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "interrupt_compile_action"),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "first"),
			edge("first", workflow.RouteSuccess, "second"),
			edge("second", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, registry
}
