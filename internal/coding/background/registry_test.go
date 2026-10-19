package background_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder captures one registry's model-visible output.
type recorder struct {
	mu          sync.Mutex
	events      []background.Event
	completions []background.Completion
}

func (r *recorder) BackgroundStarted(background.Snapshot) {}

func (r *recorder) BackgroundEvent(event background.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *recorder) BackgroundCompleted(completion background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completions = append(r.completions, completion)
}

func (r *recorder) reported() ([]background.Event, []background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]background.Event(nil), r.events...), append([]background.Completion(nil), r.completions...)
}

func (r *recorder) completed() int {
	_, completions := r.reported()

	return len(completions)
}

// waitForCompletion blocks until the recorder has seen count completions.
func waitForCompletion(t *testing.T, observer *recorder, count int) []background.Completion {
	t.Helper()

	require.Eventually(t, func() bool {
		return observer.completed() >= count
	}, 30*time.Second, 10*time.Millisecond)

	_, completions := observer.reported()

	return completions
}

type registryFixture struct {
	registry *background.Registry
	dir      string
}

func newRegistryFixture(
	t *testing.T,
	observer background.Observer,
	outputBytes, tailBytes int64,
) registryFixture {
	t.Helper()

	directory := t.TempDir()

	registry, err := background.NewRegistry(background.Options{
		OutputDir:    directory,
		Observer:     observer,
		KillToolName: background.KillTaskToolName,
		OutputBytes:  outputBytes,
		TailBytes:    tailBytes,
	})
	require.NoError(t, err)

	return registryFixture{registry: registry, dir: directory}
}

// writeStart returns a Start that writes every line in one chunk and exits,
// standing in for a script that finished on its own.
func writeStart(t *testing.T, lines ...string) background.Start {
	t.Helper()

	payload := []byte(strings.Join(lines, "\n") + "\n")

	return func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		err := sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStdout, Data: payload,
		})
		if err != nil {
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, err
		}

		return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
	}
}

// blockStart returns a Start that runs until its context ends, standing in for
// a watch that has to be stopped.
func blockStart() background.Start {
	return func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		<-ctx.Done()

		status := execution.StatusCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = execution.StatusTimedOut
		}

		return execution.Result{Status: status, ExitCode: -1}, ctx.Err()
	}
}

func launchMonitor(
	t *testing.T,
	registry *background.Registry,
	start background.Start,
	mutate func(*background.Request),
) background.Snapshot {
	t.Helper()

	request := background.Request{
		Kind: background.KindMonitor, Command: "deploy", Description: "deploy log",
		CWD: ".", Timeout: 10 * time.Minute, Start: start,
	}
	if mutate != nil {
		mutate(&request)
	}

	snapshot, err := registry.Launch(t.Context(), request)
	require.NoError(t, err)

	return snapshot
}

func TestRegistryStreamsOneBatchEventPerTickAndOneCompletion(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, "line-1", "line-2", ""), nil)
	assert.True(t, strings.HasPrefix(snapshot.ID, string(background.KindMonitor)+"-1-"))
	assert.Greater(t, len(snapshot.ID), len(string(background.KindMonitor)+"-1-"), "the id carries a unique suffix")
	assert.Equal(t, background.KindMonitor, snapshot.Kind)
	assert.Equal(t, "deploy", snapshot.Command)
	assert.Equal(t, "deploy log", snapshot.Description)
	assert.False(t, snapshot.Completed)

	completions := waitForCompletion(t, observer, 1)

	events, _ := observer.reported()
	require.Len(t, events, 1)
	assert.Equal(t, "line-1\nline-2", events[0].Text)
	assert.Equal(t, snapshot.ID, events[0].TaskID)
	assert.Equal(
		t,
		"<monitor-event description=\"deploy log\" task_id=\""+snapshot.ID+"\">\nline-1\nline-2\n</monitor-event>",
		events[0].Wrapped,
	)

	completion := completions[0]
	assert.True(t, completion.Snapshot.Completed)
	assert.Equal(t, 0, completion.Snapshot.ExitCode)
	assert.False(t, completion.Snapshot.End.IsZero())
	assert.False(t, completion.Killed)

	// The log itself is still readable by id: the completion points at the
	// task-output Tool, and the pane's own read keeps its bounded tail.
	tail, ok := fixture.registry.Tail(snapshot.ID, 1024)
	require.True(t, ok)
	assert.Equal(t, "line-1\nline-2", tail)

	// The projection is stable after the task ends.
	stored, ok := fixture.registry.Snapshot(snapshot.ID)
	require.True(t, ok)
	assert.True(t, stored.Completed)
	assert.Len(t, fixture.registry.List(), 1)
}

// TestRegistryTaskIDsAreUniqueAcrossRegistries pins the identity fix: the
// process-local counter alone restarts at 1 in every process, so the registry
// mints an id with a per-registry token and two registries never collide.
func TestRegistryTaskIDsAreUniqueAcrossRegistries(t *testing.T) {
	t.Parallel()

	first := newRegistryFixture(t, &recorder{}, 1<<20, 1024)
	second := newRegistryFixture(t, &recorder{}, 1<<20, 1024)

	one := launchMonitor(t, first.registry, writeStart(t, "line-1"), nil)
	two := launchMonitor(t, second.registry, writeStart(t, "line-1"), nil)

	assert.NotEqual(t, one.ID, two.ID, "the same sequence in two registries mints different ids")
	assert.NotEqual(
		t,
		strings.TrimPrefix(one.ID, string(background.KindMonitor)+"-1-"),
		strings.TrimPrefix(two.ID, string(background.KindMonitor)+"-1-"),
	)
	assert.True(t, strings.HasPrefix(one.ID, string(background.KindMonitor)+"-1-"))
}

func TestRegistryTailStaysBoundedForThePane(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 24)

	lines := make([]string, 0, 40)
	for index := range 40 {
		lines = append(lines, fmt.Sprintf("line-%02d", index))
	}

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, lines...), nil)
	waitForCompletion(t, observer, 1)

	tail, ok := fixture.registry.Tail(snapshot.ID, 24)
	require.True(t, ok)

	require.LessOrEqual(t, len(tail), 24)
	assert.Contains(t, tail, "line-39")
	assert.NotContains(t, tail, "line-00")
}

func TestRegistryWritesTheOutputFileInItsDirectory(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, "line-1"), nil)
	waitForCompletion(t, observer, 1)

	content, err := os.ReadFile(filepath.Join(fixture.dir, snapshot.ID+".log"))
	require.NoError(t, err)
	assert.Equal(t, "line-1\n", string(content))
}

func TestRegistryBoundsTheOutputFileIndependentlyOfTheEvent(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 64, 1024)

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, strings.Repeat("x", 4096)), nil)
	waitForCompletion(t, observer, 1)

	content, err := os.ReadFile(filepath.Join(fixture.dir, snapshot.ID+".log"))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("x", 64), string(content))

	// The pipeline only ever reads what the bounded file holds, so neither the
	// event nor the pane's tail can exceed the file bound.
	tail, ok := fixture.registry.Tail(snapshot.ID, 1024)
	require.True(t, ok)
	assert.Equal(t, strings.Repeat("x", 64), tail)
}

func TestRegistryKillVocabularyAndCompletionFlags(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, blockStart(), nil)

	assert.Equal(t, background.OutcomeNotFound, fixture.registry.Kill("missing", background.KillModel))

	// A model kill already returns its outcome in a Tool result, so the terminal
	// completion is marked as delivered and must not wake the model again.
	assert.Equal(t, background.OutcomeKilled, fixture.registry.Kill(snapshot.ID, background.KillModel))

	completions := waitForCompletion(t, observer, 1)
	assert.True(t, completions[0].Killed)
	assert.True(t, completions[0].KillResultDelivered)

	// The task is gone: a second kill reports the terminal state, not a new one.
	require.Eventually(t, func() bool {
		return fixture.registry.Kill(snapshot.ID, background.KillModel) == background.OutcomeAlreadyExited
	}, 30*time.Second, 10*time.Millisecond)
}

func TestRegistryUserKillKeepsTheDoNotRestartNotice(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, blockStart(), nil)

	assert.Equal(t, background.OutcomeKilled, fixture.registry.Kill(snapshot.ID, background.KillUser))

	completions := waitForCompletion(t, observer, 1)
	assert.True(t, completions[0].Killed)
	assert.False(t, completions[0].KillResultDelivered)
	assert.Contains(t, background.MonitorCompletionText(completions[0]), "do not restart")
}

func TestRegistryDeadlineEndsADeadlineTask(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	launchMonitor(t, fixture.registry, blockStart(), func(request *background.Request) {
		request.Timeout = 50 * time.Millisecond
	})

	completions := waitForCompletion(t, observer, 1)
	assert.False(t, completions[0].Killed)
	assert.Equal(t, -1, completions[0].Snapshot.ExitCode)
	assert.Contains(t, background.MonitorCompletionText(completions[0]), "[monitor ended: ended].")
}

func TestRegistryNoDeadlineTaskCarriesNoDeadline(t *testing.T) {
	t.Parallel()

	deadlines := make(chan bool, 2)

	start := func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		_, ok := ctx.Deadline()
		deadlines <- ok

		<-ctx.Done()

		return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
	}

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	persistent := launchMonitor(t, fixture.registry, start, func(request *background.Request) {
		request.NoDeadline = true
		request.Timeout = 0
	})

	assert.False(t, <-deadlines, "a no-deadline task's context must carry no deadline")
	assert.Equal(t, background.OutcomeKilled, fixture.registry.Kill(persistent.ID, background.KillUser))

	bounded := launchMonitor(t, fixture.registry, start, func(request *background.Request) {
		request.Timeout = time.Minute
	})

	assert.True(t, <-deadlines, "a bounded task's context must carry its deadline")
	assert.Equal(t, background.OutcomeKilled, fixture.registry.Kill(bounded.ID, background.KillUser))

	waitForCompletion(t, observer, 2)
}

func TestRegistryReapStopsEveryTaskAndRefusesNewOnes(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	first := launchMonitor(t, fixture.registry, blockStart(), nil)
	second := launchMonitor(t, fixture.registry, blockStart(), nil)

	require.NoError(t, fixture.registry.Reap(t.Context()))
	waitForCompletion(t, observer, 2)

	for _, snapshot := range []background.Snapshot{first, second} {
		stored, ok := fixture.registry.Snapshot(snapshot.ID)
		require.True(t, ok)
		assert.True(t, stored.Completed)
	}

	_, err := fixture.registry.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "deploy", Timeout: time.Minute,
		Start: writeStart(t, "late"),
	})
	require.ErrorIs(t, err, background.ErrClosed)
}

func TestRegistryLaunchValidatesItsRequest(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, nil, 1<<20, 1024)

	valid := background.Request{
		Kind: background.KindMonitor, Command: "deploy", Timeout: time.Minute,
		Start: writeStart(t, "line"),
	}

	missingStart := valid
	missingStart.Start = nil
	_, err := fixture.registry.Launch(t.Context(), missingStart)
	require.ErrorIs(t, err, background.ErrInvalid)

	missingKind := valid
	missingKind.Kind = ""
	_, err = fixture.registry.Launch(t.Context(), missingKind)
	require.ErrorIs(t, err, background.ErrInvalid)

	missingDeadline := valid
	missingDeadline.Timeout = 0
	_, err = fixture.registry.Launch(t.Context(), missingDeadline)
	require.ErrorIs(t, err, background.ErrInvalid)

	// A registered task still records its own state when no observer is set.
	snapshot, err := fixture.registry.Launch(t.Context(), valid)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		stored, ok := fixture.registry.Snapshot(snapshot.ID)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)
}

func TestNewRegistryValidatesOptions(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	for name, options := range map[string]background.Options{
		"no output directory": {OutputBytes: 1024, TailBytes: 1024},
		"no output bound":     {OutputDir: directory, TailBytes: 1024},
		"no tail bound":       {OutputDir: directory, OutputBytes: 1024},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := background.NewRegistry(options)
			require.ErrorIs(t, err, background.ErrInvalid)
		})
	}
}
