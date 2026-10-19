package coding

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/scheduler"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schedulerTestClock is the scheduler tests' controllable time source. Now and
// After share one timeline, so a test advances the clock and releases every
// timer whose deadline passed instead of waiting an interval.
type schedulerTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*schedulerTestTimer
}

type schedulerTestTimer struct {
	deadline time.Time
	channel  chan time.Time
}

func newSchedulerTestClock() *schedulerTestClock {
	return &schedulerTestClock{now: time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *schedulerTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *schedulerTestClock) After(delay time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	channel := make(chan time.Time, 1)
	c.timers = append(c.timers, &schedulerTestTimer{deadline: c.now.Add(delay), channel: channel})

	return channel
}

// advance moves the clock and releases every timer whose deadline passed.
func (c *schedulerTestClock) advance(delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(delay)

	kept := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.deadline.After(c.now) {
			select {
			case timer.channel <- c.now:
			default:
			}

			continue
		}

		kept = append(kept, timer)
	}

	c.timers = kept
}

// openSchedulerRuntime opens a real Runtime whose scheduled-task loop is driven
// by the test clock.
func openSchedulerRuntime(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	clock *schedulerTestClock,
	mutate func(*config.Config),
) *Runtime {
	t.Helper()

	return openSchedulerRuntimeWithOptions(t, base, target, model,
		scheduler.Options{Now: clock.Now, After: clock.After}, mutate)
}

// openSchedulerRuntimeWithOptions is openSchedulerRuntime with the scheduler
// options spelled out, so a test can also bound the store.
func openSchedulerRuntimeWithOptions(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	schedulerOptions scheduler.Options,
	mutate func(*config.Config),
) *Runtime {
	t.Helper()

	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))

	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.Model.Provider = model.Provider()
	cfg.Model.Model = model.ModelID()

	if mutate != nil {
		mutate(&cfg)
	}

	runtime, err := Open(t.Context(), OpenOptions{
		Workspace: ws,
		Config:    cfg,
		Paths:     layout,
		Session:   target,
		Model:     model,
		Execution: ExecutionOptions{
			SandboxProbe:     func(context.Context, *execution.Executor) error { return nil },
			SchedulerOptions: schedulerOptions,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	return runtime
}

// schedulerTasks reads the runtime's durable task list.
func schedulerTasks(t *testing.T, runtime *Runtime) []scheduler.Task {
	t.Helper()

	tasks, err := runtime.scheduler.List(t.Context())
	require.NoError(t, err)

	return tasks
}

// schedulerTask reads one task by id, reporting whether it is still scheduled.
func schedulerTask(t *testing.T, runtime *Runtime, id string) (scheduler.Task, bool) {
	t.Helper()

	for _, task := range schedulerTasks(t, runtime) {
		if task.ID == id {
			return task, true
		}
	}

	return scheduler.Task{}, false
}

// waitForSchedulerFire waits until the task's recorded fire names a child.
func waitForSchedulerFire(t *testing.T, runtime *Runtime, taskID string) scheduler.Task {
	t.Helper()

	var fired scheduler.Task

	require.Eventually(t, func() bool {
		task, scheduled := schedulerTask(t, runtime, taskID)
		if !scheduled {
			return false
		}

		fired = task

		return fired.LastChildID != ""
	}, 30*time.Second, 10*time.Millisecond, "the scheduled task never fired")

	return fired
}

// waitForSchedulerChildCount waits until the runtime owns the wanted number of
// durable children.
func waitForSchedulerChildCount(t *testing.T, runtime *Runtime, count int) []subagent.Summary {
	t.Helper()

	var summaries []subagent.Summary

	require.Eventually(t, func() bool {
		values, err := runtime.ListSubagents(t.Context())
		if err != nil {
			return false
		}

		summaries = values

		return len(values) == count
	}, 30*time.Second, 10*time.Millisecond, "the runtime never owned %d children", count)

	return summaries
}

func TestRuntimeSchedulerFiresADueTaskAndKeepsThePhase(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)
	require.Equal(t, clock.Now().Add(5*time.Minute), created.NextFireAt)

	// The loop looks again two minutes after the scheduled time, so a fire-time
	// anchored next fire would differ from a phase-preserving one.
	clock.advance(7 * time.Minute)
	runtime.signalScheduler()

	fired := waitForSchedulerFire(t, runtime, created.ID)

	assert.Equal(t, clock.Now(), fired.LastFiredAt)
	assert.Equal(t, created.CreatedAt, fired.CreatedAt, "the phase anchor is unchanged")
	// The next fire is the previous scheduled time plus one interval, not the
	// fire time plus one interval.
	assert.Equal(t, created.NextFireAt.Add(5*time.Minute), fired.NextFireAt)

	// The fire started a real background general-purpose child.
	summaries := waitForSchedulerChildCount(t, runtime, 1)
	assert.Equal(t, fired.LastChildID, summaries[0].ChildSessionID)
	assert.Equal(t, schedulerAgentID, summaries[0].Identity.ID)
	assert.Equal(t, subagent.DeliveryBackground, summaries[0].Delivery)
}

func TestRuntimeSchedulerSkipsAFireWhileThePreviousChildRunsAndFiresOnceItEnds(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()

	var (
		started     = make(chan struct{})
		released    = make(chan struct{})
		startOnce   sync.Once
		releaseOnce sync.Once
	)

	model := &messagingTestModel{onRequest: func(ai.Request) *ai.Response {
		startOnce.Do(func() { close(started) })
		<-released

		return runtimeTextResponse("the child answer")
	}}
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{}, model, clock, nil)
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	fired := waitForSchedulerFire(t, runtime, created.ID)
	firstChild := fired.LastChildID

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the fired child never reached its held turn")
	}

	// The next interval comes due while the first iteration still runs. The
	// loop's own decision is driven synchronously here so the skip is asserted
	// without a timing window.
	clock.advance(5 * time.Minute)
	runtime.fireSchedulerDueTasks(t.Context())

	held, scheduled := schedulerTask(t, runtime, created.ID)
	require.True(t, scheduled)
	assert.Equal(t, firstChild, held.LastChildID, "the fire is skipped while its child runs")
	assert.Equal(t, fired.LastFiredAt, held.LastFiredAt)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	assert.Len(t, summaries, 1, "no second child starts while the first runs")

	// The skipped fire happens once the first iteration ends.
	release()

	require.Eventually(t, func() bool {
		task, ok := schedulerTask(t, runtime, created.ID)
		return ok && task.LastChildID != "" && task.LastChildID != firstChild
	}, 30*time.Second, 10*time.Millisecond, "the skipped fire never happened")

	after, _ := schedulerTask(t, runtime, created.ID)
	assert.Equal(t, clock.Now(), after.LastFiredAt)
	assert.Equal(t, created.NextFireAt.Add(10*time.Minute), after.NextFireAt,
		"the deferred fire keeps the schedule's phase")
}

func TestRuntimeSchedulerFiresAnOverdueTaskOnceNotOncePerMissedInterval(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	// One hundred intervals pass before the loop looks again.
	clock.advance(100 * 5 * time.Minute)
	runtime.signalScheduler()

	fired := waitForSchedulerFire(t, runtime, created.ID)
	assert.Equal(t, clock.Now(), fired.LastFiredAt)
	assert.Equal(t, created.NextFireAt.Add(100*5*time.Minute), fired.NextFireAt,
		"the next fire is the next future occurrence, not a replay")

	// One fire, not one per missed interval.
	waitForSchedulerChildCount(t, runtime, 1)
}

func TestRuntimeSchedulerRemovesANonRecurringTaskDurablyBeforeTheFire(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "run once", IntervalSeconds: 300, Recurring: false,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	// The removal is the fire's record: the task is gone before its child's
	// completion is ever reported.
	require.Eventually(t, func() bool {
		_, scheduled := schedulerTask(t, runtime, created.ID)

		return !scheduled
	}, 30*time.Second, 10*time.Millisecond, "the one-shot task was never removed")

	summaries := waitForSchedulerChildCount(t, runtime, 1)
	assert.NotEmpty(t, summaries[0].ChildSessionID)

	// The removal is durable: a second store over the same directory sees none.
	reopened, err := scheduler.NewStoreAt(
		runtime.handle.StateDir(), runtime.handle.Metadata().ID, scheduler.Options{},
	)
	require.NoError(t, err)
	require.NoError(t, reopened.Load(t.Context()))

	tasks, err := reopened.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, tasks)
}

func TestRuntimeSchedulerLeavesTheTaskScheduledWhenTheSpawnIsRefused(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, func(cfg *config.Config) {
			// The fired type is disabled, so the spawn is refused before it
			// starts.
			cfg.Subagent.Toggle = map[string]bool{schedulerAgentID: false}
		})

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	// The refusal advances the schedule to its next future occurrence, and the
	// task stays scheduled with nothing reported.
	require.Eventually(t, func() bool {
		task, scheduled := schedulerTask(t, runtime, created.ID)

		return scheduled && task.NextFireAt.After(clock.Now())
	}, 30*time.Second, 10*time.Millisecond, "the refused fire never advanced the task")

	task, _ := schedulerTask(t, runtime, created.ID)
	assert.True(t, task.LastFiredAt.IsZero(), "a refused fire did not happen")
	assert.Empty(t, task.LastChildID)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	assert.Empty(t, summaries)

	pending, err := runtime.notifications.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending, "a refused fire reports nothing")
}

// storeFileSize returns the byte size of a store's task file.
func storeFileSize(t *testing.T, store *scheduler.Store) int64 {
	t.Helper()

	info, err := os.Stat(filepath.Join(store.Dir(), "tasks.json"))
	require.NoError(t, err)

	return info.Size()
}

func TestRuntimeSchedulerAdvancesWhenTheFireCannotBeRecorded(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()

	// Measure the exact documents a store writes at this clock: one task before
	// a fire, and the same task after one. The runtime's store is bounded
	// between them, so recording a fire fails while advancing the schedule
	// (which writes no child fields) still fits.
	probe, err := scheduler.NewStoreAt(t.TempDir(), "s-probe", scheduler.Options{Now: clock.Now})
	require.NoError(t, err)

	probeTask, err := probe.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)
	beforeFire := storeFileSize(t, probe)

	_, err = probe.RecordFire(t.Context(), probeTask.ID,
		"s-0123456789abcdef0123456789abcdef", clock.Now(), clock.Now().Add(5*time.Minute))
	require.NoError(t, err)
	afterFire := storeFileSize(t, probe)
	require.Greater(t, afterFire, beforeFire, "a fire's record must exceed the task alone")

	runtime := openSchedulerRuntimeWithOptions(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{},
		scheduler.Options{Now: clock.Now, After: clock.After, MaxBytes: beforeFire}, nil)

	created, err := runtime.schedulerSurface().Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	// The fire starts a child it cannot record, so it is cancelled and the
	// schedule skips forward: the task never stays permanently due.
	require.Eventually(t, func() bool {
		task, scheduled := schedulerTask(t, runtime, created.ID)

		return scheduled && task.NextFireAt.After(clock.Now()) &&
			task.LastFiredAt.IsZero() && task.LastChildID == ""
	}, 30*time.Second, 10*time.Millisecond, "an unrecorded fire left the task permanently due")

	task, _ := schedulerTask(t, runtime, created.ID)
	assert.Equal(t, created.NextFireAt.Add(5*time.Minute), task.NextFireAt,
		"the skipped occurrence keeps the schedule's phase")
}

func TestRuntimeSchedulerLoopStopsWithTheRuntime(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	require.NoError(t, runtime.Close(t.Context()))

	// The loop's goroutine is gone, so nothing can fire afterwards.
	select {
	case <-runtime.schedulerDone:
	default:
		require.FailNow(t, "the scheduler loop did not stop with the runtime")
	}

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	task, scheduled := schedulerTask(t, runtime, created.ID)
	require.True(t, scheduled)
	assert.True(t, task.LastFiredAt.IsZero(), "the stopped loop fired a task")

	runtime.publisher.mu.Lock()
	projections := len(runtime.children)
	runtime.publisher.mu.Unlock()
	assert.Zero(t, projections, "the stopped loop started no child")
}

func TestRuntimeSchedulerReloadsADurableTaskAndDropsASessionOnlyOne(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(
		t, base, SessionTarget{RetainEmpty: true}, &messagingTestModel{}, clock, nil,
	)
	sessionID := runtime.handle.Metadata().ID

	durable, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "durable", IntervalSeconds: 300, Recurring: true, Durable: true,
	})
	require.NoError(t, err)

	_, err = runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "session only", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	require.NoError(t, runtime.Close(t.Context()))

	reopenedClock := newSchedulerTestClock()
	reopened := openSchedulerRuntime(
		t, base, SessionTarget{ID: sessionID}, &messagingTestModel{}, reopenedClock, nil,
	)

	tasks := schedulerTasks(t, reopened)
	require.Len(t, tasks, 1)
	assert.Equal(t, durable.ID, tasks[0].ID)

	reopenedClock.advance(5 * time.Minute)
	reopened.signalScheduler()

	fired := waitForSchedulerFire(t, reopened, durable.ID)
	assert.Equal(t, durable.ID, fired.ID)

	summaries := waitForSchedulerChildCount(t, reopened, 1)
	assert.Equal(t, fired.LastChildID, summaries[0].ChildSessionID)
}

func TestRuntimeSchedulerDeliversTheFiredChildsCompletion(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	model := &messagingTestModel{}
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{}, model, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	fired := waitForSchedulerFire(t, runtime, created.ID)

	// The completion rides the ordinary durable notification path: the child's
	// own requests are excluded, so a parent request naming the child is the
	// delivered completion.
	require.Eventually(t, func() bool {
		for _, request := range model.Requests() {
			if isChildSystemText(requestSystemText(request)) {
				continue
			}

			if requestContainsText(request, fired.LastChildID) {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond, "the fired child's completion never reached the model")

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.PendingMessages()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond, "the delivered completion was never acknowledged")
}

func TestRuntimeSchedulerFramesTheFiredChildsPrompt(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	model := &messagingTestModel{}
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{}, model, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check the deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	waitForSchedulerFire(t, runtime, created.ID)

	// The iteration knows what it is: the task id, the schedule, and the task's
	// own text, in that frame.
	require.Eventually(t, func() bool {
		for _, request := range model.childRequests() {
			if requestContainsText(request, created.ID) &&
				requestContainsText(request, "every 5 minutes") &&
				requestContainsText(request, "check the deploy") {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond, "the fired child's prompt was never framed")
}

// assertSchedulerSignal fails unless the runtime has a pending wake for the
// firing loop. The signal is sent synchronously by a mutation, so a
// non-blocking receive is exact.
func assertSchedulerSignal(t *testing.T, runtime *Runtime) {
	t.Helper()

	select {
	case <-runtime.schedulerSignal:
	default:
		require.FailNow(t, "the mutation did not wake the firing loop")
	}
}

func TestRuntimeSchedulerSurfaceWakesTheLoopOnMutation(t *testing.T) {
	t.Parallel()

	store, err := scheduler.NewStoreAt(t.TempDir(), "s-scheduler-surface", scheduler.Options{})
	require.NoError(t, err)

	// The loop is parked on the signal channel, so a mutation must leave a wake
	// there rather than waiting for an unrelated one.
	runtime := &Runtime{scheduler: store, schedulerSignal: make(chan struct{}, 1)}
	surface := runtime.schedulerSurface()
	require.NotNil(t, surface)

	created, err := surface.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)
	assertSchedulerSignal(t, runtime)

	interval := int64(600)
	_, err = surface.Update(t.Context(), created.ID, scheduler.UpdateRequest{IntervalSeconds: &interval})
	require.NoError(t, err)
	assertSchedulerSignal(t, runtime)

	removed, err := surface.Delete(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, removed)
	assertSchedulerSignal(t, runtime)

	// A read is not a change, so it never wakes the loop.
	_, err = surface.List(t.Context())
	require.NoError(t, err)

	select {
	case <-runtime.schedulerSignal:
		require.FailNow(t, "a read woke the firing loop")
	default:
	}
}

func TestRuntimeSchedulerFiredChildIsAnOrdinaryBackgroundChild(t *testing.T) {
	t.Parallel()

	clock := newSchedulerTestClock()
	runtime := openSchedulerRuntime(t, t.TempDir(), SessionTarget{},
		&messagingTestModel{}, clock, nil)

	created, err := runtime.scheduler.Create(t.Context(), scheduler.CreateRequest{
		Prompt: "check deploy", IntervalSeconds: 300, Recurring: true,
	})
	require.NoError(t, err)

	clock.advance(5 * time.Minute)
	runtime.signalScheduler()

	fired := waitForSchedulerFire(t, runtime, created.ID)
	childID := fired.LastChildID

	// A fire while the session is idle still opens the child's own projection.
	runtime.publisher.mu.Lock()
	_, projected := runtime.children[childID]
	runtime.publisher.mu.Unlock()
	assert.True(t, projected, "the fired child was never projected")

	// The recorded id resolves through the task-output read.
	require.Eventually(t, func() bool {
		records, outcome := runtime.readBackgroundTasks(
			t.Context(), []string{childID}, tools.TaskOutputModePoll, 0,
		)

		return outcome == background.WaitOutcomeCompleted && len(records) == 1 &&
			records[0].Found && records[0].Status == background.StatusCompleted
	}, 30*time.Second, 10*time.Millisecond, "the recorded child id never resolved")

	// It resolves through the blocking wait too, and through the one stop path.
	// A child's own session lock is briefly held around its terminal
	// transition, so a resolution retries rather than failing on that window.
	require.Eventually(t, func() bool {
		records, _ := runtime.readBackgroundTasks(
			t.Context(), []string{childID}, tools.TaskOutputModeWaitAny, time.Second,
		)

		return len(records) == 1 && records[0].Found
	}, 30*time.Second, 10*time.Millisecond, "the recorded child id never resolved through a wait")

	require.Eventually(t, func() bool {
		outcome, subject, err := runtime.KillBackgroundTask(
			t.Context(), childID, background.KillModel,
		)

		return err == nil && outcome == background.OutcomeAlreadyExited &&
			subject == background.SubjectSubagent
	}, 30*time.Second, 10*time.Millisecond, "the recorded child id never resolved through the stop path")

	// The child is an ordinary durable background child of this session.
	summaries := waitForSchedulerChildCount(t, runtime, 1)
	assert.Equal(t, childID, summaries[0].ChildSessionID)
	assert.Equal(t, subagent.DeliveryBackground, summaries[0].Delivery)
	assert.Equal(t, schedulerAgentID, summaries[0].Identity.ID)
}
