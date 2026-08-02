//go:build darwin || linux

package session

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lockHelperEnv = "PIPS_SESSION_LOCK_HELPER"

func TestSessionLockOwnerDiesWithoutStaleOwnership(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "" {
		runSessionLockHelper()
		return
	}

	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.lock")
	stdout, childOutput, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdout.Close() })
	process, err := os.StartProcess(
		os.Args[0],
		[]string{os.Args[0], "-test.run=TestSessionLockOwnerDiesWithoutStaleOwnership"},
		&os.ProcAttr{
			Env:   append(os.Environ(), lockHelperEnv+"="+path),
			Files: []*os.File{os.Stdin, childOutput, os.Stderr},
		},
	)
	require.NoError(t, err)
	require.NoError(t, childOutput.Close())

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "locked\n", ready)

	_, err = acquireSessionLock(t.Context(), path)
	var locked *LockedError
	require.ErrorAs(t, err, &locked)
	assert.Equal(t, process.Pid, locked.OwnerPID)
	assert.ErrorIs(t, err, ErrLocked)

	require.NoError(t, process.Kill())
	state, err := process.Wait()
	require.NoError(t, err)
	assert.False(t, state.Success())
	require.Eventually(t, func() bool {
		lock, lockErr := acquireSessionLock(t.Context(), path)
		if lockErr != nil {
			return false
		}
		require.NoError(t, lock.Close())

		return true
	}, 5*time.Second, 10*time.Millisecond)
	_, err = os.Stat(path)
	require.NoError(t, err, "lock pathname remains diagnostic, not authoritative")
}

func runSessionLockHelper() {
	path := os.Getenv(lockHelperEnv)
	lock, err := acquireSessionLock(context.Background(), path)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	defer func() { _ = lock.Close() }()
	_, _ = os.Stdout.WriteString("locked\n")
	select {}
}

func TestLockedErrorWithoutOwnerRemainsCompatible(t *testing.T) {
	t.Parallel()
	err := &LockedError{Path: "/private/session.lock"}
	assert.True(t, errors.Is(err, ErrLocked))
	assert.Contains(t, err.Error(), strconv.Quote(err.Path))
	assert.NotContains(t, err.Error(), "owner pid")
}
