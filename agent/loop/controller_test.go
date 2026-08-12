package loop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loopClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *loopClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	return clock.now
}

func (clock *loopClock) Add(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

type loopWorkerFunc func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (function loopWorkerFunc) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return function(ctx, request)
}

func TestEveryWaitsForExplicitWakeAndDoesNotCatchUp(t *testing.T) {
	t.Parallel()

	clock := &loopClock{now: time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC)}
	controller, err := Every(5*time.Minute, WithClock(clock))
	require.NoError(t, err)
	initial, err := Prepare(ai.JSON(`{"prompt":"check deployment"}`))
	require.NoError(t, err)
	store, err := continuation.NewMemoryStore()
	require.NoError(t, err)
	engine, err := continuation.New(store, continuation.WithClock(clock))
	require.NoError(t, err)

	var workCalls atomic.Int64

	worker := loopWorkerFunc(func(_ context.Context, request continuation.WorkRequest) (continuation.WorkResult, error) {
		workCalls.Add(1)
		assert.JSONEq(t, `{"prompt":"check deployment"}`, string(request.Input))

		return continuation.WorkResult{
			Value: ai.JSON(`{"status":"running"}`), Progress: continuation.ProgressChanged,
		}, nil
	})
	workerRef := continuation.HandlerRef{Kind: "loop-test-worker", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "fixed-loop", Version: "v1"}
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID: "fixed-loop", Target: continuation.Target{Kind: "session", ID: "s1"},
		Worker: workerRef, Controller: controllerRef,
		ControllerState: initial.ControllerState, Input: initial.WorkInput,
	})
	require.NoError(t, err)

	handlers := continuation.Handlers{
		WorkerRef: workerRef, Worker: worker,
		ControllerRef: controllerRef, Controller: controller,
	}

	waiting, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers)
	require.NoError(t, err)
	require.Equal(t, continuation.StatusWaiting, waiting.Status)
	require.NotNil(t, waiting.Wait)
	require.NotNil(t, waiting.Wait.NotBefore)
	assert.Equal(t, clock.Now().Add(5*time.Minute), *waiting.Wait.NotBefore)

	_, err = engine.ResumeDue(t.Context(), waiting.ID, waiting.Revision)
	require.ErrorIs(t, err, continuation.ErrNotDue)
	assert.Equal(t, int64(1), workCalls.Load())

	clock.Add(20 * time.Minute)

	ready, err := engine.ResumeDue(t.Context(), waiting.ID, waiting.Revision)
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusReady, ready.Status)
	assert.Equal(t, continuation.ActivationTime, ready.Activation.Source)

	waiting, err = engine.Advance(t.Context(), ready.ID, ready.Revision, handlers)
	require.NoError(t, err)
	require.NotNil(t, waiting.Wait.NotBefore)
	assert.Equal(t, clock.Now().Add(5*time.Minute), *waiting.Wait.NotBefore)
	assert.Equal(t, int64(2), workCalls.Load())

	state, err := DecodeState(waiting.ControllerState)
	require.NoError(t, err)
	assert.Equal(t, 2, state.Iterations)
}

func TestControllerPersistsPlannerStateAndNewInputThenStops(t *testing.T) {
	t.Parallel()

	clock := &loopClock{now: time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC)}

	var calls atomic.Int64

	planner := PlannerFunc(func(_ context.Context, request PlanRequest) (Plan, error) {
		switch calls.Add(1) {
		case 1:
			assert.Equal(t, 1, request.Iteration)
			assert.Nil(t, request.Previous)

			return Plan{
				After: 2 * time.Minute, SignalKey: "deploy.done", Reason: "deployment still running",
				PlannerState:  ai.JSON(`{"phase":"deploy"}`),
				NextWorkInput: ai.JSON(`{"prompt":"check deploy result"}`),
				Usage:         ai.Usage{InputTokens: 3, OutputTokens: 1},
			}, nil
		default:
			assert.Equal(t, 2, request.Iteration)
			assert.JSONEq(t, `{"phase":"deploy"}`, string(request.PlannerState))
			assert.JSONEq(t, `{"prompt":"check deploy result"}`, string(request.WorkInput))
			require.NotNil(t, request.Previous)
			assert.Equal(t, 2*time.Minute, request.Previous.After)

			return Plan{Stop: true, Reason: "deployment completed"}, nil
		}
	})
	controller, err := NewController(planner, WithClock(clock))
	require.NoError(t, err)
	initial, err := Prepare(ai.JSON(`{"prompt":"check deployment"}`))
	require.NoError(t, err)

	first, err := controller.Decide(t.Context(), continuation.DecisionRequest{
		Attempt: 1, ControllerState: initial.ControllerState,
		Work: continuation.WorkResult{Value: ai.JSON(`{"status":"running"}`)},
	})
	require.NoError(t, err)
	assert.Equal(t, continuation.ActionWait, first.Action)
	require.NotNil(t, first.Wait.NotBefore)
	assert.Equal(t, clock.Now().Add(2*time.Minute), *first.Wait.NotBefore)
	require.NotNil(t, first.Wait.Signal)
	assert.Equal(t, "deploy.done", first.Wait.Signal.Key)
	assert.JSONEq(t, `{"prompt":"check deploy result"}`, string(first.NextInput))
	assert.Equal(t, ai.Usage{InputTokens: 3, OutputTokens: 1}, first.Usage)

	second, err := controller.Decide(t.Context(), continuation.DecisionRequest{
		Attempt: 2, ControllerState: first.State,
		Work: continuation.WorkResult{Value: ai.JSON(`{"status":"complete"}`)},
	})
	require.NoError(t, err)
	assert.Equal(t, continuation.ActionComplete, second.Action)
	assert.JSONEq(t, `{"iterations":2,"reason":"deployment completed"}`, string(second.Output))
}

func TestControllerRejectsInvalidPlans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		plan   Plan
		target error
	}{
		{name: "missing reason", plan: Plan{After: time.Minute}, target: ErrInvalid},
		{name: "negative delay", plan: Plan{After: -time.Second, Reason: "bad"}, target: ErrInvalid},
		{name: "no activation", plan: Plan{Reason: "bad"}, target: ErrInvalid},
		{name: "blank signal", plan: Plan{SignalKey: " ", Reason: "bad"}, target: ErrInvalid},
		{name: "negative usage", plan: Plan{After: time.Minute, Reason: "bad", Usage: ai.Usage{InputTokens: -1}}, target: ErrInvalid},
		{name: "continuing output", plan: Plan{After: time.Minute, Reason: "bad", Output: ai.JSON(`{}`)}, target: ErrInvalid},
		{name: "stop activation", plan: Plan{Stop: true, After: time.Minute, Reason: "done"}, target: ErrInvalid},
		{name: "stop next input", plan: Plan{Stop: true, Reason: "done", NextWorkInput: ai.JSON(`{}`)}, target: ErrInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller, err := NewController(PlannerFunc(func(context.Context, PlanRequest) (Plan, error) {
				return test.plan, nil
			}))
			require.NoError(t, err)
			initial, err := Prepare(ai.JSON(`{}`))
			require.NoError(t, err)

			_, err = controller.Decide(t.Context(), continuation.DecisionRequest{
				Attempt: 1, ControllerState: initial.ControllerState,
				Work: continuation.WorkResult{Value: ai.JSON(`{}`)},
			})
			require.ErrorIs(t, err, test.target)
		})
	}
}

func TestControllerPlannerErrorLeavesDecisionRetryable(t *testing.T) {
	t.Parallel()

	plannerFailure := errors.New("planner unavailable")
	controller, err := NewController(PlannerFunc(func(context.Context, PlanRequest) (Plan, error) {
		return Plan{}, plannerFailure
	}))
	require.NoError(t, err)
	initial, err := Prepare(ai.JSON(`{}`))
	require.NoError(t, err)

	_, err = controller.Decide(t.Context(), continuation.DecisionRequest{
		Attempt: 1, ControllerState: initial.ControllerState,
		Work: continuation.WorkResult{Value: ai.JSON(`{}`)},
	})
	require.ErrorIs(t, err, plannerFailure)
}

func TestConstructorsValidateDependenciesAndOptions(t *testing.T) {
	t.Parallel()

	_, err := NewController(nil)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = Every(0)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = Every(time.Minute, nil)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = Every(time.Minute, WithClock(nil))
	require.ErrorIs(t, err, ErrInvalid)
}
