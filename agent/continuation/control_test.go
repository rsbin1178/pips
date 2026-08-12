package continuation

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitResumesBySignalOrTimeAndDeduplicates(t *testing.T) {
	t.Parallel()

	engine, _, clock := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	due := clock.Now().Add(time.Hour)
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return changedWork(`{"ready":true}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{
			Action: ActionWait,
			Wait:   &WaitCondition{NotBefore: &due, Signal: &SignalSpec{Key: "task.done"}},
		}, nil
	})

	waiting, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, waiting.Status)

	_, err = engine.ResumeDue(t.Context(), waiting.ID, waiting.Revision)
	require.ErrorIs(t, err, ErrNotDue)
	_, err = engine.Signal(t.Context(), waiting.ID, waiting.Revision, Signal{ID: "signal-wrong", Key: "other"})
	require.ErrorIs(t, err, ErrSignalMismatch)

	ready, err := engine.Signal(t.Context(), waiting.ID, waiting.Revision, Signal{
		ID: "signal-1", Key: "task.done", Payload: ai.JSON(`{"task":"t1"}`),
	})
	require.NoError(t, err)
	require.Equal(t, StatusReady, ready.Status)
	require.NotNil(t, ready.Activation)
	assert.Equal(t, ActivationSignal, ready.Activation.Source)
	assert.Equal(t, ai.JSON(`{"task":"t1"}`), ready.Activation.Payload)

	historyBefore, err := engine.History(t.Context(), ready.ID)
	require.NoError(t, err)
	duplicate, err := engine.Signal(t.Context(), ready.ID, waiting.Revision, Signal{
		ID: "signal-1", Key: "task.done", Payload: ai.JSON(`{"ignored":true}`),
	})
	require.NoError(t, err)
	assert.Equal(t, ready.Revision, duplicate.Revision)
	historyAfter, err := engine.History(t.Context(), ready.ID)
	require.NoError(t, err)
	assert.Len(t, historyAfter, len(historyBefore))
}

func TestWaitResumesWhenTimeDue(t *testing.T) {
	t.Parallel()

	engine, _, clock := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	due := clock.Now().Add(time.Minute)
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionWait, Wait: &WaitCondition{
			NotBefore: &due, Signal: &SignalSpec{Key: "fallback"},
		}}, nil
	})

	waiting, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	clock.Add(time.Minute)

	ready, err := engine.ResumeDue(t.Context(), waiting.ID, waiting.Revision)
	require.NoError(t, err)
	assert.Equal(t, StatusReady, ready.Status)
	assert.Equal(t, ActivationTime, ready.Activation.Source)
}

func TestBlockResolutionCreatesActivationAndNewAttempt(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	var activation *Activation

	worker := workerFunc(func(_ context.Context, request WorkRequest) (WorkResult, error) {
		activation = cloneActivation(request.Activation)

		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(_ context.Context, request DecisionRequest) (Decision, error) {
		if request.Attempt == 1 {
			return Decision{Action: ActionBlock, Block: &Block{Kind: "approval", Data: ai.JSON(`{"scope":"write"}`)}}, nil
		}

		return Decision{Action: ActionComplete}, nil
	})

	blocked, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	require.Equal(t, StatusBlocked, blocked.Status)
	resolved, err := engine.ResolveBlock(t.Context(), blocked.ID, blocked.Revision, ai.JSON(`{"approved":true}`))
	require.NoError(t, err)
	finished, err := engine.Advance(t.Context(), resolved.ID, resolved.Revision, handlers(worker, controller))
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, finished.Status)
	require.NotNil(t, activation)
	assert.Equal(t, ActivationBlock, activation.Source)
	assert.Equal(t, ai.JSON(`{"approved":true}`), activation.Payload)
}

func TestCancelledWaitCannotWake(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionWait, Wait: &WaitCondition{Signal: &SignalSpec{Key: "wake"}}}, nil
	})

	waiting, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	cancelled, err := engine.Cancel(t.Context(), waiting.ID, waiting.Revision, "stop")
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)

	_, err = engine.Signal(t.Context(), cancelled.ID, cancelled.Revision, Signal{ID: "signal-late", Key: "wake"})
	require.ErrorIs(t, err, ErrTerminal)
	_, err = engine.ResumeDue(t.Context(), cancelled.ID, cancelled.Revision)
	assert.ErrorIs(t, err, ErrTerminal)
}

func TestJSONLSignalDeduplicationSurvivesReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)

	clock := &mutableClock{now: time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC)}
	engine, err := New(store, WithClock(clock))
	require.NoError(t, err)
	execution := createTestExecution(t, engine, Limits{})
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionWait, Wait: &WaitCondition{Signal: &SignalSpec{Key: "wake"}}}, nil
	})

	waiting, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	ready, err := engine.Signal(t.Context(), waiting.ID, waiting.Revision, Signal{ID: "durable-signal", Key: "wake"})
	require.NoError(t, err)

	reopenedStore, err := NewJSONLStore(dir)
	require.NoError(t, err)
	reopened, err := New(reopenedStore, WithClock(clock))
	require.NoError(t, err)
	historyBefore, err := reopened.History(t.Context(), ready.ID)
	require.NoError(t, err)
	duplicate, err := reopened.Signal(t.Context(), ready.ID, waiting.Revision, Signal{ID: "durable-signal", Key: "wake"})
	require.NoError(t, err)
	assert.Equal(t, ready.Revision, duplicate.Revision)
	historyAfter, err := reopened.History(t.Context(), ready.ID)
	require.NoError(t, err)
	assert.Len(t, historyAfter, len(historyBefore))
}

func TestPausedExecutionSurvivesJSONLReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	engine, err := New(store)
	require.NoError(t, err)
	execution := createTestExecution(t, engine, Limits{})
	paused, err := engine.Pause(t.Context(), execution.ID, execution.Revision, "maintenance")
	require.NoError(t, err)
	require.Equal(t, StatusPaused, paused.Status)

	reopenedStore, err := NewJSONLStore(dir)
	require.NoError(t, err)
	reopened, err := New(reopenedStore)
	require.NoError(t, err)
	loaded, err := reopened.Get(t.Context(), paused.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, loaded.Status)
	resumed, err := reopened.Resume(t.Context(), loaded.ID, loaded.Revision, "ready")
	require.NoError(t, err)
	assert.Equal(t, execution.ID, resumed.ID)
	assert.Equal(t, StatusReady, resumed.Status)
}
