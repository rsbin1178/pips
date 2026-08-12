package loop_test

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/loop"
	"github.com/rsbin1178/pips/ai"
)

type exampleClock struct {
	now time.Time
}

func (clock *exampleClock) Now() time.Time { return clock.now }

type exampleWorker func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (worker exampleWorker) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return worker(ctx, request)
}

func ExampleEvery() {
	clock := &exampleClock{now: time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC)}
	controller, _ := loop.Every(5*time.Minute,
		loop.WithClock(clock),
	)
	setup, _ := loop.Prepare(ai.JSON(`{"prompt":"check deployment"}`))
	store, _ := continuation.NewMemoryStore()
	engine, _ := continuation.New(store, continuation.WithClock(clock))
	workerRef := continuation.HandlerRef{Kind: "deployment-check", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "fixed-loop", Version: "v1"}
	execution, _ := engine.Create(context.Background(), continuation.CreateRequest{
		ID: "deployment-loop", Target: continuation.Target{Kind: "session", ID: "deploy-1"},
		Worker: workerRef, Controller: controllerRef,
		ControllerState: setup.ControllerState, Input: setup.WorkInput,
	})

	waiting, _ := engine.Advance(context.Background(), execution.ID, execution.Revision, continuation.Handlers{
		WorkerRef: workerRef,
		Worker: exampleWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
			return continuation.WorkResult{
				Value: ai.JSON(`{"status":"running"}`), Progress: continuation.ProgressChanged,
			}, nil
		}),
		ControllerRef: controllerRef, Controller: controller,
	})
	clock.now = clock.now.Add(5 * time.Minute)
	ready, _ := engine.ResumeDue(context.Background(), waiting.ID, waiting.Revision)

	fmt.Println(waiting.Status, ready.Status)
	// Output: waiting ready
}
