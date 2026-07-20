//go:build darwin || linux

package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerReturnsExitOutputEnvironmentAndStdin(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	spec := fixture.operationSpec(`printf '%s' "$HOME"; read line; printf ':%s' "$line"; printf err >&2; exit 7`)
	spec.Env = []EnvVar{{Name: "HOME", Value: "/sandbox-home"}}
	spec.Stdin = []byte("value\n")
	operation, authorization := fixture.fullAccessOperation(t, spec)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())

	var (
		chunksMu sync.Mutex
		chunks   []OutputChunk
	)

	result, err := executor.Execute(t.Context(), operation, authorization, SinkFunc(func(_ context.Context, chunk OutputChunk) error {
		chunksMu.Lock()
		defer chunksMu.Unlock()

		chunks = append(chunks, chunk)
		chunk.Data[0] = 'X'

		return nil
	}))
	require.NoError(t, err)
	assert.Equal(t, StatusExited, result.Status)
	assert.Equal(t, 7, result.ExitCode)
	assert.Equal(t, []byte("/sandbox-home:value"), result.Stdout.Head())
	assert.Equal(t, []byte("err"), result.Stderr.Head())
	assert.Equal(t, ".", result.CWD)
	assert.NotEmpty(t, chunks)
}

func TestRunnerClassifiesSignalTimeoutAndOutputLimit(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())

	tests := []struct {
		name       string
		script     string
		mutate     func(*OperationSpec)
		wantStatus Status
		wantError  error
		wantSignal string
	}{
		{
			name:       "signal",
			script:     "kill -TERM $$",
			wantStatus: StatusSignaled,
			wantSignal: "term",
		},
		{
			name:   "timeout",
			script: "/bin/sleep 10",
			mutate: func(spec *OperationSpec) {
				spec.Timeout = 40 * time.Millisecond
			},
			wantStatus: StatusTimedOut,
			wantError:  context.DeadlineExceeded,
		},
		{
			name:   "output limit",
			script: `i=0; while [ "$i" -lt 10000 ]; do printf 1234567890; i=$((i+1)); done`,
			mutate: func(spec *OperationSpec) {
				spec.Output.CaptureBytes = 64
				spec.Output.MaxBytes = 1024
				spec.Output.ChunkBytes = 128
			},
			wantStatus: StatusOutputLimit,
			wantError:  ErrOutputLimit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec := fixture.operationSpec(test.script)
			if test.mutate != nil {
				test.mutate(&spec)
			}

			operation, authorization := fixture.fullAccessOperation(t, spec)

			result, err := executor.Execute(t.Context(), operation, authorization, nil)
			if test.wantError == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.wantError)
			}

			assert.Equal(t, test.wantStatus, result.Status)
			assert.Equal(t, test.wantSignal, result.Signal)

			if test.wantStatus == StatusOutputLimit {
				assert.Greater(t, result.Stdout.TotalBytes(), spec.Output.MaxBytes)
				assert.True(t, result.Stdout.Truncated())
			}
		})
	}
}

func TestRunnerStopsOnSinkFailureAndCallerCancellation(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())
	spec := fixture.operationSpec("printf hello; /bin/sleep 10")
	operation, authorization := fixture.fullAccessOperation(t, spec)
	sinkErr := errors.New("sink stopped")
	result, err := executor.Execute(t.Context(), operation, authorization, SinkFunc(func(context.Context, OutputChunk) error {
		return sinkErr
	}))
	require.ErrorIs(t, err, sinkErr)
	assert.Equal(t, StatusCanceled, result.Status)

	operation, authorization = fixture.fullAccessOperation(t, fixture.operationSpec("printf must-not-run"))
	plan, err := executor.Plan(t.Context(), operation, authorization)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	result, err = executor.Run(canceled, plan, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, StatusUnknown, result.Status)

	spec = fixture.operationSpec("printf ready; /bin/sleep 10")
	operation, authorization = fixture.fullAccessOperation(t, spec)
	running, stop := context.WithCancel(t.Context())
	result, err = executor.Execute(running, operation, authorization, SinkFunc(func(context.Context, OutputChunk) error {
		stop()

		return nil
	}))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, StatusCanceled, result.Status)
}

func TestRunnerPipeFailureDoesNotStartProcess(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)

	operation, authorization := fixture.fullAccessOperation(t, fixture.operationSpec("printf must-not-run"))
	for failureCall := 1; failureCall <= 3; failureCall++ {
		t.Run(strconv.Itoa(failureCall), func(t *testing.T) {
			pipeErr := errors.New("pipe failed")

			var (
				commands  atomic.Int32
				pipeCalls int
			)

			deps := systemRunnerDependencies()
			systemPipe := deps.pipe
			deps.pipe = func() (*os.File, *os.File, error) {
				pipeCalls++
				if pipeCalls == failureCall {
					return nil, nil, pipeErr
				}

				return systemPipe()
			}
			originalCommand := deps.command
			deps.command = func(name string, args ...string) *exec.Cmd {
				commands.Add(1)

				return originalCommand(name, args...)
			}
			executor := fixture.executor(t, unavailableBackend{}, deps)

			result, err := executor.Execute(t.Context(), operation, authorization, nil)
			require.ErrorIs(t, err, pipeErr)
			assert.Equal(t, StatusUnknown, result.Status)
			assert.Zero(t, commands.Load())
		})
	}
}

func TestRunnerClassifiesStartFailure(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.fullAccessOperation(t, fixture.operationSpec("printf never"))
	deps := systemRunnerDependencies()
	deps.command = func(string, ...string) *exec.Cmd { return exec.CommandContext(t.Context(), "/path/does/not/exist") }
	executor := fixture.executor(t, unavailableBackend{}, deps)

	result, err := executor.Execute(t.Context(), operation, authorization, nil)
	require.ErrorIs(t, err, ErrStart)
	assert.Equal(t, StatusUnknown, result.Status)
}

func TestProcessWaitErrorDistinguishesExitFromInfrastructure(t *testing.T) {
	t.Parallel()

	waitErr := errors.New("wait failed")
	require.ErrorIs(t, processWaitError(waitErr), waitErr)
	assert.NoError(t, processWaitError(nil))
}

func TestRunnerCleansOrdinaryBackgroundDescendants(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	operation, authorization := fixture.fullAccessOperation(
		t,
		fixture.operationSpec("/bin/sleep 10 & printf '%s' $!"),
	)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())

	result, err := executor.Execute(t.Context(), operation, authorization, nil)
	require.NoError(t, err)
	assert.Equal(t, StatusExited, result.Status)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(result.Stdout.Head())))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}

func TestRunnerDropsProgressWithoutDroppingCapture(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	spec := fixture.operationSpec(`printf 'abcdefghijklmnopqrstuvwxyz'`)
	spec.Output.CaptureBytes = 10
	spec.Output.ChunkBytes = 1
	spec.Output.QueueDepth = 1
	operation, authorization := fixture.fullAccessOperation(t, spec)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())

	result, err := executor.Execute(t.Context(), operation, authorization, SinkFunc(func(ctx context.Context, _ OutputChunk) error {
		select {
		case <-time.After(2 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	require.NoError(t, err)
	assert.Equal(t, []byte("abcde"), result.Stdout.Head())
	assert.Equal(t, []byte("vwxyz"), result.Stdout.Tail())
	assert.EqualValues(t, 26, result.Stdout.TotalBytes())
	assert.True(t, result.Stdout.Truncated())
	assert.NotContains(t, string(result.Stdout.Head()), fixture.workspace.Root())
}

func TestRunnerDoesNotLeakFileDescriptorsOrGoroutines(t *testing.T) {
	// Process-wide descriptor counting must run before parallel tests are released.
	t.Setenv("PIPS_FD_LEAK_TEST", "1")

	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())
	operation, authorization := fixture.fullAccessOperation(t, fixture.operationSpec("printf ok"))
	baselineFDs := countOpenFileDescriptors(t)
	baselineGoroutines := runtime.NumGoroutine()

	for range 20 {
		result, err := executor.Execute(t.Context(), operation, authorization, nil)
		require.NoError(t, err)
		assert.Equal(t, StatusExited, result.Status)
	}

	require.Eventually(t, func() bool {
		return countOpenFileDescriptors(t) <= baselineFDs+1 && runtime.NumGoroutine() <= baselineGoroutines+2
	}, time.Second, 10*time.Millisecond)
}

func countOpenFileDescriptors(t *testing.T) int {
	t.Helper()

	directory, err := os.Open("/dev/fd")
	require.NoError(t, err)

	entries, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	require.NoError(t, errors.Join(readErr, closeErr))

	return len(entries)
}
