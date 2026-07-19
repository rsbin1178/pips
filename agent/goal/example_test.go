package goal_test

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/goal"
	"github.com/rsbin/pips/ai"
)

type exampleWorker func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (worker exampleWorker) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return worker(ctx, request)
}

func ExampleController() {
	setup, _ := goal.Prepare("tests pass")
	controller, _ := goal.NewController(goal.EvaluatorFunc(func(
		_ context.Context,
		evaluation goal.Evaluation,
	) (goal.EvaluationResult, error) {
		if string(evaluation.Evidence) == `{"tests":"pass"}` {
			return goal.EvaluationResult{
				Outcome: goal.OutcomeComplete, Reason: "test evidence is passing",
			}, nil
		}

		return goal.EvaluationResult{
			Outcome: goal.OutcomeContinue, Reason: "passing test evidence is missing",
		}, nil
	}))
	store, _ := continuation.NewMemoryStore()
	engine, _ := continuation.New(store)
	workerRef := continuation.HandlerRef{Kind: "test-worker", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "goal", Version: "v1"}
	execution, _ := engine.Create(context.Background(), continuation.CreateRequest{
		ID: "release-goal", Target: continuation.Target{Kind: "session", ID: "release-1"},
		Worker: workerRef, Controller: controllerRef,
		ControllerState: setup.ControllerState, Input: setup.WorkInput,
	})

	finished, _ := engine.Advance(context.Background(), execution.ID, execution.Revision, continuation.Handlers{
		WorkerRef: workerRef,
		Worker: exampleWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
			return continuation.WorkResult{
				Value: ai.JSON(`{"tests":"pass"}`), Progress: continuation.ProgressChanged,
			}, nil
		}),
		ControllerRef: controllerRef, Controller: controller,
	})

	fmt.Println(finished.Status, finished.Reason)
	// Output: completed test evidence is passing
}
