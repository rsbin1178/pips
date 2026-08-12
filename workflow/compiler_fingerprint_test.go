package workflow_test

import (
	"context"
	"sync"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestPlanFingerprintIgnoresUnusedRegistryContracts(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	usedAction := resultAction("used", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.MustValueOf("used"), nil
	})
	definition := singleActionDefinition(t, "used", stringSchema, workflow.NodePolicy{})

	baseRegistry, err := workflow.NewDefaultRegistry(usedAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	unusedAction := &fakeAction{spec: actionSpec(
		"unused",
		map[string]workflow.PortSchema{},
		map[string]workflow.PortSchema{"result": stringSchema},
	)}

	nodeTypes := append(workflow.BuiltinNodeTypes(), fingerprintNodeType{
		key: "fingerprint_unused",
	})

	expandedRegistry, err := workflow.NewRegistry(
		nodeTypes,
		[]workflow.Action{usedAction, unusedAction},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	basePlan := compileFingerprintPlan(t, definition, baseRegistry)
	expandedPlan := compileFingerprintPlan(t, definition, expandedRegistry)

	if baseRegistry.Fingerprint() == expandedRegistry.Fingerprint() {
		t.Fatal("Registry fingerprints are equal after adding unused contracts")
	}

	if basePlan.RegistryFingerprint() != baseRegistry.Fingerprint() {
		t.Fatalf("base Plan.RegistryFingerprint() = %q, want %q", basePlan.RegistryFingerprint(), baseRegistry.Fingerprint())
	}

	if expandedPlan.RegistryFingerprint() != expandedRegistry.Fingerprint() {
		t.Fatalf("expanded Plan.RegistryFingerprint() = %q, want %q", expandedPlan.RegistryFingerprint(), expandedRegistry.Fingerprint())
	}

	if basePlan.Fingerprint() != expandedPlan.Fingerprint() {
		t.Fatalf(
			"Plan fingerprints differ after adding unused contracts: base=%q expanded=%q",
			basePlan.Fingerprint(),
			expandedPlan.Fingerprint(),
		)
	}
}

func TestPlanFingerprintIncludesObservedActionLookups(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	definition := fingerprintNodeDefinition("fingerprint_lookup")
	nodeType := fingerprintNodeType{
		key: "fingerprint_lookup",
		compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
			compileContext.Action("optional", "v1")

			return emptyFingerprintNodeSpec()
		},
	}

	absentRegistry := newFingerprintRegistry(t, nodeType, nil)
	presentStringRegistry := newFingerprintRegistry(t, nodeType, []workflow.Action{
		fingerprintAction("optional", stringSchema),
	})
	presentNumberRegistry := newFingerprintRegistry(t, nodeType, []workflow.Action{
		fingerprintAction("optional", numberSchema),
	})

	absentPlan := compileFingerprintPlan(t, definition, absentRegistry)
	presentStringPlan := compileFingerprintPlan(t, definition, presentStringRegistry)
	presentNumberPlan := compileFingerprintPlan(t, definition, presentNumberRegistry)

	if absentPlan.Fingerprint() == presentStringPlan.Fingerprint() {
		t.Fatal("Plan fingerprint did not change when an observed absent Action became present")
	}

	if presentStringPlan.Fingerprint() == presentNumberPlan.Fingerprint() {
		t.Fatal("Plan fingerprint did not change when an observed Action schema changed")
	}

	for index := range 3 {
		recompiled := compileFingerprintPlan(t, definition, absentRegistry)
		if recompiled.Fingerprint() != absentPlan.Fingerprint() {
			t.Fatalf(
				"recompiled fingerprint[%d] = %q, want %q",
				index,
				recompiled.Fingerprint(),
				absentPlan.Fingerprint(),
			)
		}
	}
}

func TestPlanFingerprintCanonicalizesActionLookupOrderAndDuplicates(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := fingerprintNodeDefinition("fingerprint_lookup_order")
	actions := []workflow.Action{
		fingerprintAction("first", stringSchema),
		fingerprintAction("second", stringSchema),
	}
	forward := fingerprintNodeType{
		key: "fingerprint_lookup_order",
		compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
			compileContext.Action("first", "v1")
			compileContext.Action("second", "v1")
			compileContext.Action("first", "v1")

			return emptyFingerprintNodeSpec()
		},
	}
	reverse := fingerprintNodeType{
		key: "fingerprint_lookup_order",
		compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
			compileContext.Action("second", "v1")
			compileContext.Action("first", "v1")

			return emptyFingerprintNodeSpec()
		},
	}

	forwardPlan := compileFingerprintPlan(t, definition, newFingerprintRegistry(t, forward, actions))
	reversePlan := compileFingerprintPlan(t, definition, newFingerprintRegistry(t, reverse, actions))

	if forwardPlan.Fingerprint() != reversePlan.Fingerprint() {
		t.Fatalf(
			"lookup order changed Plan fingerprint: forward=%q reverse=%q",
			forwardPlan.Fingerprint(),
			reversePlan.Fingerprint(),
		)
	}
}

func TestPlanFingerprintIncludesCompiledNodeSpec(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := fingerprintNodeDefinition("fingerprint_spec")
	emptySpecType := fingerprintNodeType{key: "fingerprint_spec"}
	outputSpecType := fingerprintNodeType{
		key: "fingerprint_spec",
		compileNode: func(workflow.CompileContext) workflow.NodeSpec {
			return workflow.NodeSpec{
				Inputs:  map[string]workflow.PortSchema{},
				Outputs: map[string]workflow.PortSchema{"result": stringSchema},
				Routes:  []string{workflow.RouteSuccess},
			}
		},
	}

	emptyPlan := compileFingerprintPlan(
		t,
		definition,
		newFingerprintRegistry(t, emptySpecType, nil),
	)
	outputPlan := compileFingerprintPlan(
		t,
		definition,
		newFingerprintRegistry(t, outputSpecType, nil),
	)

	if emptyPlan.Fingerprint() == outputPlan.Fingerprint() {
		t.Fatal("Plan fingerprint did not change when the compiled NodeSpec changed")
	}
}

func TestPlanFingerprintConcurrentActionLookups(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	nodeType := fingerprintNodeType{
		key: "fingerprint_concurrent",
		compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
			var lookups sync.WaitGroup
			for index := range 32 {
				lookups.Add(1)
				go func(index int) {
					defer lookups.Done()

					if index%2 == 0 {
						compileContext.Action("present", "v1")

						return
					}

					compileContext.Action("absent", "v1")
				}(index)
			}

			lookups.Wait()

			return emptyFingerprintNodeSpec()
		},
	}
	registry := newFingerprintRegistry(t, nodeType, []workflow.Action{
		fingerprintAction("present", stringSchema),
	})
	definition := fingerprintNodeDefinition("fingerprint_concurrent")
	expected := compileFingerprintPlan(t, definition, registry).Fingerprint()

	const compiles = 16

	results := make(chan string, compiles)
	errors := make(chan error, compiles)

	var workers sync.WaitGroup
	for range compiles {
		workers.Go(func() {
			plan, err := workflow.Compile(context.Background(), definition, registry)
			if err != nil {
				errors <- err

				return
			}

			results <- plan.Fingerprint()
		})
	}

	workers.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Fatalf("Compile() error = %v", err)
	}

	for fingerprint := range results {
		if fingerprint != expected {
			t.Fatalf("concurrent fingerprint = %q, want %q", fingerprint, expected)
		}
	}
}

func TestCompileContextRejectsActionLookupAfterCompile(t *testing.T) {
	t.Parallel()

	captured := make(chan workflow.CompileContext, 1)
	nodeType := fingerprintNodeType{
		key: "fingerprint_late_lookup",
		compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
			captured <- compileContext

			return emptyFingerprintNodeSpec()
		},
	}
	registry := newFingerprintRegistry(t, nodeType, []workflow.Action{
		fingerprintAction("late", mustSchema(t, `{"type":"string"}`)),
	})
	compileFingerprintPlan(
		t,
		fingerprintNodeDefinition("fingerprint_late_lookup"),
		registry,
	)

	compileContext := <-captured
	if action, ok := compileContext.Action("late", "v1"); ok || action != nil {
		t.Fatalf("late CompileContext.Action() = %T, %v, want nil, false", action, ok)
	}
}

type fingerprintNodeType struct {
	key         workflow.NodeTypeKey
	compileNode func(workflow.CompileContext) workflow.NodeSpec
	invoke      func()
}

func (n fingerprintNodeType) Spec() workflow.NodeTypeSpec {
	return workflow.NodeTypeSpec{Key: n.key, Version: "v1", DisplayName: "Fingerprint Test"}
}

func (n fingerprintNodeType) Compile(
	_ context.Context,
	compileContext workflow.CompileContext,
	_ workflow.NodeDefinition,
) (workflow.CompiledNode, error) {
	if n.compileNode == nil {
		return fingerprintCompiledNode{
			spec:   emptyFingerprintNodeSpec(),
			invoke: n.invoke,
		}, nil
	}

	return fingerprintCompiledNode{
		spec:   n.compileNode(compileContext),
		invoke: n.invoke,
	}, nil
}

type fingerprintCompiledNode struct {
	spec   workflow.NodeSpec
	invoke func()
}

func (n fingerprintCompiledNode) Spec() workflow.NodeSpec {
	return n.spec
}

func (n fingerprintCompiledNode) Invoke(
	context.Context,
	workflow.NodeInput,
) (workflow.NodeOutput, error) {
	if n.invoke != nil {
		n.invoke()
	}

	return workflow.NodeOutput{
		Values: map[string]workflow.Value{},
		Route:  workflow.RouteSuccess,
	}, nil
}

func emptyFingerprintNodeSpec() workflow.NodeSpec {
	return workflow.NodeSpec{
		Inputs:  map[string]workflow.PortSchema{},
		Outputs: map[string]workflow.PortSchema{},
		Routes:  []string{workflow.RouteSuccess},
	}
}

func fingerprintAction(key workflow.ActionKey, schema workflow.PortSchema) workflow.Action {
	return &fakeAction{spec: actionSpec(
		key,
		map[string]workflow.PortSchema{"value": schema},
		map[string]workflow.PortSchema{},
	)}
}

func fingerprintNodeDefinition(nodeType workflow.NodeTypeKey) workflow.Definition {
	return workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "fingerprint",
		Revision: "v1",
		Name:     "Fingerprint",
		Inputs:   map[string]workflow.WorkflowInput{},
		Outputs:  map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "custom", Type: nodeType, Version: "v1"},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "custom"),
			edge("custom", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func newFingerprintRegistry(
	t *testing.T,
	nodeType workflow.NodeType,
	actions []workflow.Action,
) *workflow.Registry {
	t.Helper()

	nodeTypes := append(workflow.BuiltinNodeTypes(), nodeType)

	registry, err := workflow.NewRegistry(nodeTypes, actions)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	return registry
}

func compileFingerprintPlan(
	t *testing.T,
	definition workflow.Definition,
	registry *workflow.Registry,
) *workflow.Plan {
	t.Helper()

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return plan
}
