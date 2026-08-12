package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/workflow"
)

type diagnosticNodeType struct {
	key     workflow.NodeTypeKey
	spec    workflow.NodeSpec
	called  *atomic.Int64
	compile func(context.Context) (workflow.CompiledNode, error)
}

func (n diagnosticNodeType) Spec() workflow.NodeTypeSpec {
	return workflow.NodeTypeSpec{Key: n.key, Version: "v1", DisplayName: string(n.key)}
}

func (n diagnosticNodeType) Compile(
	ctx context.Context,
	_ workflow.CompileContext,
	_ workflow.NodeDefinition,
) (workflow.CompiledNode, error) {
	if n.called != nil {
		n.called.Add(1)
	}

	if n.compile != nil {
		return n.compile(ctx)
	}

	return diagnosticCompiledNode{spec: n.spec}, nil
}

type diagnosticCompiledNode struct {
	spec workflow.NodeSpec
}

func (n diagnosticCompiledNode) Spec() workflow.NodeSpec {
	return n.spec
}

func (diagnosticCompiledNode) Invoke(
	context.Context,
	workflow.NodeInput,
) (workflow.NodeOutput, error) {
	return workflow.NodeOutput{}, nil
}

func TestCompileDiagnosticsAggregateUnknownNodeTypesBeforeConstruction(t *testing.T) {
	t.Parallel()

	var called atomic.Int64

	probe := diagnosticNodeType{
		key:    "diagnostic_probe",
		called: &called,
		spec: workflow.NodeSpec{
			Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.PortSchema{},
			Routes: []string{workflow.RouteSuccess},
		},
	}
	registry := diagnosticRegistry(t, probe)
	definition := emptyDiagnosticDefinition(
		"unknown_types",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "missing_first", Type: "missing_first", Version: "v1"},
			{ID: "probe", Type: probe.key, Version: "v1"},
			{ID: "missing_second", Type: "missing_second", Version: "v2"},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "missing_first"),
			edge("missing_first", workflow.RouteSuccess, "probe"),
			edge("probe", workflow.RouteSuccess, "missing_second"),
			edge("missing_second", workflow.RouteSuccess, "end"),
		},
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	compileErr := requireStructuredCompileError(t, plan, err)
	issues := compileErr.Issues()

	if called.Load() != 0 {
		t.Fatalf("probe Compile() calls = %d, want 0", called.Load())
	}

	assertDiagnosticNodeIssues(
		t,
		issues,
		workflow.CompileIssueUnknownNodeType,
		[][]workflow.NodeID{{"missing_first"}, {"missing_second"}},
	)
}

func TestCompileDiagnosticsAggregateControlPathsAndGateGraph(t *testing.T) {
	t.Parallel()

	registry := diagnosticRegistry(t)
	definition := emptyDiagnosticDefinition(
		"bad_edges",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{
			edge("missing_source", workflow.RouteSuccess, "end"),
			edge("start", workflow.RouteSuccess, "missing_target"),
			edge("start", "unknown_route", "end"),
			edge("start", workflow.RouteSuccess, "start"),
			edge("start", workflow.RouteSuccess, "end"),
			edge("start", workflow.RouteSuccess, "end"),
		},
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	compileErr := requireStructuredCompileError(t, plan, err)
	issues := compileErr.Issues()

	if len(issues) != 5 {
		t.Fatalf("CompileError.Issues() len = %d, want 5: %v", len(issues), compileErr)
	}

	want := []workflow.CompileControlPath{
		{From: workflow.NewNodePath("missing_source"), To: workflow.NewNodePath("end"), Route: workflow.RouteSuccess},
		{From: workflow.NewNodePath("start"), To: workflow.NewNodePath("missing_target"), Route: workflow.RouteSuccess},
		{From: workflow.NewNodePath("start"), To: workflow.NewNodePath("end"), Route: "unknown_route"},
		{From: workflow.NewNodePath("start"), To: workflow.NewNodePath("start"), Route: workflow.RouteSuccess},
		{From: workflow.NewNodePath("start"), To: workflow.NewNodePath("end"), Route: workflow.RouteSuccess},
	}

	for index, issue := range issues {
		if issue.Code() != workflow.CompileIssueInvalidControlPath ||
			issue.Location() != workflow.CompileLocationPath {
			t.Fatalf("issue %d = (%q, %q), want invalid control path", index, issue.Code(), issue.Location())
		}

		got, ok := issue.ControlPath()
		if !ok || !sameControlPath(got, want[index]) {
			t.Fatalf("issue %d ControlPath() = %#v, %t, want %#v", index, got, ok, want[index])
		}
	}
}

func TestCompileDiagnosticsAggregateGraphProblems(t *testing.T) {
	t.Parallel()

	probe := diagnosticNodeType{
		key: "graph_probe",
		spec: workflow.NodeSpec{
			Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.PortSchema{},
			Routes: []string{workflow.RouteSuccess},
		},
	}
	registry := diagnosticRegistry(t, probe)
	definition := emptyDiagnosticDefinition(
		"bad_graph",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "first", Type: probe.key, Version: "v1"},
			{ID: "second", Type: probe.key, Version: "v1"},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "first"),
			edge("start", workflow.RouteSuccess, "second"),
		},
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	compileErr := requireStructuredCompileError(t, plan, err)
	assertDiagnosticNodeIssues(
		t,
		compileErr.Issues(),
		workflow.CompileIssueInvalidGraph,
		[][]workflow.NodeID{{"first"}, {"first"}, {"second"}, {"second"}, {"end"}},
	)
}

func TestCompileDiagnosticsBindingOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	probe := diagnosticNodeType{
		key: "binding_probe",
		spec: workflow.NodeSpec{
			Inputs:  map[string]workflow.PortSchema{"zeta": numberSchema, "alpha": numberSchema},
			Outputs: map[string]workflow.PortSchema{}, Routes: []string{workflow.RouteSuccess},
		},
	}
	registry := diagnosticRegistry(t, probe)

	compile := func(inputs map[string]workflow.WorkflowInput, bindings map[string]workflow.Binding) *workflow.CompileError {
		definition := workflow.Definition{
			Schema: workflow.SchemaV1Alpha1, ID: "binding_order", Revision: "v1", Name: "Binding Order",
			Inputs: inputs, Outputs: map[string]workflow.OutputBinding{},
			Nodes: []workflow.NodeDefinition{
				{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
				{ID: "probe", Type: probe.key, Version: "v1", Inputs: bindings},
				{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
			},
			Edges: []workflow.ControlEdge{
				edge("start", workflow.RouteSuccess, "probe"),
				edge("probe", workflow.RouteSuccess, "end"),
			},
			Limits: workflow.DefaultLimits(),
		}

		plan, err := workflow.Compile(t.Context(), definition, registry)

		return requireStructuredCompileError(t, plan, err)
	}

	first := compile(
		map[string]workflow.WorkflowInput{
			"zeta":  {Schema: stringSchema, Required: true},
			"alpha": {Schema: stringSchema, Required: true},
		},
		map[string]workflow.Binding{"zeta": workflowInput("zeta"), "alpha": workflowInput("alpha")},
	)
	second := compile(
		map[string]workflow.WorkflowInput{
			"alpha": {Schema: stringSchema, Required: true},
			"zeta":  {Schema: stringSchema, Required: true},
		},
		map[string]workflow.Binding{"alpha": workflowInput("alpha"), "zeta": workflowInput("zeta")},
	)

	if first.Error() != second.Error() {
		t.Fatalf("CompileError order changed:\nfirst:  %s\nsecond: %s", first, second)
	}

	issues := first.Issues()
	assertDiagnosticNodeIssues(
		t,
		issues,
		workflow.CompileIssueInvalidBinding,
		[][]workflow.NodeID{{"probe"}, {"probe"}},
	)

	if !strings.Contains(issues[0].Message(), `input "alpha"`) ||
		!strings.Contains(issues[1].Message(), `input "zeta"`) {
		t.Fatalf("binding issue order = %q, %q, want alpha then zeta", issues[0].Message(), issues[1].Message())
	}
}

func TestCompileDiagnosticsPreserveCausesAndConstructionBarrier(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("diagnostic sentinel")

	var laterCalled atomic.Int64

	failing := diagnosticNodeType{
		key: "failing_probe",
		compile: func(context.Context) (workflow.CompiledNode, error) {
			return nil, sentinel
		},
	}
	later := diagnosticNodeType{
		key:    "later_probe",
		called: &laterCalled,
		spec: workflow.NodeSpec{
			Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.PortSchema{},
			Routes: []string{workflow.RouteSuccess},
		},
	}
	registry := diagnosticRegistry(t, failing, later)
	definition := emptyDiagnosticDefinition(
		"construction_barrier",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "failing", Type: failing.key, Version: "v1"},
			{ID: "later", Type: later.key, Version: "v1"},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "failing"),
			edge("failing", workflow.RouteSuccess, "later"),
			edge("later", workflow.RouteSuccess, "end"),
		},
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	compileErr := requireStructuredCompileError(t, plan, err)

	if !errors.Is(compileErr, sentinel) {
		t.Fatalf("CompileError chain = %v, want sentinel", compileErr)
	}

	if laterCalled.Load() != 0 {
		t.Fatalf("later Compile() calls = %d, want 0", laterCalled.Load())
	}

	assertDiagnosticNodeIssues(
		t,
		compileErr.Issues(),
		workflow.CompileIssueInvalidNodeConfig,
		[][]workflow.NodeID{{"failing"}},
	)
}

func TestCompileDiagnosticsPrefixCompositeLocations(t *testing.T) {
	t.Parallel()

	registry := diagnosticRegistry(t)
	tests := []struct {
		name string
		kind workflow.NodeTypeKey
	}{
		{name: "batch", kind: workflow.NodeTypeBatch},
		{name: "loop", kind: workflow.NodeTypeLoop},
		{name: "sub-workflow", kind: workflow.NodeTypeSubWorkflow},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			t.Run("node", func(t *testing.T) {
				t.Parallel()

				child := emptyDiagnosticDefinition(
					workflow.DefinitionID("child_unknown_"+string(test.kind)),
					[]workflow.NodeDefinition{
						{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
						{ID: "missing", Type: "missing_child", Version: "v1"},
						{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
					},
					[]workflow.ControlEdge{
						edge("start", workflow.RouteSuccess, "missing"),
						edge("missing", workflow.RouteSuccess, "end"),
					},
				)
				parent, options := diagnosticCompositeParent(t, test.kind, child)
				plan, err := workflow.Compile(t.Context(), parent, registry, options...)
				compileErr := requireStructuredCompileError(t, plan, err)
				assertDiagnosticNodeIssues(
					t,
					compileErr.Issues(),
					workflow.CompileIssueUnknownNodeType,
					[][]workflow.NodeID{{"composite", "missing"}},
				)
			})

			t.Run("control-path", func(t *testing.T) {
				t.Parallel()

				child := emptyDiagnosticDefinition(
					workflow.DefinitionID("child_path_"+string(test.kind)),
					[]workflow.NodeDefinition{
						{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
						{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
					},
					[]workflow.ControlEdge{edge("start", "bad_route", "end")},
				)
				parent, options := diagnosticCompositeParent(t, test.kind, child)
				plan, err := workflow.Compile(t.Context(), parent, registry, options...)
				compileErr := requireStructuredCompileError(t, plan, err)
				issues := compileErr.Issues()

				if len(issues) != 1 || issues[0].Code() != workflow.CompileIssueInvalidControlPath {
					t.Fatalf("issues = %#v, want one invalid control path", issues)
				}

				path, ok := issues[0].ControlPath()

				want := workflow.CompileControlPath{
					From: workflow.NewNodePath("composite", "start"),
					To:   workflow.NewNodePath("composite", "end"), Route: "bad_route",
				}
				if !ok || !sameControlPath(path, want) {
					t.Fatalf("ControlPath() = %#v, %t, want %#v", path, ok, want)
				}
			})
		})
	}
}

func TestCompileDiagnosticsDistinguishDiagnosticsFromCallerFailures(t *testing.T) {
	t.Parallel()

	registry := diagnosticRegistry(t)
	definition := emptyDiagnosticDefinition(
		"caller_failures",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
	)
	_, err := workflow.Compile(t.Context(), definition, registry, nil)

	var compileErr *workflow.CompileError
	if err == nil || errors.As(err, &compileErr) {
		t.Fatalf("Compile() error = %v, CompileError = %#v, want caller error", err, compileErr)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = workflow.Compile(ctx, definition, registry)
	if !errors.Is(err, context.Canceled) || errors.As(err, &compileErr) {
		t.Fatalf("canceled Compile() error = %v, CompileError = %#v", err, compileErr)
	}
}

func TestCompileDiagnosticsPreserveInvalidDefinitionCause(t *testing.T) {
	t.Parallel()

	registry := diagnosticRegistry(t)
	definition := emptyDiagnosticDefinition(
		"invalid_definition",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
	)
	definition.Name = ""

	plan, err := workflow.Compile(t.Context(), definition, registry)

	compileErr := requireStructuredCompileError(t, plan, err)
	if !errors.Is(compileErr, workflow.ErrInvalidDefinition) {
		t.Fatalf("CompileError chain = %v, want ErrInvalidDefinition", compileErr)
	}

	issues := compileErr.Issues()
	if len(issues) != 1 || issues[0].Code() != workflow.CompileIssueInvalidDefinition ||
		issues[0].Location() != workflow.CompileLocationDefinition {
		t.Fatalf("issues = %#v, want one definition issue", issues)
	}
}

func TestCompileDiagnosticsLocateResolverFailuresAtOwningNode(t *testing.T) {
	t.Parallel()

	registry := diagnosticRegistry(t)
	child := emptyDiagnosticDefinition(
		"missing_child_definition",
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
	)
	parent, _ := diagnosticCompositeParent(t, workflow.NodeTypeSubWorkflow, child)
	sentinel := errors.New("resolver sentinel")
	resolver := workflow.DefinitionResolverFunc(func(
		context.Context,
		workflow.DefinitionID,
		workflow.Revision,
	) (workflow.Definition, error) {
		return workflow.Definition{}, sentinel
	})

	plan, err := workflow.Compile(
		t.Context(),
		parent,
		registry,
		workflow.WithDefinitionResolver(resolver),
	)

	compileErr := requireStructuredCompileError(t, plan, err)
	if !errors.Is(compileErr, sentinel) {
		t.Fatalf("CompileError chain = %v, want resolver sentinel", compileErr)
	}

	assertDiagnosticNodeIssues(
		t,
		compileErr.Issues(),
		workflow.CompileIssueInvalidReference,
		[][]workflow.NodeID{{"composite"}},
	)
}

func diagnosticRegistry(t *testing.T, nodeTypes ...workflow.NodeType) *workflow.Registry {
	t.Helper()

	all := append(workflow.BuiltinNodeTypes(), nodeTypes...)

	registry, err := workflow.NewRegistry(all, nil)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	return registry
}

func emptyDiagnosticDefinition(
	id workflow.DefinitionID,
	nodes []workflow.NodeDefinition,
	edges []workflow.ControlEdge,
) workflow.Definition {
	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: nodes, Edges: edges, Limits: workflow.DefaultLimits(),
	}
}

func diagnosticCompositeParent(
	t *testing.T,
	kind workflow.NodeTypeKey,
	child workflow.Definition,
) (workflow.Definition, []workflow.CompileOption) {
	t.Helper()

	definition := emptyDiagnosticDefinition(
		"parent_"+workflow.DefinitionID(kind),
		[]workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "composite", Type: kind, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		[]workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "composite"),
			edge("composite", workflow.RouteSuccess, "end"),
		},
	)

	switch kind {
	case workflow.NodeTypeBatch:
		itemsSchema := mustSchema(t, `{"type":"array","items":true}`)
		definition.Inputs["items"] = workflow.WorkflowInput{Schema: itemsSchema, Required: true}
		definition.Nodes[1].Inputs = map[string]workflow.Binding{"items": workflowInput("items")}
		definition.Nodes[1].Config = mustJSON(t, workflow.BatchConfig{
			Body: child, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 1,
		})
	case workflow.NodeTypeLoop:
		definition.Nodes[1].Config = mustJSON(t, workflow.LoopConfig{
			Body: child, Mode: workflow.LoopInfinite, MaxIterations: 1,
		})
	case workflow.NodeTypeSubWorkflow:
		fingerprint, err := child.Fingerprint()
		if err != nil {
			t.Fatalf("child Fingerprint() error = %v", err)
		}

		definition.Nodes[1].Config = mustJSON(t, workflow.SubWorkflowConfig{Workflow: workflow.DefinitionRef{
			ID: child.ID, Revision: child.Revision, Fingerprint: fingerprint,
		}})
		resolver := workflow.DefinitionResolverFunc(func(
			context.Context,
			workflow.DefinitionID,
			workflow.Revision,
		) (workflow.Definition, error) {
			return child, nil
		})

		return definition, []workflow.CompileOption{workflow.WithDefinitionResolver(resolver)}
	default:
		t.Fatalf("unsupported composite kind %q", kind)
	}

	return definition, nil
}

func requireStructuredCompileError(
	t *testing.T,
	plan *workflow.Plan,
	err error,
) *workflow.CompileError {
	t.Helper()

	if plan != nil {
		t.Fatalf("Compile() plan = %#v, want nil", plan)
	}

	if !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("Compile() error = %v, want ErrCompile", err)
	}

	var compileErr *workflow.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("Compile() error = %T %v, want *CompileError", err, err)
	}

	if len(compileErr.Issues()) == 0 {
		t.Fatal("CompileError.Issues() is empty")
	}

	return compileErr
}

func assertDiagnosticNodeIssues(
	t *testing.T,
	issues []workflow.CompileIssue,
	code workflow.CompileIssueCode,
	wantPaths [][]workflow.NodeID,
) {
	t.Helper()

	if len(issues) != len(wantPaths) {
		t.Fatalf("issues len = %d, want %d", len(issues), len(wantPaths))
	}

	for index, issue := range issues {
		if issue.Code() != code || issue.Location() != workflow.CompileLocationNode {
			t.Fatalf("issue %d = (%q, %q), want (%q, node)", index, issue.Code(), issue.Location(), code)
		}

		path, ok := issue.NodePath()

		wantPath := wantPaths[index] // #nosec G602 -- lengths are checked above.
		if !ok || !reflect.DeepEqual(path.Nodes(), wantPath) {
			t.Fatalf("issue %d NodePath() = %v, %t, want %v", index, path.Nodes(), ok, wantPath)
		}
	}
}

func sameControlPath(first, second workflow.CompileControlPath) bool {
	return reflect.DeepEqual(first.From.Nodes(), second.From.Nodes()) &&
		reflect.DeepEqual(first.To.Nodes(), second.To.Nodes()) &&
		first.Route == second.Route
}
