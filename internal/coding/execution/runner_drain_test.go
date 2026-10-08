//go:build darwin || linux

package execution

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFinishIODrainsBeforeClosingTheReadEnds pins the ordering behind a capture that
// described reader scheduling: when the output trigger fires, the read ends stay open
// until the readers report done, so a caller receives what the process wrote rather
// than whatever had been read when the trigger landed.
func TestFinishIODrainsBeforeClosingTheReadEnds(t *testing.T) {
	t.Parallel()

	stdoutRead, stdoutWrite, err := os.Pipe()
	require.NoError(t, err)
	stderrRead, stderrWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, file := range []*os.File{stdoutRead, stdoutWrite, stderrRead, stderrWrite} {
			_ = file.Close()
		}
	})

	pipes := &processPipes{
		stdoutParent: stdoutRead,
		stdoutChild:  stdoutWrite,
		stderrParent: stderrRead,
		stderrChild:  stderrWrite,
	}

	trigger := make(chan error, 1)
	trigger <- ErrOutputLimit

	// The readers are still running. The grace is far beyond this test, so a timer
	// cannot end the drain in its place.
	readersDone := make(chan struct{})
	ioDone := make(chan struct{})
	close(ioDone)

	var runErr error

	finished := make(chan error, 1)
	executor := &Executor{drainGrace: 10 * time.Second}

	go func() {
		finished <- executor.finishIO(
			context.Background(), context.Background(), pipes,
			readersDone, ioDone, ioDone, make(chan OutputChunk), trigger, &runErr,
		)
	}()

	// Wait until the runner has taken the trigger, so the branch under test is the one
	// running, then let the readers finish.
	require.Eventually(t, func() bool { return len(trigger) == 0 },
		30*time.Second, 10*time.Millisecond, "the runner did not take the trigger")
	close(readersDone)
	require.NoError(t, <-finished)
	require.ErrorIs(t, runErr, ErrOutputLimit)

	// After the drain the read end is still ours: writing succeeds while it is open and
	// fails with a broken pipe when the trigger closed it early.
	_, writeErr := stdoutWrite.Write([]byte("late"))
	assert.NoError(t, writeErr, "the read end was closed before the readers drained")
}
