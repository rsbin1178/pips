//go:build darwin || linux

package execution

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const hookProcessTerminationGrace = 250 * time.Millisecond

// configureHookCommand gives every reviewed hook its own process group. The
// reviewed shell can start descendants, so cancellation must address the
// complete group rather than only the shell leader.
func configureHookCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}

		return signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
	}
	command.WaitDelay = hookProcessTerminationGrace
}

// cleanupHookCommand also reaps a hook that deliberately backgrounds a child
// and exits successfully. It shares the Executor's bounded process-group
// cleanup routine so hook descendants cannot outlive their invocation.
func cleanupHookCommand(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}

	err := cleanupProcessGroup(command.Process.Pid, hookProcessTerminationGrace)
	if errors.Is(err, syscall.ECHILD) {
		return nil
	}

	return err
}
