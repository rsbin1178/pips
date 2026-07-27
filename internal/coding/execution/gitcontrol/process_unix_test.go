//go:build darwin || linux

package gitcontrol

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunProcessStopsProcessGroupAtCombinedOutputLimit(t *testing.T) {
	t.Parallel()

	executable, err := exec.LookPath("yes")
	require.NoError(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require.NoError(t, err)

	result, err := runProcess(
		t.Context(), executable, nil, []string{localeEnvironment}, nil, 1024, 2*time.Second,
	)
	require.ErrorIs(t, err, ErrLimit)
	assert.LessOrEqual(t, len(result.stdout)+len(result.stderr), 1024)
}

func TestRunProcessTerminatesOnDeadline(t *testing.T) {
	t.Parallel()

	executable, err := exec.LookPath("sleep")
	require.NoError(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require.NoError(t, err)

	started := time.Now()
	_, err = runProcess(
		context.Background(), executable, []string{"30"}, []string{localeEnvironment},
		nil, 1024, 20*time.Millisecond,
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}

func TestRunProcessCollectsCompleteConcurrentOutput(t *testing.T) {
	t.Parallel()

	executable, err := exec.LookPath("printf")
	require.NoError(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require.NoError(t, err)

	const workers = 64

	type workerResult struct {
		output string
		err    error
	}

	results := make(chan workerResult, workers)

	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			result, runErr := runProcess(
				t.Context(), executable, []string{"identity-line\\n"},
				[]string{localeEnvironment}, nil, 1024, 2*time.Second,
			)
			results <- workerResult{output: string(result.stdout), err: runErr}
		})
	}

	group.Wait()
	close(results)

	for result := range results {
		require.NoError(t, result.err)
		assert.Equal(t, "identity-line\n", result.output)
	}
}
