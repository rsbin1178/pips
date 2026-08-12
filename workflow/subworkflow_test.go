package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestSubWorkflowNodeExecutesExactReferencedDefinition(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	childAction := &fakeAction{
		spec: actionSpec(
			"child_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			value, err := workflow.DecodeValue[string](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("child:" + value),
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "child_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	plan := compileWithResolver(t, parent, staticResolver(child), childAction)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	runner := mustRunner(t)

	result, err := runner.Run(ctx, plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if got := result.Outputs["result"].String(); got != `"child:input"` {
		t.Fatalf("result = %s, want child:input", got)
	}
}

func TestSubWorkflowPropagatesHandledFailureState(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)
	child := failureBranchDefinition(t, stringSchema, 1)
	failing := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("child handled failure")
		},
	)
	handler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["message"],
			}}, nil
		},
	}
	parent := emptySubWorkflowParent(t, stringSchema, child)
	plan := compileWithResolver(
		t,
		parent,
		staticResolver(child),
		failing,
		constantAction("typed_success", "success", stringSchema),
		handler,
	)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Nodes["sub"].Status != workflow.NodeStatusSucceeded ||
		result.Outputs["result"].String() != `"child handled failure"` {
		t.Fatalf("result = %#v", result)
	}
}

func TestSubWorkflowFailureUsesOuterPolicyAndTerminalFailureWins(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	child := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "failing-child", Revision: "v1", Name: "Failing Child",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "fail", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "fail", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "child_fail")},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "fail"),
			edge("fail", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	childFailure := resultAction(
		"child_fail",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("child stopped")
		},
	)
	parent := emptySubWorkflowParent(t, stringSchema, child)
	parent.Nodes[1].Policy = workflow.NodePolicy{
		Error: workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{
			"result": workflow.MustValueOf("outer default"),
		},
	}
	plan := compileWithResolver(t, parent, staticResolver(child), childFailure)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Nodes["sub"].Status != workflow.NodeStatusException ||
		result.Outputs["result"].String() != `"outer default"` {
		t.Fatalf("outer policy result = %#v", result)
	}

	handledChild := failureBranchDefinition(t, stringSchema, 1)
	handledParent := emptySubWorkflowParent(t, stringSchema, handledChild)
	handledParent.Nodes = append(
		handledParent.Nodes[:2],
		workflow.NodeDefinition{
			ID: "terminal", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
			Config: actionConfig(t, "terminal_fail"),
		},
		handledParent.Nodes[2],
	)
	handledParent.Edges = []workflow.ControlEdge{
		edge("start", workflow.RouteSuccess, "sub"),
		edge("sub", workflow.RouteSuccess, "terminal"),
		edge("terminal", workflow.RouteSuccess, "end"),
	}
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)
	handledSource := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("handled first")
		},
	)
	handledHandler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["message"],
			}}, nil
		},
	}
	terminal := resultAction(
		"terminal_fail",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("terminal failure")
		},
	)
	handledPlan := compileWithResolver(
		t,
		handledParent,
		staticResolver(handledChild),
		handledSource,
		constantAction("typed_success", "success", stringSchema),
		handledHandler,
		terminal,
	)

	failed, err := runner.Run(t.Context(), handledPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) || failed.Status != workflow.RunStatusFailed ||
		failed.Nodes["terminal"].Status != workflow.NodeStatusFailed {
		t.Fatalf("terminal result = %#v, error = %v", failed, err)
	}
}

func TestSubWorkflowNodeRejectsInvalidResolution(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	child := subWorkflowChildDefinition(stringSchema, "child_action")
	validRef := workflowRef(t, child)
	parent := subWorkflowParentDefinition(t, stringSchema, validRef)

	registry, err := workflow.NewDefaultRegistry(&fakeAction{spec: actionSpec(
		"child_action",
		map[string]workflow.PortSchema{"value": stringSchema},
		map[string]workflow.PortSchema{"result": stringSchema},
	)})
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	wrongIdentity := child
	wrongIdentity.ID = "different"
	resolverFailure := errors.New("resolver unavailable")

	tests := []struct {
		name       string
		definition workflow.Definition
		options    []workflow.CompileOption
		wantCause  error
	}{
		{
			name:       "missing resolver",
			definition: parent,
		},
		{
			name:       "identity mismatch",
			definition: parent,
			options: []workflow.CompileOption{workflow.WithDefinitionResolver(
				workflow.DefinitionResolverFunc(func(
					context.Context,
					workflow.DefinitionID,
					workflow.Revision,
				) (workflow.Definition, error) {
					return wrongIdentity, nil
				}),
			)},
		},
		{
			name: "fingerprint mismatch",
			definition: subWorkflowParentDefinition(t, stringSchema, workflow.DefinitionRef{
				ID: child.ID, Revision: child.Revision,
				Fingerprint: strings.Repeat("0", 64),
			}),
			options: []workflow.CompileOption{
				workflow.WithDefinitionResolver(staticResolver(child)),
			},
		},
		{
			name:       "resolver error",
			definition: parent,
			options: []workflow.CompileOption{workflow.WithDefinitionResolver(
				workflow.DefinitionResolverFunc(func(
					context.Context,
					workflow.DefinitionID,
					workflow.Revision,
				) (workflow.Definition, error) {
					return workflow.Definition{}, resolverFailure
				}),
			)},
			wantCause: resolverFailure,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, compileErr := workflow.Compile(
				t.Context(),
				test.definition,
				registry,
				test.options...,
			)
			if !errors.Is(compileErr, workflow.ErrCompile) {
				t.Fatalf("Compile() error = %v, want ErrCompile", compileErr)
			}

			if test.wantCause != nil && !errors.Is(compileErr, test.wantCause) {
				t.Fatalf("Compile() error = %v, want cause %v", compileErr, test.wantCause)
			}
		})
	}
}

func TestSubWorkflowNodeRejectsSchemaMismatch(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	numberSchema := mustSchema(t, `{"type":"number"}`)
	child := subWorkflowChildDefinition(stringSchema, "child_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	parent.Inputs["value"] = workflow.WorkflowInput{Schema: numberSchema, Required: true}

	registry, err := workflow.NewDefaultRegistry(&fakeAction{spec: actionSpec(
		"child_action",
		map[string]workflow.PortSchema{"value": stringSchema},
		map[string]workflow.PortSchema{"result": stringSchema},
	)})
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(
		t.Context(),
		parent,
		registry,
		workflow.WithDefinitionResolver(staticResolver(child)),
	)
	if !errors.Is(err, workflow.ErrCompile) {
		t.Fatalf("Compile() error = %v, want ErrCompile", err)
	}
}

func TestSubWorkflowNodeRejectsRecursionAndExcessiveDepth(t *testing.T) {
	t.Parallel()

	emptyRegistry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	self := emptySubWorkflowDefinition(
		t,
		"self",
		workflow.DefinitionRef{
			ID: "self", Revision: "v1", Fingerprint: strings.Repeat("0", 64),
		},
	)

	_, err = workflow.Compile(
		t.Context(),
		self,
		emptyRegistry,
		workflow.WithDefinitionResolver(staticResolver(self)),
	)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("recursive Compile() error = %v, want recursive ErrCompile", err)
	}

	definitions := make(map[string]workflow.Definition)
	current := emptyLeafDefinition("depth-9")
	definitions[definitionMapKey(current.ID, current.Revision)] = current

	for index := 8; index >= 0; index-- {
		id := workflow.DefinitionID(fmt.Sprintf("depth-%d", index))
		current = emptySubWorkflowDefinition(t, id, workflowRef(t, current))
		definitions[definitionMapKey(current.ID, current.Revision)] = current
	}

	resolver := workflow.DefinitionResolverFunc(func(
		_ context.Context,
		id workflow.DefinitionID,
		revision workflow.Revision,
	) (workflow.Definition, error) {
		definition, ok := definitions[definitionMapKey(id, revision)]
		if !ok {
			return workflow.Definition{}, errors.New("definition not found")
		}

		return definition, nil
	})

	_, err = workflow.Compile(
		t.Context(),
		current,
		emptyRegistry,
		workflow.WithDefinitionResolver(resolver),
	)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("deep Compile() error = %v, want nesting ErrCompile", err)
	}
}

func TestSubWorkflowPropagatesCancellation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{}, 1)
	childAction := &fakeAction{
		spec: actionSpec(
			"child_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			started <- struct{}{}

			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "child_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	plan := compileWithResolver(t, parent, staticResolver(child), childAction)
	runner := mustRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(ctx, plan, map[string]workflow.Value{
			"value": workflow.MustValueOf("input"),
		})
		resultChannel <- result

		errorChannel <- err
	}()

	<-started
	cancel()

	if err := <-errorChannel; !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	if result := <-resultChannel; result.Status != workflow.RunStatusCanceled {
		t.Fatalf("RunResult.Status = %q, want canceled", result.Status)
	}
}

func TestSubWorkflowSharesStepLimitAndScopedEvents(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	childAction := &fakeAction{
		spec: actionSpec(
			"child_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "child_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	parent.Limits = workflow.Limits{MaxConcurrency: 1, MaxSteps: 4}
	plan := compileWithResolver(t, parent, staticResolver(child), childAction)

	events := make([]workflow.Event, 0)

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "nested-run", nil }),
		workflow.WithEventSink(func(event workflow.Event) {
			events = append(events, event)
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	result, err := runner.Run(ctx, plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if result.Nodes["sub"].Failure != workflow.FailureLimit {
		t.Fatalf("sub failure = %q, want limit", result.Nodes["sub"].Failure)
	}

	var childEvents int

	for _, event := range events {
		if event.RunID != "nested-run" {
			t.Fatalf("event RunID = %q, want nested-run", event.RunID)
		}

		scope := event.Scope()
		if event.DefinitionID == child.ID {
			childEvents++

			if len(scope) != 1 || scope[0].Kind != workflow.ScopeSubWorkflow ||
				scope[0].NodeID != "sub" || scope[0].Index != -1 {
				t.Fatalf("child event scope = %#v", scope)
			}

			scope[0].NodeID = "mutated"
			if event.Scope()[0].NodeID != "sub" {
				t.Fatal("mutating Event.Scope result changed Event")
			}
		} else if len(scope) != 0 {
			t.Fatalf("root event scope = %#v, want empty", scope)
		}
	}

	if childEvents == 0 {
		t.Fatal("no child events observed")
	}
}

func TestSubWorkflowPlanFingerprintIncludesChildPlan(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	childV1 := subWorkflowChildDefinition(stringSchema, "child_action")
	childV2 := childV1
	childV2.Revision = "v2"
	childV2.Name = "Child V2"
	childV2.Nodes[1].Name = "changed semantic node name"

	parentV1 := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, childV1))
	parentV2 := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, childV2))
	action := &fakeAction{spec: actionSpec(
		"child_action",
		map[string]workflow.PortSchema{"value": stringSchema},
		map[string]workflow.PortSchema{"result": stringSchema},
	)}
	planV1 := compileWithResolver(t, parentV1, staticResolver(childV1), action)
	planV2 := compileWithResolver(t, parentV2, staticResolver(childV2), action)

	if planV1.Fingerprint() == planV2.Fingerprint() {
		t.Fatal("parent Plan fingerprint did not change with child Plan")
	}
}

func subWorkflowChildDefinition(
	stringSchema workflow.PortSchema,
	action workflow.ActionKey,
) workflow.Definition {
	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "child", Revision: "v1", Name: "Child",
		Inputs: map[string]workflow.WorkflowInput{"value": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "child_action", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "child_action", Type: workflow.NodeTypeAction,
				Version: workflow.BuiltinNodeVersion,
				Config:  json.RawMessage(fmt.Sprintf(`{"action":%q,"version":"v1"}`, action)),
				Inputs:  map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "child_action"),
			edge("child_action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func emptySubWorkflowParent(
	t *testing.T,
	stringSchema workflow.PortSchema,
	child workflow.Definition,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "empty-parent", Revision: "v1", Name: "Empty Parent",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "sub", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "sub", Type: workflow.NodeTypeSubWorkflow, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, workflow.SubWorkflowConfig{Workflow: workflowRef(t, child)}),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "sub"),
			edge("sub", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func subWorkflowParentDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
	reference workflow.DefinitionRef,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "parent", Revision: "v1", Name: "Parent",
		Inputs: map[string]workflow.WorkflowInput{"value": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "sub", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "sub", Type: workflow.NodeTypeSubWorkflow,
				Version: workflow.BuiltinNodeVersion,
				Config:  mustJSON(t, workflow.SubWorkflowConfig{Workflow: reference}),
				Inputs:  map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "sub"),
			edge("sub", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 1, MaxSteps: 100},
	}
}

func emptyLeafDefinition(id workflow.DefinitionID) workflow.Definition {
	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges:  []workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
		Limits: workflow.DefaultLimits(),
	}
}

func emptySubWorkflowDefinition(
	t *testing.T,
	id workflow.DefinitionID,
	reference workflow.DefinitionRef,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "sub", Type: workflow.NodeTypeSubWorkflow,
				Version: workflow.BuiltinNodeVersion,
				Config:  mustJSON(t, workflow.SubWorkflowConfig{Workflow: reference}),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "sub"),
			edge("sub", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func workflowRef(t *testing.T, definition workflow.Definition) workflow.DefinitionRef {
	t.Helper()

	fingerprint, err := definition.Fingerprint()
	if err != nil {
		t.Fatalf("Definition.Fingerprint() error = %v", err)
	}

	return workflow.DefinitionRef{
		ID: definition.ID, Revision: definition.Revision, Fingerprint: fingerprint,
	}
}

func staticResolver(definitions ...workflow.Definition) workflow.DefinitionResolver {
	byIdentity := make(map[string]workflow.Definition, len(definitions))
	for _, definition := range definitions {
		byIdentity[definitionMapKey(definition.ID, definition.Revision)] = definition
	}

	return workflow.DefinitionResolverFunc(func(
		_ context.Context,
		id workflow.DefinitionID,
		revision workflow.Revision,
	) (workflow.Definition, error) {
		definition, ok := byIdentity[definitionMapKey(id, revision)]
		if !ok {
			return workflow.Definition{}, errors.New("definition not found")
		}

		return definition, nil
	})
}

func definitionMapKey(id workflow.DefinitionID, revision workflow.Revision) string {
	return string(id) + "@" + string(revision)
}

func compileWithResolver(
	t *testing.T,
	definition workflow.Definition,
	resolver workflow.DefinitionResolver,
	actions ...workflow.Action,
) *workflow.Plan {
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

	plan, err := workflow.Compile(
		t.Context(),
		decoded,
		registry,
		workflow.WithDefinitionResolver(resolver),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v\ndefinition: %s", err, data)
	}

	return plan
}
