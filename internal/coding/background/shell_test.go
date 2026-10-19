package background

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellRecorder captures one registry's events and completions, so a test can
// prove a shell task emits none while a monitor still does.
type shellRecorder struct {
	mu          sync.Mutex
	events      []Event
	completions []Completion
}

func (*shellRecorder) BackgroundStarted(Snapshot) {}

func (r *shellRecorder) BackgroundEvent(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *shellRecorder) BackgroundCompleted(completion Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completions = append(r.completions, completion)
}

func (r *shellRecorder) reported() ([]Event, []Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Event(nil), r.events...), append([]Completion(nil), r.completions...)
}

func newShellRegistry(t *testing.T, observer Observer, outputBytes, shellOutputBytes int64) *Registry {
	t.Helper()

	registry, err := NewRegistry(Options{
		OutputDir:        t.TempDir(),
		Observer:         observer,
		KillToolName:     KillTaskToolName,
		OutputBytes:      outputBytes,
		ShellOutputBytes: shellOutputBytes,
		TailBytes:        4096,
	})
	require.NoError(t, err)

	return registry
}

// writeBothStreams is a Start that writes one stdout line and one stderr line
// into the task's sink.
func writeBothStreams(stdout, stderr string) Start {
	return func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		if err := sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStdout, Data: []byte(stdout),
		}); err != nil {
			return execution.Result{}, err
		}

		if err := sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStderr, Data: []byte(stderr),
		}); err != nil {
			return execution.Result{}, err
		}

		return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
	}
}

func waitForTask(t *testing.T, registry *Registry, id string) {
	t.Helper()

	require.Eventually(t, func() bool {
		stored, ok := registry.Snapshot(id)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)
}

// TestShellTaskLogKeepsBothStreamsAndEmitsNoEvents pins the shell kind's two
// differences from a monitor: a command's stderr is part of its output, and a
// command has no event contract.
func TestShellTaskLogKeepsBothStreamsAndEmitsNoEvents(t *testing.T) {
	t.Parallel()

	observer := &shellRecorder{}
	registry := newShellRegistry(t, observer, 1<<20, 2<<20)

	snapshot, err := registry.Launch(t.Context(), Request{
		Kind: KindShell, Command: "deploy", Timeout: time.Minute,
		Start: writeBothStreams("out\n", "err\n"),
	})
	require.NoError(t, err)

	waitForTask(t, registry, snapshot.ID)

	tail, ok := registry.Tail(snapshot.ID, 4096)
	require.True(t, ok)
	assert.Contains(t, tail, "out")
	assert.Contains(t, tail, "err")

	require.Eventually(t, func() bool {
		_, completions := observer.reported()

		return len(completions) == 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := observer.reported()
	assert.Empty(t, events, "a shell task has no event contract")
	require.Len(t, completions, 1)
	assert.True(t, completions[0].Snapshot.Completed)
}

// TestMonitorTaskLogDropsStderr pins the monitor's own sink contract: a monitor
// reports the stdout lines of the command it watches, so stderr is not part of
// its log.
func TestMonitorTaskLogDropsStderr(t *testing.T) {
	t.Parallel()

	observer := &shellRecorder{}
	registry := newShellRegistry(t, observer, 1<<20, 2<<20)

	snapshot, err := registry.Launch(t.Context(), Request{
		Kind: KindMonitor, Command: "watch", Timeout: time.Minute,
		Start: writeBothStreams("out\n", "err\n"),
	})
	require.NoError(t, err)

	waitForTask(t, registry, snapshot.ID)

	tail, ok := registry.Tail(snapshot.ID, 4096)
	require.True(t, ok)
	assert.Contains(t, tail, "out")
	assert.NotContains(t, tail, "err")
}

// TestShellTaskOutputBoundIsTheShellCeiling pins the per-kind bound: a command's
// log takes the shell's own ceiling while a watch keeps the monitor cap, and an
// absent shell bound falls back to the default.
func TestShellTaskOutputBoundIsTheShellCeiling(t *testing.T) {
	t.Parallel()

	registry := newShellRegistry(t, nil, 10<<20, 16<<20)

	assert.Equal(t, int64(16<<20), registry.outputLimit(KindShell))
	assert.Equal(t, int64(10<<20), registry.outputLimit(KindMonitor))

	fallback := newShellRegistry(t, nil, 10<<20, 0)
	assert.Equal(t, int64(10<<20), fallback.outputLimit(KindShell))
}

// TestShellTaskIDCarriesTheShellPrefixAndIsUniqueAcrossRegistries pins the id
// shape every id-addressed surface relies on: the kind prefix, the counter, and
// the token that separates two registries.
func TestShellTaskIDCarriesTheShellPrefixAndIsUniqueAcrossRegistries(t *testing.T) {
	t.Parallel()

	launch := func(registry *Registry) Snapshot {
		t.Helper()

		snapshot, err := registry.Launch(t.Context(), Request{
			Kind: KindShell, Command: "true", Timeout: time.Minute,
			Start: func(context.Context, execution.Sink) (execution.Result, error) {
				return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
			},
		})
		require.NoError(t, err)

		return snapshot
	}

	one := launch(newShellRegistry(t, nil, 1<<20, 2<<20))
	two := launch(newShellRegistry(t, nil, 1<<20, 2<<20))

	assert.True(t, strings.HasPrefix(one.ID, string(KindShell)+"-1-"))
	assert.True(t, strings.HasPrefix(two.ID, string(KindShell)+"-1-"))
	assert.NotEqual(t, one.ID, two.ID, "the token separates two registries' ids")
}

// TestRegistryResultReturnsAFinishedTaskOutcome pins the positive-block path's
// source of truth: the task keeps the executor's own result and the error beside
// it, so that path renders exactly what the synchronous path would, and an
// unknown id reports nothing.
func TestRegistryResultReturnsAFinishedTaskOutcome(t *testing.T) {
	t.Parallel()

	registry := newShellRegistry(t, nil, 1<<20, 2<<20)
	want := execution.Result{Status: execution.StatusTimedOut, ExitCode: -1}
	wantErr := context.DeadlineExceeded
	release := make(chan struct{})

	snapshot, err := registry.Launch(t.Context(), Request{
		Kind: KindShell, Command: "false", Timeout: time.Minute,
		Start: func(context.Context, execution.Sink) (execution.Result, error) {
			<-release

			return want, wantErr
		},
	})
	require.NoError(t, err)

	_, ok, _ := registry.Result(snapshot.ID)
	assert.False(t, ok, "a task that is still running has no result to render")

	close(release)

	waitForTask(t, registry, snapshot.ID)

	got, ok, gotErr := registry.Result(snapshot.ID)
	require.True(t, ok)
	assert.Equal(t, want, got)
	require.ErrorIs(t, gotErr, wantErr)

	_, ok, _ = registry.Result("shell-unknown")
	assert.False(t, ok)
}
