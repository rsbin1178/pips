package background

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitRecorder captures one registry's terminal completions, so a test can read
// the waiter flag the delivery path consults.
type waitRecorder struct {
	mu          sync.Mutex
	completions []Completion
}

func (*waitRecorder) BackgroundStarted(Snapshot) {}
func (*waitRecorder) BackgroundEvent(Event)      {}

func (r *waitRecorder) BackgroundCompleted(completion Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completions = append(r.completions, completion)
}

func (r *waitRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.completions)
}

// waitFixture is one registry plus the release channel its task blocks on, so a
// test controls exactly when the task ends.
type waitFixture struct {
	registry *Registry
	observer *waitRecorder
	snapshot Snapshot
	release  chan struct{}
}

func newWaitFixture(t *testing.T) waitFixture {
	t.Helper()

	observer := &waitRecorder{}

	registry, err := NewRegistry(Options{
		OutputDir:    t.TempDir(),
		Observer:     observer,
		KillToolName: KillTaskToolName,
		OutputBytes:  1 << 20,
		TailBytes:    1024,
	})
	require.NoError(t, err)

	release := make(chan struct{})

	start := func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		select {
		case <-release:
			return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
		case <-ctx.Done():
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		}
	}

	snapshot, err := registry.Launch(context.Background(), Request{
		Kind: KindMonitor, Command: "watch", Timeout: time.Minute, Start: start,
	})
	require.NoError(t, err)

	return waitFixture{registry: registry, observer: observer, snapshot: snapshot, release: release}
}

// TestWaitRegistersBeforeBlockingAndTakesTheResult pins the wake suppression's
// determinism: the waiter registers under the registry lock before it blocks, so
// a completion produced while it is registered sees ResultTaken, and the waiter
// marks the terminal result taken when it returns.
func TestWaitRegistersBeforeBlockingAndTakesTheResult(t *testing.T) {
	t.Parallel()

	fixture := newWaitFixture(t)

	results := make(chan []TaskOutput, 1)

	go func() {
		records, _ := fixture.registry.Wait(
			context.Background(), []string{fixture.snapshot.ID}, false, time.Minute,
		)
		results <- records
	}()

	// The waiter registers before it blocks, so the live predicate reports the
	// result taken while the task is still running: the delivery path would
	// suppress a completion rendered in this window.
	require.Eventually(t, func() bool {
		return fixture.registry.ResultTaken(fixture.snapshot.ID)
	}, 30*time.Second, 10*time.Millisecond)

	stored, ok := fixture.registry.Snapshot(fixture.snapshot.ID)
	require.True(t, ok)
	require.False(t, stored.Completed, "the registration must be visible before the task ends")

	close(fixture.release)

	records := <-results
	require.Len(t, records, 1)
	assert.Equal(t, StatusCompleted, records[0].Status)
	assert.True(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"the terminal record the wait returned is taken")

	require.Eventually(t, func() bool {
		return fixture.observer.count() == 1
	}, 30*time.Second, 10*time.Millisecond)
}

// TestWaitThatElapsesReleasesTheRegistration proves the release rule: a wait
// that ends with the task still running stops being a reason to suppress, so the
// task's own later completion still wakes the model.
func TestWaitThatElapsesReleasesTheRegistration(t *testing.T) {
	t.Parallel()

	fixture := newWaitFixture(t)

	records, outcome := fixture.registry.Wait(
		context.Background(), []string{fixture.snapshot.ID}, false, 20*time.Millisecond,
	)
	assert.Equal(t, WaitOutcomeDeadline, outcome)
	require.Len(t, records, 1)
	assert.Equal(t, StatusRunning, records[0].Status)
	assert.False(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"an elapsed wait must release its registration")

	close(fixture.release)

	require.Eventually(t, func() bool {
		return fixture.observer.count() == 1
	}, 30*time.Second, 10*time.Millisecond)

	assert.False(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"nothing took the result, so the completion stands")
}

// TestReadOfAFinishedTaskMarksTheResultTaken pins the immediate-read rule: a
// snapshot of a task that already ended is the surface that carries its result.
func TestReadOfAFinishedTaskMarksTheResultTaken(t *testing.T) {
	t.Parallel()

	fixture := newWaitFixture(t)

	close(fixture.release)

	require.Eventually(t, func() bool {
		return fixture.observer.count() == 1
	}, 30*time.Second, 10*time.Millisecond)

	assert.False(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"an unread result is not taken yet")

	records := fixture.registry.TaskOutputs([]string{fixture.snapshot.ID})
	require.Len(t, records, 1)
	assert.Equal(t, StatusCompleted, records[0].Status)
	assert.True(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"the read that returned the terminal record took it")
}

// TestWaitWithACancelledContextReportsTheCancellation pins the outcome of a wait
// whose context is already done: it never blocks, so it must not report a
// completion no task reached, and it must leave no registration behind.
func TestWaitWithACancelledContextReportsTheCancellation(t *testing.T) {
	t.Parallel()

	fixture := newWaitFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	records, outcome := fixture.registry.Wait(ctx, []string{fixture.snapshot.ID}, false, time.Minute)
	assert.Equal(t, WaitOutcomeCanceled, outcome)
	require.Len(t, records, 1)
	assert.Equal(t, StatusRunning, records[0].Status)
	assert.False(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"a wait that never blocked must not leave a registration behind")

	close(fixture.release)

	require.Eventually(t, func() bool {
		return fixture.observer.count() == 1
	}, 30*time.Second, 10*time.Millisecond)
}

// TestWaitAnyReleasesTheStillRunningRegistrations proves a wait-any releases the
// ids it left running: their own later completions are not suppressed on the
// waiter's behalf.
func TestWaitAnyReleasesTheStillRunningRegistrations(t *testing.T) {
	t.Parallel()

	observer := &waitRecorder{}

	registry, err := NewRegistry(Options{
		OutputDir:    t.TempDir(),
		Observer:     observer,
		KillToolName: KillTaskToolName,
		OutputBytes:  1 << 20,
		TailBytes:    1024,
	})
	require.NoError(t, err)

	release := make([]chan struct{}, 2)

	launch := func(index int) Snapshot {
		release[index] = make(chan struct{})
		channel := release[index]

		snapshot, launchErr := registry.Launch(context.Background(), Request{
			Kind: KindMonitor, Command: "watch", Timeout: time.Minute,
			Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
				select {
				case <-channel:
					return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
				case <-ctx.Done():
					return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
				}
			},
		})
		require.NoError(t, launchErr)

		return snapshot
	}

	first := launch(0)
	second := launch(1)

	close(release[0])

	records, outcome := registry.Wait(
		context.Background(), []string{first.ID, second.ID}, true, time.Minute,
	)
	require.Equal(t, WaitOutcomeCompleted, outcome)
	require.Len(t, records, 2)
	assert.Equal(t, StatusCompleted, records[0].Status)
	assert.Equal(t, StatusRunning, records[1].Status)

	assert.True(t, registry.ResultTaken(first.ID), "the terminal record the wait returned is taken")
	assert.False(t, registry.ResultTaken(second.ID),
		"a wait-any must release the ids it left running")

	close(release[1])

	require.Eventually(t, func() bool {
		return observer.count() == 2
	}, 30*time.Second, 10*time.Millisecond)
}

// TestWaitCountsConcurrentRegistrationsOnOneTask proves one read's release does
// not clear another read's registration: two reads can stand by for the same
// task, and the delivery path keeps seeing the result taken until the last of
// them returns.
func TestWaitCountsConcurrentRegistrationsOnOneTask(t *testing.T) {
	t.Parallel()

	fixture := newWaitFixture(t)

	standing := make(chan []TaskOutput, 1)

	go func() {
		records, _ := fixture.registry.Wait(
			context.Background(), []string{fixture.snapshot.ID}, false, time.Minute,
		)
		standing <- records
	}()

	require.Eventually(t, func() bool {
		return fixture.registry.ResultTaken(fixture.snapshot.ID)
	}, 30*time.Second, 10*time.Millisecond)

	// A second read on the same task leaves with its budget, without the result.
	records, outcome := fixture.registry.Wait(
		context.Background(), []string{fixture.snapshot.ID}, false, 50*time.Millisecond,
	)
	require.Len(t, records, 1)
	assert.Equal(t, WaitOutcomeDeadline, outcome)
	assert.Equal(t, StatusRunning, records[0].Status)

	assert.True(t, fixture.registry.ResultTaken(fixture.snapshot.ID),
		"the read that is still standing by keeps the result taken")

	close(fixture.release)

	terminal := <-standing
	require.Len(t, terminal, 1)
	assert.Equal(t, StatusCompleted, terminal[0].Status)
}
