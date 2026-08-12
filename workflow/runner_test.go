package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestRunnerLinearWorkflow(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	normalize := &fakeAction{
		spec: actionSpec("normalize", map[string]workflow.PortSchema{"value": stringSchema}, map[string]workflow.PortSchema{"value": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			var value string

			value, err := workflow.DecodeValue[string](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{"value": workflow.MustValueOf("normalized:" + value)}}, nil
		},
	}
	save := &fakeAction{
		spec: actionSpec("save", map[string]workflow.PortSchema{"value": stringSchema}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": input.Values["value"]}}, nil
		},
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "linear", Revision: "v1", Name: "Linear",
		Inputs: map[string]workflow.WorkflowInput{"input": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "save", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "normalize", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "normalize"),
				Inputs: map[string]workflow.Binding{"value": workflowInput("input")},
			},
			{
				ID: "save", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "save"),
				Inputs: map[string]workflow.Binding{"value": nodeBinding("normalize", "value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "normalize"),
			edge("normalize", workflow.RouteSuccess, "save"),
			edge("save", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	plan := compileRoundTrip(t, definition, normalize, save)

	result := runWorkflow(t, plan, map[string]workflow.Value{"input": workflow.MustValueOf("value")})
	if got := result.Outputs["result"].String(); got != `"normalized:value"` {
		t.Fatalf("result = %s, want normalized:value", got)
	}
}

func TestRunnerConditionalMergeWorkflow(t *testing.T) {
	t.Parallel()

	boolSchema := mustSchema(t, `{"type":"boolean"}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	validate := &fakeAction{
		spec: actionSpec("validate", map[string]workflow.PortSchema{"value": boolSchema}, map[string]workflow.PortSchema{"valid": boolSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{"valid": input.Values["value"]}}, nil
		},
	}
	accept := constantAction("accept", "accepted", stringSchema)
	reject := constantAction("reject", "rejected", stringSchema)
	trueValue := workflow.MustValueOf(true)

	conditionConfig := workflow.ConditionConfig{
		Predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "valid", Value: &trueValue},
		TrueRoute: "yes", FalseRoute: "no",
	}
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"accepted", "rejected"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "condition", Revision: "v1", Name: "Condition",
		Inputs: map[string]workflow.WorkflowInput{"approved": {Schema: boolSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "validate", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "validate"), Inputs: map[string]workflow.Binding{"value": workflowInput("approved")},
			},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, conditionConfig), Inputs: map[string]workflow.Binding{"valid": nodeBinding("validate", "valid")},
			},
			{ID: "accept", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "accept")},
			{ID: "reject", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "reject")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"accepted": nodeBinding("accept", "result"),
					"rejected": nodeBinding("reject", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "validate"),
			edge("validate", workflow.RouteSuccess, "condition"),
			edge("condition", "yes", "accept"),
			edge("condition", "no", "reject"),
			edge("accept", workflow.RouteSuccess, "merge"),
			edge("reject", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	plan := compileRoundTrip(t, definition, validate, accept, reject)

	result := runWorkflow(t, plan, map[string]workflow.Value{"approved": workflow.MustValueOf(true)})
	if got := result.Outputs["result"].String(); got != `"accepted"` {
		t.Fatalf("result = %s, want accepted", got)
	}

	if result.Nodes["reject"].Status != workflow.NodeStatusSkipped {
		t.Fatalf("reject status = %s, want skipped", result.Nodes["reject"].Status)
	}
}

func TestRunnerParallelMergeWaitsForAll(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan string, 2)
	release := make(chan struct{})
	parallelAction := func(key string) *fakeAction {
		return &fakeAction{
			spec: actionSpec(workflow.ActionKey(key), map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{"result": stringSchema}),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				started <- key

				select {
				case <-ctx.Done():
					return workflow.ActionOutput{}, ctx.Err()
				case <-release:
					return workflow.ActionOutput{Values: map[string]workflow.Value{"result": workflow.MustValueOf(key)}}, nil
				}
			},
		}
	}
	profile := parallelAction("profile")
	policy := parallelAction("policy")
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeParallel,
		Outputs: map[string]workflow.MergeOutputConfig{
			"profile": {Schema: stringSchema, Sources: []string{"profile"}},
			"policy":  {Schema: stringSchema, Sources: []string{"policy"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "parallel", Revision: "v1", Name: "Parallel",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"profile": nodeOutput(stringSchema, "merge", "profile"),
			"policy":  nodeOutput(stringSchema, "merge", "policy"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "profile", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "profile")},
			{ID: "policy", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "policy")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"profile": nodeBinding("profile", "result"),
					"policy":  nodeBinding("policy", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "profile"),
			edge("start", workflow.RouteSuccess, "policy"),
			edge("profile", workflow.RouteSuccess, "merge"),
			edge("policy", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
	plan := compileRoundTrip(t, definition, profile, policy)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, runErr := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		resultChannel <- result

		errorChannel <- runErr
	}()

	first := <-started

	second := <-started
	if first == second {
		t.Fatalf("only one parallel Action started: %q", first)
	}

	close(release)

	if runErr := <-errorChannel; runErr != nil {
		t.Fatalf("Runner.Run() error = %v", runErr)
	}

	result := <-resultChannel
	if result.Outputs["profile"].String() != `"profile"` || result.Outputs["policy"].String() != `"policy"` {
		t.Fatalf("outputs = %#v", result.Outputs)
	}
}

func TestRunnerRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var attempts atomic.Int32

	unreliable := &fakeAction{
		spec: actionSpec("unreliable", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			if attempts.Add(1) == 1 {
				return workflow.ActionOutput{}, errors.New("temporary failure")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": workflow.MustValueOf("ok")}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "retry", Revision: "v1", Name: "Retry",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "unreliable", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "unreliable", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "unreliable"), Policy: workflow.NodePolicy{Retry: workflow.RetryPolicy{MaxAttempts: 2}},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "unreliable"),
			edge("unreliable", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	plan := compileRoundTrip(t, definition, unreliable)

	result := runWorkflow(t, plan, map[string]workflow.Value{})
	if attempts.Load() != 2 || result.Nodes["unreliable"].Attempts != 2 {
		t.Fatalf("attempts = %d/%d, want 2/2", attempts.Load(), result.Nodes["unreliable"].Attempts)
	}
}

func TestPlanInputAndResultMutationIsolation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	definition := workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "isolation",
		Revision: "v1",
		Name:     "Isolation",
		Inputs:   map[string]workflow.WorkflowInput{"input": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": {
				Schema:  stringSchema,
				Binding: workflowInput("input"),
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	planFingerprint := plan.Fingerprint()
	definition.Inputs["input"] = workflow.WorkflowInput{Schema: numberSchema, Required: true}
	definition.Nodes[0].ID = "mutated"

	runner := mustRunner(t)
	inputs := map[string]workflow.Value{"input": workflow.MustValueOf("first")}

	first, err := runner.Run(t.Context(), plan, inputs)
	if err != nil {
		t.Fatalf("first Runner.Run() error = %v", err)
	}

	inputs["input"] = workflow.MustValueOf("mutated")
	first.Outputs["result"] = workflow.MustValueOf("mutated")
	delete(first.Nodes, "start")

	second, err := runner.Run(
		t.Context(),
		plan,
		map[string]workflow.Value{"input": workflow.MustValueOf("second")},
	)
	if err != nil {
		t.Fatalf("second Runner.Run() error = %v", err)
	}

	if plan.Fingerprint() != planFingerprint || second.Outputs["result"].String() != `"second"` {
		t.Fatalf("plan or result changed: fingerprint=%s result=%s", plan.Fingerprint(), second.Outputs["result"].String())
	}

	if _, ok := second.Nodes["start"]; !ok {
		t.Fatal("mutating first RunResult changed the second RunResult")
	}
}

func compileRoundTrip(t *testing.T, definition workflow.Definition, actions ...workflow.Action) *workflow.Plan {
	t.Helper()

	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	decoded, err := workflow.DecodeDefinition(data)
	if err != nil {
		t.Fatalf("DecodeDefinition() error = %v", err)
	}

	registry, err := workflow.NewDefaultRegistry(actions...)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), decoded, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v\ndefinition: %s", err, data)
	}

	return plan
}

func runWorkflow(t *testing.T, plan *workflow.Plan, inputs map[string]workflow.Value) workflow.RunResult {
	t.Helper()

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "run-1", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, inputs)
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusSucceeded {
		t.Fatalf("RunResult.Status = %s, want succeeded", result.Status)
	}

	return result
}

func actionSpec(
	key workflow.ActionKey,
	inputs map[string]workflow.PortSchema,
	outputs map[string]workflow.PortSchema,
) workflow.ActionSpec {
	return workflow.ActionSpec{Key: key, Version: "v1", Inputs: inputs, Outputs: outputs}
}

func constantAction(key, value string, schema workflow.PortSchema) *fakeAction {
	return &fakeAction{
		spec: actionSpec(workflow.ActionKey(key), map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{"result": schema}),
		run: func(_ context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": workflow.MustValueOf(value)}}, nil
		},
	}
}

func mustSchema(t *testing.T, schema string) workflow.PortSchema {
	t.Helper()

	parsed, err := workflow.ParsePortSchema([]byte(schema))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	return parsed
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T) error = %v", value, err)
	}

	return data
}

func actionConfig(t *testing.T, key workflow.ActionKey) json.RawMessage {
	t.Helper()

	return mustJSON(t, workflow.ActionConfig{Action: key, Version: "v1"})
}

func workflowInput(port string) workflow.Binding {
	return workflow.Binding{Source: workflow.BindingWorkflowInput, Port: port}
}

func nodeBinding(node workflow.NodeID, port string) workflow.Binding {
	return workflow.Binding{Source: workflow.BindingNodeOutput, Node: node, Port: port}
}

func nodeOutput(schema workflow.PortSchema, node workflow.NodeID, port string) workflow.OutputBinding {
	return workflow.OutputBinding{Schema: schema, Binding: nodeBinding(node, port)}
}

func edge(from workflow.NodeID, route string, to workflow.NodeID) workflow.ControlEdge {
	return workflow.ControlEdge{From: workflow.NodeRoute{Node: from, Route: route}, To: to}
}

func ExampleRunner_Run() {
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	definition := workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "example",
		Revision: "v1",
		Name:     "Example",
		Inputs:   map[string]workflow.WorkflowInput{"input": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": {
				Schema: stringSchema,
				Binding: workflow.Binding{
					Source: workflow.BindingWorkflowInput,
					Port:   "input",
				},
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	registry, _ := workflow.NewDefaultRegistry()
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	runner, _ := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "example-run", nil }),
	)
	result, _ := runner.Run(
		context.Background(),
		plan,
		map[string]workflow.Value{"input": workflow.MustValueOf("hello")},
	)

	fmt.Println(result.Status, result.Outputs["result"].String())
	// Output: succeeded "hello"
}
