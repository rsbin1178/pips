package continuation_test

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

type exampleWorker func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (worker exampleWorker) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return worker(ctx, request)
}

type exampleController func(context.Context, continuation.DecisionRequest) (continuation.Decision, error)

func (controller exampleController) Decide(
	ctx context.Context,
	request continuation.DecisionRequest,
) (continuation.Decision, error) {
	return controller(ctx, request)
}

func exampleEngine() (*continuation.Engine, continuation.Handlers, error) {
	store, err := continuation.NewMemoryStore()
	if err != nil {
		return nil, continuation.Handlers{}, err
	}

	engine, err := continuation.New(store)
	if err != nil {
		return nil, continuation.Handlers{}, err
	}

	workerRef := continuation.HandlerRef{Kind: "example-worker", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "example-controller", Version: "v1"}
	worker := exampleWorker(func(_ context.Context, request continuation.WorkRequest) (continuation.WorkResult, error) {
		return continuation.WorkResult{
			Value:    ai.JSON(fmt.Sprintf(`{"attempt":%d}`, request.Attempt)),
			Progress: continuation.ProgressChanged,
		}, nil
	})

	return engine, continuation.Handlers{
		WorkerRef: workerRef, Worker: worker,
		ControllerRef: controllerRef,
	}, nil
}

func Example_goalStyle() {
	engine, handlers, _ := exampleEngine()
	handlers.Controller = exampleController(func(
		_ context.Context,
		request continuation.DecisionRequest,
	) (continuation.Decision, error) {
		if request.Attempt < 2 {
			return continuation.Decision{
				Action: continuation.ActionContinue, Progress: continuation.ProgressChanged,
			}, nil
		}

		return continuation.Decision{Action: continuation.ActionComplete, Reason: "goal satisfied"}, nil
	})

	execution, _ := engine.Create(context.Background(), continuation.CreateRequest{
		ID: "goal-example", Target: continuation.Target{Kind: "session", ID: "s1"},
		Worker: handlers.WorkerRef, Controller: handlers.ControllerRef,
	})
	result, _ := engine.Drive(context.Background(), execution.ID, execution.Revision, handlers,
		continuation.DriveOptions{MaxAdvances: 3})

	fmt.Println(result.Execution.Status, result.Execution.Accounting.Attempts)
	// Output: completed 2
}

func Example_loopStyle() {
	engine, handlers, _ := exampleEngine()
	wakeAt := time.Now().Add(time.Hour)
	handlers.Controller = exampleController(func(
		context.Context,
		continuation.DecisionRequest,
	) (continuation.Decision, error) {
		return continuation.Decision{
			Action: continuation.ActionWait,
			Wait: &continuation.WaitCondition{
				NotBefore: &wakeAt, Signal: &continuation.SignalSpec{Key: "maintenance.tick"},
			},
		}, nil
	})

	execution, _ := engine.Create(context.Background(), continuation.CreateRequest{
		ID: "loop-example", Target: continuation.Target{Kind: "session", ID: "s2"},
		Worker: handlers.WorkerRef, Controller: handlers.ControllerRef,
	})
	waiting, _ := engine.Advance(context.Background(), execution.ID, execution.Revision, handlers)
	ready, _ := engine.Signal(context.Background(), waiting.ID, waiting.Revision,
		continuation.Signal{ID: "tick-1", Key: "maintenance.tick"})

	fmt.Println(waiting.Status, ready.Status)
	// Output: waiting ready
}

func Example_teamWorkflowStyle() {
	engine, handlers, _ := exampleEngine()
	handlers.Controller = exampleController(func(
		context.Context,
		continuation.DecisionRequest,
	) (continuation.Decision, error) {
		return continuation.Decision{
			Action: continuation.ActionBlock,
			Block:  &continuation.Block{Kind: "dependency", Data: ai.JSON(`{"task":"build"}`)},
		}, nil
	})

	execution, _ := engine.Create(context.Background(), continuation.CreateRequest{
		ID: "workflow-example", Target: continuation.Target{Kind: "workflow_node", ID: "test"},
		Worker: handlers.WorkerRef, Controller: handlers.ControllerRef,
	})
	blocked, _ := engine.Advance(context.Background(), execution.ID, execution.Revision, handlers)
	ready, _ := engine.ResolveBlock(context.Background(), blocked.ID, blocked.Revision, ai.JSON(`{"task":"build","status":"done"}`))

	fmt.Println(blocked.Status, ready.Status)
	// Output: blocked ready
}
