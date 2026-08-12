package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/workflow"
)

func TestRunPartialHandledDefaultFailureIsNotReusable(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction("partial-default", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("failed")
	})
	policy := workflow.NodePolicy{
		Error: workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{
			"result": workflow.MustValueOf("default"),
		},
	}
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "partial-default", stringSchema, policy),
		action,
	)
	runner := mustRunner(t)

	result, err := runner.RunPartial(t.Context(), plan, "action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded ||
		result.Nodes["action"].NodeRun.Status != workflow.NodeStatusException ||
		result.Nodes["action"].Origin != workflow.PartialDataExecuted ||
		result.Outputs["result"].String() != `"default"` {
		t.Fatalf("handled failure result = %#v", result)
	}

	if _, reusable := result.Data.Nodes["action"]; reusable {
		t.Fatalf("handled failure became reusable: %#v", result.Data)
	}
}

func TestRunPartialReexecutesErrorHandlerWhenExceptionSourceIsMissing(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)

	var sourceCalls, handlerCalls atomic.Int32

	source := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			sourceCalls.Add(1)

			return workflow.Value{}, errors.New("partial branch failure")
		},
	)
	handler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			handlerCalls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["message"],
			}}, nil
		},
	}
	plan := compileRoundTrip(
		t,
		failureBranchDefinition(t, stringSchema, 1),
		source,
		constantAction("typed_success", "success", stringSchema),
		handler,
	)
	runner := mustRunner(t)

	first, err := runner.RunPartial(t.Context(), plan, "handler", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(first) error = %v", err)
	}

	if _, reusable := first.Data.Nodes["unreliable"]; reusable {
		t.Fatalf("exception source became reusable: %#v", first.Data)
	}

	if _, reusable := first.Data.Nodes["handler"]; !reusable {
		t.Fatalf("successful handler is missing from first data: %#v", first.Data)
	}

	second, err := runner.RunPartial(t.Context(), plan, "handler", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{},
		Previous: &first.Data,
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(second) error = %v", err)
	}

	if sourceCalls.Load() != 2 || handlerCalls.Load() != 2 ||
		second.Nodes["handler"].Origin != workflow.PartialDataExecuted {
		t.Fatalf(
			"second result = %#v, calls = %d/%d",
			second,
			sourceCalls.Load(),
			handlerCalls.Load(),
		)
	}
}

func TestRunPartialPreservesRetryAndStopFailure(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	retry := resultAction("partial-retry", stringSchema, func(context.Context) (workflow.Value, error) {
		if calls.Add(1) == 1 {
			return workflow.Value{}, errors.New("retry")
		}

		return workflow.MustValueOf("retried"), nil
	})
	retryPolicy := workflow.NodePolicy{Retry: workflow.RetryPolicy{MaxAttempts: 2}}
	retryPlan := compileRoundTrip(
		t,
		singleActionDefinition(t, "partial-retry", stringSchema, retryPolicy),
		retry,
	)
	runner := mustRunner(t)

	retried, err := runner.RunPartial(t.Context(), retryPlan, "action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(retry) error = %v", err)
	}

	if retried.Nodes["action"].NodeRun.Attempts != 2 ||
		retried.Nodes["action"].Origin != workflow.PartialDataExecuted ||
		retried.Outputs["result"].String() != `"retried"` {
		t.Fatalf("retry result = %#v", retried)
	}

	stop := resultAction("partial-stop", stringSchema, func(context.Context) (workflow.Value, error) {
		return workflow.Value{}, errors.New("stop")
	})
	stopPlan := compileRoundTrip(
		t,
		singleActionDefinition(t, "partial-stop", stringSchema, workflow.NodePolicy{}),
		stop,
	)

	stopped, err := runner.RunPartial(t.Context(), stopPlan, "action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	if !errors.Is(err, workflow.ErrRun) || stopped.Status != workflow.RunStatusFailed ||
		stopped.Nodes["action"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("stop result = (%#v, %v)", stopped, err)
	}

	if _, reusable := stopped.Data.Nodes["action"]; reusable {
		t.Fatalf("stopped node became reusable: %#v", stopped.Data)
	}
}

func TestRunPartialCancellationDrainsAttemptWithoutReusableData(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})
	action := &fakeAction{
		spec: actionSpec(
			"partial-cancel",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			close(started)
			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "partial-cancel", stringSchema, workflow.NodePolicy{}),
		action,
	)
	runner := mustRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	type runResponse struct {
		result workflow.PartialRunResult
		err    error
	}

	responseChannel := make(chan runResponse, 1)

	go func() {
		result, runErr := runner.RunPartial(ctx, plan, "action", workflow.PartialRunInput{
			Inputs: map[string]workflow.Value{},
		})
		responseChannel <- runResponse{result: result, err: runErr}
	}()

	<-started
	cancel()

	response := <-responseChannel

	if !errors.Is(response.err, context.Canceled) ||
		response.result.Status != workflow.RunStatusCanceled ||
		response.result.Nodes["action"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("canceled Partial Run = (%#v, %v)", response.result, response.err)
	}

	if _, reusable := response.result.Data.Nodes["action"]; reusable {
		t.Fatalf("canceled Action became reusable: %#v", response.result.Data)
	}
}

func TestRunPartialSubstitutedNodeDoesNotConsumeStepLimit(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("partial-step-limit", "completed", stringSchema)
	definition := singleActionDefinition(
		t,
		"partial-step-limit",
		stringSchema,
		workflow.NodePolicy{},
	)
	definition.Limits.MaxSteps = 1
	plan := compileRoundTrip(t, definition, action)
	runner := mustRunner(t)

	limited, err := runner.RunPartial(t.Context(), plan, "action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
	})
	if !errors.Is(err, workflow.ErrRun) ||
		limited.Nodes["action"].NodeRun.Failure != workflow.FailureLimit {
		t.Fatalf("ordinary Partial Run limit = (%#v, %v)", limited, err)
	}

	completed, err := runner.RunPartial(t.Context(), plan, "action", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins:   workflow.PinData{"start": {}},
	})
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["result"].String() != `"completed"` {
		t.Fatalf("Partial Run with substituted Start = (%#v, %v)", completed, err)
	}

	if completed.Nodes["start"].Origin != workflow.PartialDataPinned ||
		completed.Nodes["action"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("Partial Run step origins = %#v", completed.Nodes)
	}
}
