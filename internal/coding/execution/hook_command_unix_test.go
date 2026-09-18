//go:build darwin || linux

package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunHookCommandCleansUpBackgroundProcess(t *testing.T) {
	pid := runHookWithBackgroundSleep(t, t.Context())

	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 30*time.Second, 10*time.Millisecond, "background hook process %d still exists", pid)
}

func TestRunHookCommandCancelsBackgroundProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "hook-child.pid")
	done := make(chan error, 1)
	go func() {
		done <- RunHookCommand(ctx, HookCommandOptions{
			Workspace:   workspace,
			Environment: []string{"HOOK_CHILD_PID=" + pidFile},
			Command:     `sleep 30 & printf '%s' "$!" > "$HOOK_CHILD_PID"; wait`,
		})
	}()

	pid := waitForHookChildPID(t, pidFile)
	cancel()
	require.Error(t, <-done)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 30*time.Second, 10*time.Millisecond, "canceled hook process %d still exists", pid)
}

func runHookWithBackgroundSleep(t *testing.T, ctx context.Context) int {
	t.Helper()

	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "hook-child.pid")
	err := RunHookCommand(ctx, HookCommandOptions{
		Workspace:   workspace,
		Environment: []string{"HOOK_CHILD_PID=" + pidFile},
		Command:     `sleep 30 & printf '%s' "$!" > "$HOOK_CHILD_PID"`,
	})
	require.NoError(t, err)

	return waitForHookChildPID(t, pidFile)
}

func waitForHookChildPID(t *testing.T, path string) int {
	t.Helper()

	var value string
	require.Eventually(t, func() bool {
		encoded, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		value = strings.TrimSpace(string(encoded))

		return value != ""
	}, 30*time.Second, 10*time.Millisecond, "hook did not record its background process")

	pid, err := strconv.Atoi(value)
	require.NoError(t, err)
	if pid <= 0 {
		require.Failf(t, "invalid hook process ID", "pid = %d", pid)
	}

	return pid
}
