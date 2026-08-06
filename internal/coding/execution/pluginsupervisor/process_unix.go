//go:build darwin || linux

//nolint:wsl_v5 // Process-group termination is one platform lifecycle boundary.
package pluginsupervisor

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type unixProcessController struct{}

func newProcessController() processController { return &unixProcessController{} }

func (c *unixProcessController) configure(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func (c *unixProcessController) attach(*exec.Cmd) error { return nil }
func (c *unixProcessController) close() error           { return nil }

func (c *unixProcessController) terminate(
	ctx context.Context,
	command *exec.Cmd,
	_ <-chan struct{},
) (processTermination, error) {
	if command == nil || command.Process == nil {
		return processTermination{treeQuiesced: true}, nil
	}
	pid := command.Process.Pid
	if pid <= 0 {
		return processTermination{}, ErrCleanupUncertain
	}

	groupAlive, err := processGroupAlive(pid)
	if err != nil {
		return processTermination{}, err
	}
	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
		return processTermination{}, err
	}
	quiesced, err := waitForProcessGroup(ctx, pid, 250*time.Millisecond)
	if err != nil {
		return processTermination{}, errors.Join(ErrCleanupUncertain, err)
	}
	if quiesced {
		return processTermination{forced: groupAlive, treeQuiesced: true}, nil
	}

	if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil {
		return processTermination{forced: true}, err
	}
	quiesced, err = waitForProcessGroup(ctx, pid, 0)
	if err != nil {
		return processTermination{forced: true}, errors.Join(ErrCleanupUncertain, err)
	}
	if !quiesced {
		return processTermination{forced: true}, ErrCleanupUncertain
	}
	return processTermination{forced: true, treeQuiesced: true}, nil
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func waitForProcessGroup(ctx context.Context, pid int, grace time.Duration) (bool, error) {
	var deadline <-chan time.Time
	if grace > 0 {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		alive, err := processGroupAlive(pid)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline:
			return false, nil
		case <-ticker.C:
		}
	}
}

func processGroupAlive(pid int) (bool, error) {
	err := syscall.Kill(-pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}
