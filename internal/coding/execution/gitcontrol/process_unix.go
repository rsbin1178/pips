//go:build darwin || linux

package gitcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const processTerminationGrace = 250 * time.Millisecond

//nolint:gocyclo // Process start, bounded I/O, cancellation, and reap paths stay in one lifecycle.
func runProcess(
	ctx context.Context,
	executable string,
	arguments, environment []string,
	input []byte,
	outputLimit int64,
	timeout time.Duration,
) (commandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, executable, arguments...) //nolint:gosec // Runner pins the executable identity and constructs typed fixed argv.
	// CommandContext satisfies ownership propagation; cancellation itself is
	// handled below for the complete process group rather than only the leader.
	command.Cancel = func() error { return nil }
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return commandResult{}, fmt.Errorf("create stdout pipe: %w", err)
	}

	stderr, err := command.StderrPipe()
	if err != nil {
		return commandResult{}, fmt.Errorf("create stderr pipe: %w", err)
	}

	var stdin io.WriteCloser
	if len(input) != 0 {
		stdin, err = command.StdinPipe()
		if err != nil {
			return commandResult{}, fmt.Errorf("create stdin pipe: %w", err)
		}
	}

	if err := command.Start(); err != nil {
		return commandResult{}, fmt.Errorf("start Git: %w", err)
	}

	trigger := make(chan error, 1)

	var (
		total                      atomic.Int64
		stdoutBuffer, stderrBuffer bytes.Buffer
		readers                    sync.WaitGroup
	)

	readers.Add(2)
	go readBounded(stdout, &stdoutBuffer, &total, outputLimit, trigger, &readers)
	go readBounded(stderr, &stderrBuffer, &total, outputLimit, trigger, &readers)

	readersDone := make(chan struct{})

	go func() {
		readers.Wait()
		close(readersDone)
	}()

	stdinDone := make(chan error, 1)
	if stdin == nil {
		stdinDone <- nil
	} else {
		go func() {
			_, copyErr := io.Copy(stdin, bytes.NewReader(input))
			stdinDone <- errors.Join(copyErr, stdin.Close())
		}()
	}

	var waitErr, runErr error
	select {
	case <-readersDone:
		// StdoutPipe and StderrPipe require all reads to finish before Wait.
		waitErr = command.Wait()

		if err := runCtx.Err(); err != nil {
			runErr = err
		}
	case runErr = <-trigger:
		waitDone := make(chan error, 1)
		go func() { waitDone <- command.Wait() }()

		waitErr = terminateProcessGroup(command.Process.Pid, waitDone)
	case <-runCtx.Done():
		runErr = runCtx.Err()

		waitDone := make(chan error, 1)
		go func() { waitDone <- command.Wait() }()

		waitErr = terminateProcessGroup(command.Process.Pid, waitDone)
	}

	<-readersDone

	stdinErr := <-stdinDone
	result := commandResult{stdout: stdoutBuffer.Bytes(), stderr: stderrBuffer.Bytes()}

	if runErr == nil {
		select {
		case runErr = <-trigger:
		default:
		}
	}

	if runErr != nil {
		return result, errors.Join(runErr, stdinErr)
	}

	if stdinErr != nil && !errors.Is(stdinErr, os.ErrClosed) && !errors.Is(stdinErr, syscall.EPIPE) {
		return result, stdinErr
	}

	if waitErr == nil {
		return result, nil
	}

	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		return result, &processExitError{Code: exit.ExitCode(), Stderr: cleanCommandError(result.stderr)}
	}

	return result, waitErr
}

func readBounded(
	reader io.Reader,
	buffer *bytes.Buffer,
	total *atomic.Int64,
	limit int64,
	trigger chan<- error,
	done *sync.WaitGroup,
) {
	defer done.Done()

	chunk := make([]byte, 16<<10)
	for {
		count, err := reader.Read(chunk)
		if count > 0 {
			before := total.Add(int64(count)) - int64(count)

			remaining := max(limit-before, 0)
			if remaining > 0 {
				_, _ = buffer.Write(chunk[:min(int64(count), remaining)])
			}

			if before+int64(count) > limit {
				notify(trigger, ErrLimit)
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				notify(trigger, err)
			}

			return
		}
	}
}

func notify(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}

func terminateProcessGroup(pid int, waitDone <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	timer := time.NewTimer(processTerminationGrace)
	defer timer.Stop()

	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)

		return <-waitDone
	}
}
