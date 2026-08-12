package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestRunnerRecoversActionPanic(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction("panic", stringSchema, func(context.Context) (workflow.Value, error) {
		panic("secret panic text")
	})
	plan := compileRoundTrip(t, singleActionDefinition(t, "panic", stringSchema, workflow.NodePolicy{}), action)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if result.Status != workflow.RunStatusFailed || result.Nodes["action"].Failure != workflow.FailurePanic {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerNodeTimeoutIsNotRetried(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var attempts atomic.Int32

	action := resultAction("timeout", stringSchema, func(ctx context.Context) (workflow.Value, error) {
		attempts.Add(1)
		<-ctx.Done()

		return workflow.Value{}, ctx.Err()
	})
	policy := workflow.NodePolicy{
		TimeoutMilli: 10,
		Retry:        workflow.RetryPolicy{MaxAttempts: 3},
	}
	plan := compileRoundTrip(t, singleActionDefinition(t, "timeout", stringSchema, policy), action)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Runner.Run() error = %v, want DeadlineExceeded", err)
	}

	if attempts.Load() != 1 || result.Nodes["action"].Failure != workflow.FailureTimeout {
		t.Fatalf("attempts/failure = %d/%s, want 1/timeout", attempts.Load(), result.Nodes["action"].Failure)
	}
}

func TestRunnerEnforcesStepLimit(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("limited", "unexpected", stringSchema)
	definition := singleActionDefinition(t, "limited", stringSchema, workflow.NodePolicy{})
	definition.Limits.MaxSteps = 1
	plan := compileRoundTrip(t, definition, action)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if result.Nodes["action"].Failure != workflow.FailureLimit {
		t.Fatalf("action failure = %s, want limit", result.Nodes["action"].Failure)
	}
}

func TestRunnerErrorRoute(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	unreliable := resultAction("unreliable", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("failed")
	})
	success := constantAction("success", "success", stringSchema)
	fallback := constantAction("fallback", "fallback", stringSchema)
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeExclusive,
		Outputs: map[string]workflow.MergeOutputConfig{
			"result": {Schema: stringSchema, Sources: []string{"success", "fallback"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "error-route", Revision: "v1", Name: "Error Route",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "merge", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "unreliable", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "unreliable"), Policy: workflow.NodePolicy{Error: workflow.ErrorRoute},
			},
			{ID: "success", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "success")},
			{ID: "fallback", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "fallback")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"success":  nodeBinding("success", "result"),
					"fallback": nodeBinding("fallback", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "unreliable"),
			edge("unreliable", workflow.RouteSuccess, "success"),
			edge("unreliable", workflow.RouteError, "fallback"),
			edge("success", workflow.RouteSuccess, "merge"),
			edge("fallback", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	plan := compileRoundTrip(t, definition, unreliable, success, fallback)

	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded {
		t.Fatalf("RunResult.Status = %s, want partial-succeeded", result.Status)
	}

	if result.Outputs["result"].String() != `"fallback"` {
		t.Fatalf("result = %s, want fallback", result.Outputs["result"].String())
	}

	if result.Nodes["unreliable"].Status != workflow.NodeStatusException ||
		result.Nodes["success"].Status != workflow.NodeStatusSkipped {
		t.Fatalf("node states = %#v", result.Nodes)
	}
}

func TestRunnerContinuesWithValidatedDefault(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction("default", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("failed")
	})
	policy := workflow.NodePolicy{
		Error:          workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{"result": workflow.MustValueOf("default")},
	}
	plan := compileRoundTrip(t, singleActionDefinition(t, "default", stringSchema, policy), action)

	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Outputs["result"].String() != `"default"` ||
		result.Nodes["action"].Status != workflow.NodeStatusException {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerRoutesStableFailureKinds(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)

	tests := []struct {
		name        string
		invoke      func(context.Context) (workflow.ActionOutput, error)
		timeout     int64
		wantFailure workflow.FailureKind
	}{
		{
			name: "ordinary error",
			invoke: func(context.Context) (workflow.ActionOutput, error) {
				return workflow.ActionOutput{}, errors.New("ordinary")
			},
			wantFailure: workflow.FailureError,
		},
		{
			name: "timeout",
			invoke: func(ctx context.Context) (workflow.ActionOutput, error) {
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			},
			timeout:     5,
			wantFailure: workflow.FailureTimeout,
		},
		{
			name: "panic",
			invoke: func(context.Context) (workflow.ActionOutput, error) {
				panic("classified panic")
			},
			wantFailure: workflow.FailurePanic,
		},
		{
			name: "node returned cancellation",
			invoke: func(context.Context) (workflow.ActionOutput, error) {
				return workflow.ActionOutput{}, context.Canceled
			},
			wantFailure: workflow.FailureCanceled,
		},
		{
			name: "invalid output",
			invoke: func(context.Context) (workflow.ActionOutput, error) {
				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf(42),
				}}, nil
			},
			wantFailure: workflow.FailureError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition := failureBranchDefinition(t, stringSchema, 1)
			definition.Nodes[1].Policy.TimeoutMilli = test.timeout
			source := &fakeAction{
				spec: actionSpec(
					"typed_failure",
					map[string]workflow.PortSchema{},
					map[string]workflow.PortSchema{"result": stringSchema},
				),
				run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
					return test.invoke(ctx)
				},
			}
			handler := &fakeAction{
				spec: actionSpec(
					"typed_handler",
					map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
					map[string]workflow.PortSchema{"result": stringSchema},
				),
				run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
					return workflow.ActionOutput{Values: map[string]workflow.Value{
						"result": input.Values["type"],
					}}, nil
				},
			}
			plan := compileRoundTrip(
				t,
				definition,
				source,
				constantAction("typed_success", "success", stringSchema),
				handler,
			)
			runner := mustRunner(t)

			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			if err != nil {
				t.Fatalf("Runner.Run() error = %v", err)
			}

			if result.Status != workflow.RunStatusPartialSucceeded ||
				result.Nodes["unreliable"].Failure != test.wantFailure ||
				result.Outputs["result"].String() != fmt.Sprintf("%q", test.wantFailure) {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestRunnerCancellationDoesNotStartSuccessor(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})
	blocking := &fakeAction{
		spec: actionSpec("blocking", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{}),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			close(started)
			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}

	var successorRuns atomic.Int32

	successor := resultAction("successor", stringSchema, func(context.Context) (workflow.Value, error) {
		successorRuns.Add(1)

		return workflow.MustValueOf("unexpected"), nil
	})
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "cancel", Revision: "v1", Name: "Cancel",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(stringSchema, "successor", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "blocking", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "blocking")},
			{ID: "successor", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "successor")},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "blocking"),
			edge("blocking", workflow.RouteSuccess, "successor"),
			edge("successor", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, blocking, successor)
	runner := mustRunner(t)
	runContext, cancel := context.WithCancel(t.Context())
	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, runErr := runner.Run(runContext, plan, map[string]workflow.Value{})
		resultChannel <- result

		errorChannel <- runErr
	}()

	<-started
	cancel()

	if err := <-errorChannel; !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	result := <-resultChannel
	if result.Status != workflow.RunStatusCanceled || successorRuns.Load() != 0 {
		t.Fatalf("status/successor runs = %s/%d", result.Status, successorRuns.Load())
	}

	if result.Nodes["blocking"].Status != workflow.NodeStatusFailed ||
		result.Nodes["blocking"].Failure != workflow.FailureCanceled {
		t.Fatalf("blocking node = %#v, want failed/canceled", result.Nodes["blocking"])
	}
}

func TestRunnerEventsAreTypedAndPayloadFree(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction("event", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("credential=do-not-export")
	})
	policy := workflow.NodePolicy{
		Error:          workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{"result": workflow.MustValueOf("safe")},
	}
	plan := compileRoundTrip(t, singleActionDefinition(t, "event", stringSchema, policy), action)
	events := []workflow.Event{}

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "events", nil }),
		workflow.WithEventSink(func(event workflow.Event) {
			events = append(events, event)
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Nodes["action"].Status != workflow.NodeStatusException {
		t.Fatalf("result = %#v", result)
	}

	if len(events) == 0 {
		t.Fatal("no events observed")
	}

	var sawNodeFailure, sawRunCompleted bool

	for _, event := range events {
		if event.RunID != "events" || event.PlanFingerprint == "" || event.Time.IsZero() {
			t.Fatalf("invalid envelope: %#v", event)
		}

		if strings.Contains(fmt.Sprintf("%#v", event.Payload()), "do-not-export") {
			t.Fatalf("event leaked Action error: %#v", event.Payload())
		}

		switch payload := event.Payload().(type) {
		case workflow.NodeFailed:
			sawNodeFailure = payload.Kind == workflow.FailureError
		case workflow.RunCompleted:
			sawRunCompleted = true
		}
	}

	if !sawNodeFailure || !sawRunCompleted {
		t.Fatalf("missing terminal metadata events: %#v", events)
	}

	if _, err := json.Marshal(events[0]); !errors.Is(err, workflow.ErrEventWireFormat) {
		t.Fatalf("json.Marshal(Event) error = %v, want ErrEventWireFormat", err)
	}
}

func singleActionDefinition(
	t *testing.T,
	key workflow.ActionKey,
	schema workflow.PortSchema,
	policy workflow.NodePolicy,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "single", Revision: "v1", Name: "Single",
		Inputs:  map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{"result": nodeOutput(schema, "action", "result")},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, key), Policy: policy,
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func resultAction(
	key workflow.ActionKey,
	schema workflow.PortSchema,
	run func(context.Context) (workflow.Value, error),
) *fakeAction {
	return &fakeAction{
		spec: actionSpec(key, map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{"result": schema}),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			value, err := run(ctx)
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": value}}, nil
		},
	}
}

func mustRunner(t *testing.T) *workflow.Runner {
	t.Helper()

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "run-failure", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	return runner
}
