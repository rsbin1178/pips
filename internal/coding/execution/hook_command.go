package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
)

// HookCommandOptions describe one already-reviewed Pips lifecycle hook command.
// This deliberately stays outside Executor's model-tool Sandbox path: callers
// must pass only an explicitly reviewed local command and stdin data.
type HookCommandOptions struct {
	Workspace   string
	Environment []string
	Command     string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// HookCommandExitError reports a shell command that started and returned a
// non-zero status. Callers can use ExitCode to apply their own hook protocol.
type HookCommandExitError struct {
	code  int
	cause error
}

func (e *HookCommandExitError) Error() string {
	if e == nil || e.cause == nil {
		return "coding execution: hook command exited unsuccessfully"
	}

	return e.cause.Error()
}

// Unwrap returns the underlying process error.
func (e *HookCommandExitError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// ExitCode returns the command's process exit status.
func (e *HookCommandExitError) ExitCode() int {
	if e == nil {
		return -1
	}

	return e.code
}

// RunHookCommand owns one reviewed lifecycle-hook process until it exits or
// ctx is cancelled. It must not be used for model-controlled command strings.
func RunHookCommand(ctx context.Context, options HookCommandOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.Workspace == "" || strings.TrimSpace(options.Command) == "" {
		return fmt.Errorf("%w: incomplete lifecycle hook command", ErrInvalidOperation)
	}

	// The command string is a separately trusted lifecycle-hook definition.
	// Dynamic model and session values are supplied through Stdin, never here.
	command := exec.CommandContext( //nolint:gosec // explicit reviewed hook command, not model-generated input.
		ctx,
		"/bin/sh",
		"-c",
		options.Command,
	)
	configureHookCommand(command)
	command.Dir = options.Workspace
	command.Env = slices.Clone(options.Environment)
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	runErr := command.Run()
	cleanupErr := cleanupHookCommand(command)
	if runErr != nil {
		var exited *exec.ExitError
		if errors.As(runErr, &exited) {
			return &HookCommandExitError{code: exited.ExitCode(), cause: errors.Join(runErr, cleanupErr)}
		}

		return errors.Join(runErr, cleanupErr)
	}
	if cleanupErr != nil {
		return cleanupErr
	}

	return nil
}
