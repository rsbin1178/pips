package workflow_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestMergeExactVersionsKeepDistinctExclusiveSemantics(t *testing.T) {
	t.Parallel()

	anySchema := mustSchema(t, `true`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	null := workflow.MustValueOf(nil)
	first := workflow.MustValueOf("first")
	second := workflow.MustValueOf("second")

	tests := []struct {
		name      string
		version   string
		schema    workflow.PortSchema
		inputs    map[string]workflow.Value
		want      workflow.Value
		wantError bool
	}{
		{
			name: "v2 skips missing", version: workflow.MergeNodeVersionV2,
			schema: anySchema, inputs: map[string]workflow.Value{"second": second}, want: second,
		},
		{
			name: "v2 skips explicit null", version: workflow.MergeNodeVersionV2,
			schema: anySchema,
			inputs: map[string]workflow.Value{"first": null, "second": second}, want: second,
		},
		{
			name: "v2 selects first of multiple non-null values", version: workflow.MergeNodeVersionV2,
			schema: anySchema,
			inputs: map[string]workflow.Value{"first": first, "second": second}, want: first,
		},
		{
			name: "v2 all null returns null", version: workflow.MergeNodeVersionV2,
			schema: anySchema,
			inputs: map[string]workflow.Value{"first": null, "second": null}, want: null,
		},
		{
			name: "v2 all missing returns null", version: workflow.MergeNodeVersionV2,
			schema: anySchema, inputs: map[string]workflow.Value{}, want: null,
		},
		{
			name: "v2 all null preserves schema validation", version: workflow.MergeNodeVersionV2,
			schema: stringSchema,
			inputs: map[string]workflow.Value{"first": null, "second": null}, wantError: true,
		},
		{
			name: "v1 treats one explicit null as present", version: workflow.BuiltinNodeVersion,
			schema: anySchema, inputs: map[string]workflow.Value{"first": null}, want: null,
		},
		{
			name: "v1 rejects all missing", version: workflow.BuiltinNodeVersion,
			schema: anySchema, inputs: map[string]workflow.Value{}, wantError: true,
		},
		{
			name: "v1 rejects two present", version: workflow.BuiltinNodeVersion,
			schema: anySchema,
			inputs: map[string]workflow.Value{"first": first, "second": second}, wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			executor := compileExactMerge(t, test.version, workflow.MergeExclusive, test.schema)

			output, err := executor.Invoke(t.Context(), workflow.NodeInput{Values: test.inputs})
			if test.wantError {
				if err == nil {
					t.Fatal("Invoke() error = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("Invoke() error = %v", err)
			}

			if !output.Values["result"].Equal(test.want) {
				t.Fatalf("result = %s, want %s", output.Values["result"].String(), test.want.String())
			}
		})
	}
}

func TestMergeParallelSemanticsAreIdenticalAcrossVersions(t *testing.T) {
	t.Parallel()

	anySchema := mustSchema(t, `true`)
	null := workflow.MustValueOf(nil)

	for _, version := range []string{workflow.BuiltinNodeVersion, workflow.MergeNodeVersionV2} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			executor := compileExactMerge(t, version, workflow.MergeParallel, anySchema)

			output, err := executor.Invoke(t.Context(), workflow.NodeInput{
				Values: map[string]workflow.Value{"first": null},
			})
			if err != nil || !output.Values["result"].Equal(null) {
				t.Fatalf("Invoke(present null) = %#v, %v", output, err)
			}

			_, err = executor.Invoke(t.Context(), workflow.NodeInput{Values: map[string]workflow.Value{}})
			if err == nil {
				t.Fatal("Invoke(missing) error = nil, want error")
			}
		})
	}
}

func TestMergeV2ExclusiveAcceptsRouteRelevantIndirectOutputs(t *testing.T) {
	t.Parallel()

	definition, actions := indirectOutputMergeDefinition(t, workflow.MergeNodeVersionV2)
	plan := compileRoundTrip(t, definition, actions...)
	result := runWorkflow(t, plan, map[string]workflow.Value{})

	if got := result.Outputs["result"].String(); got != `"first"` {
		t.Fatalf("result = %s, want first", got)
	}
}

func TestMergeV2ExactIdentityWorksWithDebugAndPartialRun(t *testing.T) {
	t.Parallel()

	definition, actions := indirectOutputMergeDefinition(t, workflow.MergeNodeVersionV2)
	plan := compileRoundTrip(t, definition, actions...)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("merge"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	if !slices.Equal(debugPlan.Spec().OptionalInputs, []string{"first", "second"}) {
		t.Fatalf("OptionalInputs = %#v", debugPlan.Spec().OptionalInputs)
	}

	runner := mustRunner(t)

	debugResult, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"first":  workflow.MustValueOf(nil),
		"second": workflow.MustValueOf("debug-second"),
	})
	if err != nil {
		t.Fatalf("Runner.DebugNode() error = %v", err)
	}

	if got := debugResult.Execution.Outputs["result"].String(); got != `"debug-second"` {
		t.Fatalf("debug result = %s, want debug-second", got)
	}

	partialResult, err := runner.RunPartial(
		t.Context(),
		plan,
		"merge",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := partialResult.Outputs["result"].String(); got != `"first"` {
		t.Fatalf("partial result = %s, want first", got)
	}
}

func TestMergeV2CheckpointRejectsV1PlanBeforeInvocation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition, actions := directMergeDefinition(t, workflow.MergeNodeVersionV2, stringSchema)

	registry, err := workflow.NewDefaultRegistry(actions...)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	v2Plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("merge")),
	)
	if err != nil {
		t.Fatalf("Compile(v2) error = %v", err)
	}

	definition.Nodes[3].Version = workflow.BuiltinNodeVersion

	v1Plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("merge")),
	)
	if err != nil {
		t.Fatalf("Compile(v1) error = %v", err)
	}

	if v1Plan.Fingerprint() == v2Plan.Fingerprint() {
		t.Fatal("merge exact version did not change Plan fingerprint")
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "merge-v2", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), v2Plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "merge-v2")

	_, err = runner.Resume(t.Context(), v1Plan, "merge-v2", nil)
	if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Resume(v1) error = %v, want ErrRun only", err)
	}
}

func TestMergeV2ExclusiveAcceptsRouteRelevantIndirectErrors(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	failing := resultAction(
		"merge_v2_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("indirect failure")
		},
	)
	definition := indirectErrorMergeDefinition(t, stringSchema)
	plan := compileRoundTrip(
		t,
		definition,
		failing,
		constantAction("merge_v2_success", "success", stringSchema),
		constantAction("merge_v2_handler", "handler", stringSchema),
		constantAction("merge_v2_other", "other", stringSchema),
	)

	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Outputs["result"].String() != `"indirect failure"` {
		t.Fatalf("result = %#v", result)
	}
}

func TestMergeCompilerKeepsExactVersionAvailabilityMatrix(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	tests := []struct {
		name       string
		definition func(*testing.T) (workflow.Definition, []workflow.Action)
		wantDetail string
	}{
		{
			name: "v1 indirect output remains rejected",
			definition: func(t *testing.T) (workflow.Definition, []workflow.Action) {
				t.Helper()

				return indirectOutputMergeDefinition(t, workflow.BuiltinNodeVersion)
			},
			wantDetail: "not from an incoming node",
		},
		{
			name: "v2 parallel indirect output remains rejected",
			definition: func(t *testing.T) (workflow.Definition, []workflow.Action) {
				t.Helper()

				definition, actions := indirectOutputMergeDefinition(t, workflow.MergeNodeVersionV2)
				config := workflow.MergeConfig{
					Mode: workflow.MergeParallel,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"first"}},
					},
				}
				definition.Nodes[4].Config = mustJSON(t, config)
				definition.Nodes[4].Inputs = map[string]workflow.Binding{
					"first": nodeBinding("first", "result"),
				}

				return definition, actions
			},
			wantDetail: "not from an incoming node",
		},
		{
			name:       "v2 future output is rejected",
			definition: futureMergeCandidateDefinition,
			wantDetail: "not from a route-relevant upstream node",
		},
		{
			name:       "v2 unrelated output is rejected",
			definition: unrelatedMergeCandidateDefinition,
			wantDetail: "not from a route-relevant upstream node",
		},
		{
			name: "v2 normal output reachable only through error route is rejected",
			definition: func(t *testing.T) (workflow.Definition, []workflow.Action) {
				t.Helper()

				definition, actions := wrongErrorRouteMergeDefinition(t, stringSchema)
				definition.Edges[4].To = "final"
				definition.Edges[7].To = "merge"
				definition.Nodes[5].Inputs = map[string]workflow.Binding{
					"failure": nodeBinding("source", "result"),
				}
				definition.Nodes[6].Inputs["handled"] = nodeBinding("normal", "result")

				return definition, actions
			},
			wantDetail: "not from a route-relevant upstream node",
		},
		{
			name: "v2 wrong error route is rejected",
			definition: func(t *testing.T) (workflow.Definition, []workflow.Action) {
				t.Helper()

				return wrongErrorRouteMergeDefinition(t, stringSchema)
			},
			wantDetail: "not from a route-relevant upstream error route",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition, actions := test.definition(t)

			registry, err := workflow.NewDefaultRegistry(actions...)
			if err != nil {
				t.Fatalf("NewDefaultRegistry() error = %v", err)
			}

			_, err = workflow.Compile(t.Context(), definition, registry)
			if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("Compile() error = %v, want ErrCompile containing %q", err, test.wantDetail)
			}
		})
	}
}

func compileExactMerge(
	t *testing.T,
	version string,
	mode workflow.MergeMode,
	schema workflow.PortSchema,
) workflow.CompiledNode {
	t.Helper()

	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	nodeType, ok := registry.NodeType(workflow.NodeTypeMerge, version)
	if !ok {
		t.Fatalf("Registry.NodeType(merge, %q) not found", version)
	}

	value := workflow.MustValueOf("unused")

	inputs := map[string]workflow.Binding{
		"first": {Source: workflow.BindingLiteral, Value: &value},
	}
	if mode == workflow.MergeExclusive {
		inputs["second"] = workflow.Binding{Source: workflow.BindingLiteral, Value: &value}
	}

	executor, err := nodeType.Compile(t.Context(), nil, workflow.NodeDefinition{
		ID: "merge", Type: workflow.NodeTypeMerge, Version: version,
		Config: mustJSON(t, workflow.MergeConfig{
			Mode: mode,
			Outputs: map[string]workflow.MergeOutputConfig{
				"result": {Schema: schema, Sources: mergeSources(mode)},
			},
		}),
		Inputs: inputs,
	})
	if err != nil {
		t.Fatalf("NodeType.Compile() error = %v", err)
	}

	return executor
}

func mergeSources(mode workflow.MergeMode) []string {
	if mode == workflow.MergeParallel {
		return []string{"first"}
	}

	return []string{"first", "second"}
}

func indirectOutputMergeDefinition(
	t *testing.T,
	version string,
) (workflow.Definition, []workflow.Action) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"first", "second"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-v2-indirect", Revision: "v1", Name: "Merge V2 Indirect",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "first", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_first")},
			{ID: "side", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_side")},
			{ID: "second", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_second")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: version,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"first":  nodeBinding("first", "result"),
					"second": nodeBinding("second", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "first"),
			edge("start", workflow.RouteSuccess, "second"),
			edge("first", workflow.RouteSuccess, "side"),
			edge("side", workflow.RouteSuccess, "merge"),
			edge("second", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, []workflow.Action{
		constantAction("merge_v2_first", "first", stringSchema),
		constantAction("merge_v2_side", "side", stringSchema),
		constantAction("merge_v2_second", "second", stringSchema),
	}
}

func directMergeDefinition(
	t *testing.T,
	version string,
	stringSchema workflow.PortSchema,
) (workflow.Definition, []workflow.Action) {
	t.Helper()

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-exact-identity", Revision: "v1", Name: "Merge Exact Identity",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "merge", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "first", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_exact_first")},
			{ID: "second", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_exact_second")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: version,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"first", "second"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"first":  nodeBinding("first", "result"),
					"second": nodeBinding("second", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "first"),
			edge("start", workflow.RouteSuccess, "second"),
			edge("first", workflow.RouteSuccess, "merge"),
			edge("second", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, []workflow.Action{
		constantAction("merge_exact_first", "first", stringSchema),
		constantAction("merge_exact_second", "second", stringSchema),
	}
}

func indirectErrorMergeDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-v2-error", Revision: "v1", Name: "Merge V2 Error",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "source", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_v2_failure"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{ID: "success", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_success")},
			{ID: "handler", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_handler")},
			{ID: "other", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_other")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.MergeNodeVersionV2,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"failure"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"failure": nodeErrorBinding("source", workflow.NodeErrorMessagePort),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "source"),
			edge("start", workflow.RouteSuccess, "other"),
			edge("source", workflow.RouteSuccess, "success"),
			edge("source", workflow.RouteError, "handler"),
			edge("success", workflow.RouteSuccess, "merge"),
			edge("handler", workflow.RouteSuccess, "merge"),
			edge("other", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func futureMergeCandidateDefinition(t *testing.T) (workflow.Definition, []workflow.Action) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-v2-future", Revision: "v1", Name: "Merge V2 Future",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "future", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "left", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_left")},
			{ID: "right", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_right")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.MergeNodeVersionV2,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"candidate"}},
					},
				}),
				Inputs: map[string]workflow.Binding{"candidate": nodeBinding("future", "result")},
			},
			{ID: "future", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_future")},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "left"),
			edge("start", workflow.RouteSuccess, "right"),
			edge("left", workflow.RouteSuccess, "merge"),
			edge("right", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "future"),
			edge("future", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, []workflow.Action{
		constantAction("merge_v2_left", "left", stringSchema),
		constantAction("merge_v2_right", "right", stringSchema),
		constantAction("merge_v2_future", "future", stringSchema),
	}
}

func unrelatedMergeCandidateDefinition(t *testing.T) (workflow.Definition, []workflow.Action) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-v2-unrelated", Revision: "v1", Name: "Merge V2 Unrelated",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "final", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "candidate", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_candidate")},
			{ID: "left", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_left")},
			{ID: "right", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_right")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.MergeNodeVersionV2,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"candidate"}},
					},
				}),
				Inputs: map[string]workflow.Binding{"candidate": nodeBinding("candidate", "result")},
			},
			{
				ID: "final", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"merged", "candidate"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"merged":    nodeBinding("merge", "result"),
					"candidate": nodeBinding("candidate", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "candidate"),
			edge("start", workflow.RouteSuccess, "left"),
			edge("start", workflow.RouteSuccess, "right"),
			edge("left", workflow.RouteSuccess, "merge"),
			edge("right", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "final"),
			edge("candidate", workflow.RouteSuccess, "final"),
			edge("final", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, []workflow.Action{
		constantAction("merge_v2_candidate", "candidate", stringSchema),
		constantAction("merge_v2_left", "left", stringSchema),
		constantAction("merge_v2_right", "right", stringSchema),
	}
}

func wrongErrorRouteMergeDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
) (workflow.Definition, []workflow.Action) {
	t.Helper()

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "merge-v2-wrong-error", Revision: "v1", Name: "Merge V2 Wrong Error",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "final", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "source", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "merge_v2_source"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{ID: "normal", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_normal")},
			{ID: "handler", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_handler")},
			{ID: "other", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "merge_v2_other")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.MergeNodeVersionV2,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"failure"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"failure": nodeErrorBinding("source", workflow.NodeErrorMessagePort),
				},
			},
			{
				ID: "final", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.MergeConfig{
					Mode: workflow.MergeExclusive,
					Outputs: map[string]workflow.MergeOutputConfig{
						"result": {Schema: stringSchema, Sources: []string{"merged", "handled"}},
					},
				}),
				Inputs: map[string]workflow.Binding{
					"merged":  nodeBinding("merge", "result"),
					"handled": nodeBinding("handler", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "source"),
			edge("start", workflow.RouteSuccess, "other"),
			edge("source", workflow.RouteSuccess, "normal"),
			edge("source", workflow.RouteError, "handler"),
			edge("normal", workflow.RouteSuccess, "merge"),
			edge("other", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "final"),
			edge("handler", workflow.RouteSuccess, "final"),
			edge("final", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, []workflow.Action{
		constantAction("merge_v2_source", "source", stringSchema),
		constantAction("merge_v2_normal", "normal", stringSchema),
		constantAction("merge_v2_handler", "handler", stringSchema),
		constantAction("merge_v2_other", "other", stringSchema),
	}
}
