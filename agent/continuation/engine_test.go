package continuation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testWorkerRef     = HandlerRef{Kind: "test-worker", Version: "v1"}
	testControllerRef = HandlerRef{Kind: "test-controller", Version: "v1"}
)

type workerFunc func(context.Context, WorkRequest) (WorkResult, error)

func (function workerFunc) Run(ctx context.Context, request WorkRequest) (WorkResult, error) {
	return function(ctx, request)
}

type controllerFunc func(context.Context, DecisionRequest) (Decision, error)

func (function controllerFunc) Decide(ctx context.Context, request DecisionRequest) (Decision, error) {
	return function(ctx, request)
}

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *mutableClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	return clock.now
}

func (clock *mutableClock) Add(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func newTestEngine(t *testing.T) (*Engine, *MemoryStore, *mutableClock) {
	t.Helper()

	store, err := NewMemoryStore()
	require.NoError(t, err)

	clock := &mutableClock{now: time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC)}

	var sequence atomic.Int64

	engine, err := New(store,
		WithClock(clock),
		WithIDSource(func(prefix string, _ time.Time) (string, error) {
			return fmt.Sprintf("%s-%d", prefix, sequence.Add(1)), nil
		}),
	)
	require.NoError(t, err)

	return engine, store, clock
}

func createTestExecution(t *testing.T, engine *Engine, limits Limits) Execution {
	t.Helper()

	execution, err := engine.Create(t.Context(), CreateRequest{
		ID: "execution-1", Target: Target{Kind: "session", ID: "session-1"},
		Worker: testWorkerRef, Controller: testControllerRef,
		Input: ai.JSON(`"first"`), Limits: limits,
	})
	require.NoError(t, err)

	return execution
}

func handlers(worker Worker, controller Controller) Handlers {
	return Handlers{
		WorkerRef: testWorkerRef, Worker: worker,
		ControllerRef: testControllerRef, Controller: controller,
	}
}

func changedWork(value string) WorkResult {
	return WorkResult{
		Value: ai.JSON(value), Turns: 1,
		Usage: ai.Usage{InputTokens: 2, OutputTokens: 3}, Progress: ProgressChanged,
	}
}

func TestAdvanceContinuesWithControllerInputAndCompletes(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	var inputs []string

	worker := workerFunc(func(_ context.Context, request WorkRequest) (WorkResult, error) {
		inputs = append(inputs, string(request.Input))

		return changedWork(fmt.Sprintf(`{"attempt":%d}`, request.Attempt)), nil
	})
	controller := controllerFunc(func(_ context.Context, request DecisionRequest) (Decision, error) {
		if request.Attempt == 1 {
			return Decision{
				Action: ActionContinue, NextInput: ai.JSON(`"second"`),
				State: ai.JSON(`null`), Progress: ProgressChanged,
				Usage: ai.Usage{InputTokens: 1, OutputTokens: 1},
			}, nil
		}

		return Decision{
			Action: ActionComplete, Reason: "done", Output: ai.JSON(`{"ok":true}`),
			Usage: ai.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	})

	execution, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	require.Equal(t, StatusReady, execution.Status)
	assert.Equal(t, PhaseWork, execution.Phase)
	assert.Equal(t, ai.JSON(`null`), execution.ControllerState)

	execution, err = engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, execution.Status)
	assert.Equal(t, "done", execution.Reason)
	assert.Equal(t, 2, execution.Accounting.Attempts)
	assert.Equal(t, 2, execution.Accounting.Turns)
	assert.Equal(t, 14, execution.Accounting.Tokens())
	require.NotNil(t, execution.LastAttempt)
	require.NotNil(t, execution.LastAttempt.Decision)
	assert.Equal(t, ai.Usage{InputTokens: 1, OutputTokens: 1}, execution.LastAttempt.Decision.Usage)
	assert.Equal(t, []string{`"first"`, `"second"`}, inputs)

	_, err = engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	assert.ErrorIs(t, err, ErrTerminal)
}

func TestDecisionUsageLimitPreventsAnotherWork(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{MaxAttempts: -1, MaxTokens: 6})

	var workCalls atomic.Int64

	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		workCalls.Add(1)

		return WorkResult{
			Usage: ai.Usage{InputTokens: 2, OutputTokens: 2}, Progress: ProgressChanged,
		}, nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{
			Action: ActionContinue, Progress: ProgressChanged,
			Usage: ai.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	})

	limited, err := engine.Advance(
		t.Context(), execution.ID, execution.Revision, handlers(worker, controller),
	)
	require.NoError(t, err)
	assert.Equal(t, StatusLimited, limited.Status)
	assert.Equal(t, "maximum tokens reached", limited.Reason)
	assert.Equal(t, 6, limited.Accounting.Tokens())
	assert.Equal(t, int64(1), workCalls.Load())
}

func TestInvalidDecisionUsageInterruptsDecision(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{
			Action: ActionComplete, Usage: ai.Usage{InputTokens: -1},
		}, nil
	})

	interrupted, err := engine.Advance(
		t.Context(), execution.ID, execution.Revision, handlers(worker, controller),
	)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, StatusInterrupted, interrupted.Status)
	assert.Equal(t, PhaseDecision, interrupted.Phase)
	assert.Equal(t, 5, interrupted.Accounting.Tokens())
	require.NotNil(t, interrupted.CurrentAttempt)
	require.NotNil(t, interrupted.CurrentAttempt.Work)
	assert.Nil(t, interrupted.CurrentAttempt.Decision)
}

func TestControllerRetryKeepsAttemptAndDoesNotRerunWork(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	var workCalls atomic.Int64

	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		workCalls.Add(1)

		return changedWork(`{"evidence":true}`), nil
	})
	controllerFailure := errors.New("evaluator unavailable")
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{}, controllerFailure
	})

	interrupted, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.ErrorIs(t, err, controllerFailure)
	require.Equal(t, StatusInterrupted, interrupted.Status)
	require.Equal(t, PhaseDecision, interrupted.Phase)
	require.NotNil(t, interrupted.CurrentAttempt)
	require.NotNil(t, interrupted.CurrentAttempt.Work)
	attemptID := interrupted.CurrentAttempt.ID

	retry, err := engine.RetryDecision(t.Context(), interrupted.ID, interrupted.Revision, "retry evaluator")
	require.NoError(t, err)
	require.Equal(t, attemptID, retry.CurrentAttempt.ID)

	complete := controllerFunc(func(_ context.Context, request DecisionRequest) (Decision, error) {
		assert.Equal(t, attemptID, request.AttemptID)

		return Decision{Action: ActionComplete}, nil
	})
	finished, err := engine.Advance(t.Context(), retry.ID, retry.Revision, handlers(worker, complete))
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, finished.Status)
	assert.Equal(t, int64(1), workCalls.Load())
	assert.Equal(t, 1, finished.Accounting.Attempts)
}

func TestWorkerFailureRequiresExplicitNewAttempt(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	workerFailure := errors.New("worker transport lost")
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return WorkResult{Turns: 1, Usage: ai.Usage{InputTokens: 4}, Progress: ProgressUnknown}, workerFailure
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		t.Fatal("controller must not run after Worker failure")

		return Decision{}, nil
	})

	interrupted, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers(worker, controller))
	require.ErrorIs(t, err, workerFailure)
	require.Equal(t, StatusInterrupted, interrupted.Status)
	oldAttempt := interrupted.CurrentAttempt.ID
	assert.Equal(t, 1, interrupted.Accounting.Turns)

	_, err = engine.Advance(t.Context(), interrupted.ID, interrupted.Revision, handlers(worker, controller))
	require.ErrorIs(t, err, ErrRetryRequired)

	retry, err := engine.RetryWork(t.Context(), interrupted.ID, interrupted.Revision, "reconciled target")
	require.NoError(t, err)
	assert.Nil(t, retry.CurrentAttempt)
	require.NotNil(t, retry.LastAttempt)
	assert.Equal(t, oldAttempt, retry.LastAttempt.ID)
}

func TestDriveStopsBeforeAttemptTwentySix(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	var calls atomic.Int64

	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		calls.Add(1)

		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionContinue, Progress: ProgressChanged}, nil
	})

	result, err := engine.Drive(t.Context(), execution.ID, execution.Revision, handlers(worker, controller), DriveOptions{MaxAdvances: 30})
	require.NoError(t, err)
	assert.Equal(t, YieldTerminal, result.Yield)
	assert.Equal(t, StatusLimited, result.Execution.Status)
	assert.Equal(t, 25, result.Execution.Accounting.Attempts)
	assert.Equal(t, int64(25), calls.Load())
}

func TestDriveStopsOnNoProgressAndGate(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
		return WorkResult{Progress: ProgressUnchanged}, nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionContinue}, nil
	})

	result, err := engine.Drive(t.Context(), execution.ID, execution.Revision, handlers(worker, controller), DriveOptions{MaxAdvances: 5})
	require.NoError(t, err)
	assert.Equal(t, YieldNoProgress, result.Yield)
	assert.Equal(t, 1, result.Advances)

	second, err := engine.Create(t.Context(), CreateRequest{
		ID: "execution-2", Target: Target{Kind: "session", ID: "session-2"},
		Worker: testWorkerRef, Controller: testControllerRef,
	})
	require.NoError(t, err)
	result, err = engine.Drive(t.Context(), second.ID, second.Revision, handlers(worker, controller), DriveOptions{
		MaxAdvances: 1,
		Gate:        func(context.Context, Execution) (bool, error) { return false, nil },
	})
	require.NoError(t, err)
	assert.Equal(t, YieldGate, result.Yield)
	assert.Zero(t, result.Advances)
}

func TestAdvanceAdmissionIsPerExecution(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	first := createTestExecution(t, engine, Limits{})
	second, err := engine.Create(t.Context(), CreateRequest{
		ID: "execution-2", Target: Target{Kind: "session", ID: "session-2"},
		Worker: testWorkerRef, Controller: testControllerRef,
	})
	require.NoError(t, err)

	started := make(chan ID, 2)
	release := make(chan struct{})
	worker := workerFunc(func(_ context.Context, request WorkRequest) (WorkResult, error) {
		started <- request.ExecutionID

		<-release

		return changedWork(`{}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionComplete}, nil
	})

	type outcome struct {
		execution Execution
		err       error
	}

	results := make(chan outcome, 2)

	for _, execution := range []Execution{first, second} {
		go func(execution Execution) {
			result, runErr := engine.Advance(
				context.Background(), execution.ID, execution.Revision, handlers(worker, controller),
			)
			results <- outcome{execution: result, err: runErr}
		}(execution)
	}

	seen := map[ID]bool{<-started: true, <-started: true}
	assert.True(t, seen[first.ID])
	assert.True(t, seen[second.ID])

	_, err = engine.Advance(t.Context(), first.ID, first.Revision, handlers(worker, controller))
	require.ErrorIs(t, err, ErrBusy)
	close(release)

	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		assert.Equal(t, StatusCompleted, result.execution.Status)
	}
}

func TestEachCumulativeLimitStopsBeforeController(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits func(*mutableClock) Limits
		work   func(*mutableClock) WorkResult
		reason string
	}{
		{
			name:   "turns",
			limits: func(*mutableClock) Limits { return Limits{MaxAttempts: -1, MaxTurns: 2} },
			work: func(*mutableClock) WorkResult {
				return WorkResult{Turns: 2, Progress: ProgressChanged}
			},
			reason: "maximum turns reached",
		},
		{
			name:   "tokens",
			limits: func(*mutableClock) Limits { return Limits{MaxAttempts: -1, MaxTokens: 5} },
			work: func(*mutableClock) WorkResult {
				return WorkResult{
					Usage: ai.Usage{InputTokens: 2, OutputTokens: 3}, Progress: ProgressChanged,
				}
			},
			reason: "maximum tokens reached",
		},
		{
			name: "active duration",
			limits: func(*mutableClock) Limits {
				return Limits{MaxAttempts: -1, MaxActiveDuration: time.Second}
			},
			work: func(clock *mutableClock) WorkResult {
				clock.Add(2 * time.Second)

				return WorkResult{Progress: ProgressChanged}
			},
			reason: "maximum active duration reached",
		},
		{
			name: "deadline",
			limits: func(clock *mutableClock) Limits {
				return Limits{MaxAttempts: -1, Deadline: clock.Now().Add(time.Second)}
			},
			work: func(clock *mutableClock) WorkResult {
				clock.Add(2 * time.Second)

				return WorkResult{Progress: ProgressChanged}
			},
			reason: "deadline reached",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			engine, _, clock := newTestEngine(t)
			execution := createTestExecution(t, engine, test.limits(clock))
			worker := workerFunc(func(context.Context, WorkRequest) (WorkResult, error) {
				return test.work(clock), nil
			})
			controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
				t.Fatal("controller must not run after cumulative limit")

				return Decision{}, nil
			})

			limited, err := engine.Advance(
				t.Context(), execution.ID, execution.Revision, handlers(worker, controller),
			)
			require.NoError(t, err)
			assert.Equal(t, StatusLimited, limited.Status)
			assert.Equal(t, test.reason, limited.Reason)
		})
	}
}

func TestPauseActiveWorkRequiresRetry(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})

	started := make(chan struct{})

	var calls atomic.Int64

	worker := workerFunc(func(ctx context.Context, _ WorkRequest) (WorkResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()

			return WorkResult{Progress: ProgressUnknown}, ctx.Err()
		}

		return changedWork(`{"retried":true}`), nil
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		return Decision{Action: ActionComplete}, nil
	})

	type advanceResult struct {
		execution Execution
		err       error
	}

	advanced := make(chan advanceResult, 1)

	go func() {
		result, err := engine.Advance(context.Background(), execution.ID, execution.Revision, handlers(worker, controller))
		advanced <- advanceResult{execution: result, err: err}
	}()

	<-started

	running, err := engine.Get(t.Context(), execution.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, running.Status)

	requested, err := engine.Pause(t.Context(), running.ID, running.Revision, "user pause")
	require.NoError(t, err)
	assert.Equal(t, StatusPauseRequested, requested.Status)

	result := <-advanced
	require.ErrorIs(t, result.err, context.Canceled)
	require.Equal(t, StatusPaused, result.execution.Status)
	require.NotNil(t, result.execution.Suspension)
	assert.True(t, result.execution.Suspension.RetryRequired)

	_, err = engine.Resume(t.Context(), result.execution.ID, result.execution.Revision, "resume")
	require.ErrorIs(t, err, ErrRetryRequired)
	retry, err := engine.RetryWork(t.Context(), result.execution.ID, result.execution.Revision, "retry")
	require.NoError(t, err)
	finished, err := engine.Advance(t.Context(), retry.ID, retry.Revision, handlers(worker, controller))
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, finished.Status)
	assert.Equal(t, int64(2), calls.Load())
}

func TestHostCancellationInterruptsWithoutProductCancel(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	started := make(chan struct{})
	worker := workerFunc(func(ctx context.Context, _ WorkRequest) (WorkResult, error) {
		close(started)
		<-ctx.Done()

		return WorkResult{Progress: ProgressUnknown}, ctx.Err()
	})
	controller := controllerFunc(func(context.Context, DecisionRequest) (Decision, error) {
		t.Fatal("controller must not run after host cancellation")

		return Decision{}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		execution Execution
		err       error
	}

	completed := make(chan outcome, 1)

	go func() {
		result, err := engine.Advance(ctx, execution.ID, execution.Revision, handlers(worker, controller))
		completed <- outcome{execution: result, err: err}
	}()

	<-started
	cancel()

	result := <-completed
	require.ErrorIs(t, result.err, context.Canceled)
	assert.Equal(t, StatusInterrupted, result.execution.Status)
	assert.NotEqual(t, StatusCancelled, result.execution.Status)
}

func TestGetRecoversOrphanedRunningAttempt(t *testing.T) {
	t.Parallel()

	engine, store, clock := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	current, err := store.Load(t.Context(), execution.ID)
	require.NoError(t, err)

	running := cloneExecution(execution)
	running.Status = StatusRunning
	running.Phase = PhaseWork
	running.Accounting.Attempts = 1
	running.CurrentAttempt = &Attempt{
		ID: "attempt-orphan", Number: 1, Phase: PhaseWork, WorkStartedAt: clock.Now(),
	}
	next := engine.nextRecord(current, running, CauseStageStart, "")
	require.NoError(t, store.CompareAndSwap(t.Context(), execution.ID, execution.Revision, next))
	clock.Add(time.Minute)

	reopened, err := New(store, WithClock(clock))
	require.NoError(t, err)
	recovered, err := reopened.Get(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusInterrupted, recovered.Status)
	assert.Equal(t, PhaseWork, recovered.Phase)
	assert.Equal(t, time.Minute, recovered.Accounting.ActiveDuration)
	assert.True(t, recovered.CurrentAttempt.Interrupted)
}

func TestSnapshotsAreDefensiveCopies(t *testing.T) {
	t.Parallel()

	engine, _, _ := newTestEngine(t)
	execution := createTestExecution(t, engine, Limits{})
	execution.NextInput[0] = 'x'
	execution.Activation.Payload = ai.JSON(`{"mutated":true}`)

	loaded, err := engine.Get(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, ai.JSON(`"first"`), loaded.NextInput)
	assert.Nil(t, loaded.Activation.Payload)
}

func TestMemoryStoreCASAndPaging(t *testing.T) {
	t.Parallel()

	engine, store, _ := newTestEngine(t)
	first := createTestExecution(t, engine, Limits{})
	_, err := engine.Create(t.Context(), CreateRequest{
		ID: "execution-2", Target: Target{Kind: "session", ID: "session-2"},
		Worker: testWorkerRef, Controller: testControllerRef,
	})
	require.NoError(t, err)

	record, err := store.Load(t.Context(), first.ID)
	require.NoError(t, err)

	nextExecution := cloneExecution(record.Execution)
	nextExecution.Reason = "updated"
	next := engine.nextRecord(record, nextExecution, CauseResume, "updated")
	require.NoError(t, store.CompareAndSwap(t.Context(), first.ID, first.Revision, next))

	err = store.CompareAndSwap(t.Context(), first.ID, first.Revision, next)

	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, Revision(2), conflict.Actual)

	page, err := store.List(t.Context(), ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Executions, 1)
	assert.Equal(t, ID("execution-1"), page.Executions[0].ID)
	assert.Equal(t, "execution-1", page.NextCursor)

	page, err = store.List(t.Context(), ListOptions{Limit: 1, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page.Executions, 1)
	assert.Equal(t, ID("execution-2"), page.Executions[0].ID)
}

func TestJSONLStoreTornTailAndCommittedCorruption(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "control")
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)

	clock := ClockFunc(func() time.Time { return time.Date(2026, time.July, 19, 8, 0, 0, 0, time.UTC) })
	engine, err := New(store, WithClock(clock))
	require.NoError(t, err)
	execution := createTestExecution(t, engine, Limits{})
	path := filepath.Join(dir, string(execution.ID)+continuationFileExt)
	directoryInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), directoryInfo.Mode().Perm())

	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test path is under t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteString(`{"execution":`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	reopened, err := NewJSONLStore(dir)
	require.NoError(t, err)
	loaded, err := reopened.Load(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, execution.Revision, loaded.Execution.Revision)

	reopenedEngine, err := New(reopened, WithClock(clock))
	require.NoError(t, err)
	cancelled, err := reopenedEngine.Cancel(
		t.Context(), execution.ID, loaded.Execution.Revision, "recover after torn append",
	)
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)
	loaded, err = reopened.Load(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, cancelled.Revision, loaded.Execution.Revision)

	file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test path is under t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteString("broken\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = reopened.Load(t.Context(), execution.ID)
	assert.ErrorIs(t, err, ErrCorruptStore)
}
