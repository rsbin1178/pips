package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestDebugNodeFailurePolicies(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	failing := resultAction("debug-failure", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("debug failure text")
	})

	tests := []struct {
		name       string
		definition workflow.Definition
		wantStatus workflow.RunStatus
		wantRoute  string
		wantOutput string
		wantError  bool
	}{
		{
			name: "stop",
			definition: singleActionDefinition(
				t, "debug-failure", stringSchema, workflow.NodePolicy{},
			),
			wantStatus: workflow.RunStatusFailed,
			wantError:  true,
		},
		{
			name: "continue with default",
			definition: singleActionDefinition(t, "debug-failure", stringSchema, workflow.NodePolicy{
				Error: workflow.ErrorContinueWithDefault,
				DefaultOutputs: map[string]workflow.Value{
					"result": workflow.MustValueOf("default"),
				},
			}),
			wantStatus: workflow.RunStatusSucceeded,
			wantRoute:  workflow.RouteSuccess,
			wantOutput: `"default"`,
		},
		{
			name:       "error route",
			definition: debugErrorRouteDefinition(t, stringSchema),
			wantStatus: workflow.RunStatusSucceeded,
			wantRoute:  workflow.RouteError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actions := []workflow.Action{failing}
			if test.name == "error route" {
				actions = append(
					actions,
					constantAction("debug-success", "success", stringSchema),
					constantAction("debug-fallback", "fallback", stringSchema),
				)
			}

			plan := compileRoundTrip(t, test.definition, actions...)

			debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
			if err != nil {
				t.Fatalf("PrepareNodeDebug() error = %v", err)
			}

			runner, _ := workflow.NewRunner()

			result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
			if test.wantError != errors.Is(err, workflow.ErrRun) {
				t.Fatalf("DebugNode() error = %v, want ErrRun=%v", err, test.wantError)
			}

			if result.Status != test.wantStatus ||
				result.Execution.NodeRun.Status != workflow.NodeStatusFailed ||
				result.Execution.Route != test.wantRoute ||
				!strings.Contains(result.Execution.ErrorMessage, "debug failure text") {
				t.Fatalf("DebugNode() result = %#v", result)
			}

			if test.wantOutput != "" && result.Execution.Outputs["result"].String() != test.wantOutput {
				t.Fatalf("DebugNode() output = %s", result.Execution.Outputs["result"].String())
			}
		})
	}
}

func TestDebugNodeMergeUsesOptionalCandidateInputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	boolSchema := mustSchema(t, `{"type":"boolean"}`)
	trueValue := workflow.MustValueOf(true)
	conditionConfig := workflow.ConditionConfig{
		Predicate: workflow.Predicate{
			Op: workflow.PredicateEqual, Input: "approved", Value: &trueValue,
		},
		TrueRoute: "yes", FalseRoute: "no",
	}
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"accepted", "rejected"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-merge", Revision: "v1", Name: "Debug Merge",
		Inputs: map[string]workflow.PortSchema{"approved": boolSchema},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, conditionConfig), Inputs: map[string]workflow.Binding{
					"approved": workflowInput("approved"),
				},
			},
			{ID: "accept", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "debug-accept")},
			{ID: "reject", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "debug-reject")},
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
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "yes", "accept"),
			edge("condition", "no", "reject"),
			edge("accept", workflow.RouteSuccess, "merge"),
			edge("reject", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(
		t,
		definition,
		constantAction("debug-accept", "accepted", stringSchema),
		constantAction("debug-reject", "rejected", stringSchema),
	)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("merge"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	if !slices.Equal(debugPlan.Spec().OptionalInputs, []string{"accepted", "rejected"}) {
		t.Fatalf("OptionalInputs = %#v", debugPlan.Spec().OptionalInputs)
	}

	runner, _ := workflow.NewRunner()

	result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"rejected": workflow.MustValueOf("selected"),
	})
	if err != nil || result.Execution.Outputs["result"].String() != `"selected"` {
		t.Fatalf("DebugNode(one candidate) result=%#v error=%v", result, err)
	}

	result, err = runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"accepted": workflow.MustValueOf("a"), "rejected": workflow.MustValueOf("b"),
	})
	if !errors.Is(err, workflow.ErrRun) || result.Execution.NodeRun.Status != workflow.NodeStatusFailed {
		t.Fatalf("DebugNode(conflict) result=%#v error=%v", result, err)
	}
}

func TestDebugNodeEventsUseDerivedPlanWithoutEndpointNodes(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("debug-event", "ok", stringSchema)
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "debug-event", stringSchema, workflow.NodePolicy{}),
		action,
	)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	events := make([]workflow.Event, 0)
	runner, _ := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-events", nil }),
		workflow.WithEventSink(func(event workflow.Event) { events = append(events, event) }),
	)

	_, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("event count = %d, want 5: %#v", len(events), events)
	}

	for _, event := range events {
		if event.PlanFingerprint != debugPlan.Fingerprint() ||
			event.DefinitionID != plan.DefinitionID() {
			t.Fatalf("event identity = %#v", event)
		}

		if event.NodeID != "" && event.NodeID != "action" {
			t.Fatalf("unexpected endpoint event = %#v", event)
		}
	}
}

func TestDebugNodeConcurrentPlanReuseIsIsolated(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec("debug-concurrent", map[string]workflow.PortSchema{
			"value": stringSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-concurrent", Revision: "v1", Name: "Debug Concurrent",
		Inputs: map[string]workflow.PortSchema{"value": stringSchema},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "action", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "debug-concurrent"), Inputs: map[string]workflow.Binding{
					"value": workflowInput("value"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, action)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	runner, _ := workflow.NewRunner()

	const runs = 24

	errorsChannel := make(chan error, runs)

	var group sync.WaitGroup
	for index := range runs {
		group.Go(func() {
			want := fmt.Sprintf("value-%d", index)

			result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
				"value": workflow.MustValueOf(want),
			})
			if err != nil {
				errorsChannel <- err

				return
			}

			if result.Execution.Outputs["result"].String() != workflow.MustValueOf(want).String() {
				errorsChannel <- fmt.Errorf("result = %s, want %q", result.Execution.Outputs["result"].String(), want)
			}
		})
	}

	group.Wait()
	close(errorsChannel)

	for err := range errorsChannel {
		t.Error(err)
	}
}

func TestDebugNodeCancellationReturnsFinalExecutionRecord(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	action := &fakeAction{
		spec: actionSpec("debug-cancel", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{}),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			close(started)
			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-cancel", Revision: "v1", Name: "Debug Cancel",
		Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"debug-cancel","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, action)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	runner, _ := workflow.NewRunner()
	runContext, cancel := context.WithCancel(t.Context())
	resultChannel := make(chan workflow.NodeDebugResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.DebugNode(runContext, debugPlan, map[string]workflow.Value{})
		resultChannel <- result

		errorChannel <- err
	}()

	<-started
	cancel()

	err := <-errorChannel

	result := <-resultChannel
	if !errors.Is(err, context.Canceled) || result.Status != workflow.RunStatusCanceled ||
		result.Execution.NodeRun.Status != workflow.NodeStatusFailed ||
		result.Execution.NodeRun.Failure != workflow.FailureCanceled ||
		!strings.Contains(result.Execution.ErrorMessage, context.Canceled.Error()) {
		t.Fatalf("DebugNode() result=%#v error=%v", result, err)
	}
}

func debugErrorRouteDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"success", "fallback"}},
		},
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-error-route", Revision: "v1", Name: "Debug Error Route",
		Inputs: map[string]workflow.PortSchema{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "merge", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "debug-failure"),
				Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{ID: "success", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "debug-success")},
			{ID: "fallback", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "debug-fallback")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig), Inputs: map[string]workflow.Binding{
					"success":  nodeBinding("success", "result"),
					"fallback": nodeBinding("fallback", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "success"),
			edge("action", workflow.RouteError, "fallback"),
			edge("success", workflow.RouteSuccess, "merge"),
			edge("fallback", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}
