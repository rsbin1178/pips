//go:build darwin || linux

package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type processPipes struct {
	stdinChild   *os.File
	stdinParent  *os.File
	stdoutParent *os.File
	stdoutChild  *os.File
	stderrParent *os.File
	stderrChild  *os.File
}

func (e *Executor) run(ctx context.Context, plan *Plan, sink Sink) (Result, error) {
	result := emptyResult(plan.operation.cwd)
	startedAt := e.deps.now()

	runCtx, cancel := context.WithTimeout(ctx, plan.operation.timeout)
	defer cancel()

	pipes, err := openProcessPipes(e.deps)
	if err != nil {
		return result, err
	}
	defer func() { _ = pipes.closeAll() }()

	command := e.deps.command(plan.launch.executable, plan.launch.args...)
	if command == nil {
		return result, fmt.Errorf("%w: command constructor returned nil", ErrStart)
	}

	configureCommand(command, plan.launch, pipes)

	if err := command.Start(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrStart, err)
	}

	closeErr := pipes.closeChildEnds()

	collector := newOutputCollector(plan.operation.output)
	trigger := make(chan error, 1)
	chunks := make(chan OutputChunk, plan.operation.output.QueueDepth)
	readersDone := startOutputReaders(pipes, collector, plan.operation.output.ChunkBytes, chunks, trigger)
	dispatchDone := startDispatcher(runCtx, sink, chunks, trigger)
	stdinDone := startStdinWriter(pipes.stdinParent, plan.launch.stdin, trigger)

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	waitErr, runErr := e.waitForProcess(runCtx, ctx, command.Process.Pid, waitDone, trigger)
	cleanupErr := cleanupProcessGroup(command.Process.Pid, e.termGrace)
	readerErr := e.finishIO(runCtx, ctx, pipes, readersDone, dispatchDone, stdinDone, chunks, trigger, &runErr)
	closeErr = errors.Join(closeErr, readerErr, pipes.closeAll())

	result.Duration = e.deps.now().Sub(startedAt)
	result.Stdout, result.Stderr = collector.results()
	classifyProcessResult(&result, waitErr, runErr)

	var waitInfrastructureErr error
	if runErr == nil {
		waitInfrastructureErr = processWaitError(waitErr)
	}

	return result, errors.Join(runErr, waitInfrastructureErr, closeErr, cleanupErr)
}

func openProcessPipes(deps runnerDependencies) (*processPipes, error) {
	stdinChild, stdinParent, err := deps.pipe()
	if err != nil {
		return nil, fmt.Errorf("coding execution: create stdin pipe: %w", err)
	}

	pipes := &processPipes{stdinChild: stdinChild, stdinParent: stdinParent}

	stdoutParent, stdoutChild, err := deps.pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("coding execution: create stdout pipe: %w", err), pipes.closeAll())
	}

	pipes.stdoutParent, pipes.stdoutChild = stdoutParent, stdoutChild

	stderrParent, stderrChild, err := deps.pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("coding execution: create stderr pipe: %w", err), pipes.closeAll())
	}

	pipes.stderrParent, pipes.stderrChild = stderrParent, stderrChild

	return pipes, nil
}

func configureCommand(command *exec.Cmd, launch launchSpec, pipes *processPipes) {
	command.Dir = launch.cwd
	command.Env = slices.Clone(launch.environment)
	command.Stdin = pipes.stdinChild
	command.Stdout = pipes.stdoutChild
	command.Stderr = pipes.stderrChild
	command.ExtraFiles = slices.Clone(launch.extraFiles)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startOutputReaders(
	pipes *processPipes,
	collector *outputCollector,
	chunkBytes int,
	chunks chan<- OutputChunk,
	trigger chan<- error,
) <-chan struct{} {
	done := make(chan struct{})

	var readers sync.WaitGroup
	readers.Add(2)

	go readOutput(StreamStdout, pipes.stdoutParent, collector, chunkBytes, chunks, trigger, &readers)
	go readOutput(StreamStderr, pipes.stderrParent, collector, chunkBytes, chunks, trigger, &readers)
	go func() {
		readers.Wait()
		close(done)
	}()

	return done
}

func readOutput(
	stream Stream,
	reader *os.File,
	collector *outputCollector,
	chunkBytes int,
	chunks chan<- OutputChunk,
	trigger chan<- error,
	done *sync.WaitGroup,
) {
	defer done.Done()

	buffer := make([]byte, chunkBytes)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			data := bytes.Clone(buffer[:count])

			offset, exceeded := collector.write(stream, data)
			if exceeded {
				notifyTrigger(trigger, ErrOutputLimit)
			}

			select {
			case chunks <- OutputChunk{Stream: stream, Offset: offset, Data: data}:
			default:
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				notifyTrigger(trigger, fmt.Errorf("coding execution: read %s: %w", streamName(stream), err))
			}

			return
		}
	}
}

func startDispatcher(
	ctx context.Context,
	sink Sink,
	chunks <-chan OutputChunk,
	trigger chan<- error,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)

		for chunk := range chunks {
			if sink == nil {
				continue
			}

			if err := sink.WriteOutput(ctx, chunk); err != nil {
				notifyTrigger(trigger, fmt.Errorf("coding execution: output sink: %w", err))

				return
			}
		}
	}()

	return done
}

func startStdinWriter(writer *os.File, input []byte, trigger chan<- error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)

		if len(input) > 0 {
			if _, err := io.Copy(writer, bytes.NewReader(input)); err != nil && !errors.Is(err, os.ErrClosed) {
				notifyTrigger(trigger, fmt.Errorf("coding execution: write stdin: %w", err))
			}
		}

		_ = writer.Close()
	}()

	return done
}

func (e *Executor) waitForProcess(
	runCtx, callerCtx context.Context,
	pid int,
	waitDone <-chan error,
	trigger <-chan error,
) (error, error) {
	select {
	case waitErr := <-waitDone:
		return waitErr, nil
	case runErr := <-trigger:
		return terminateAndWait(pid, e.termGrace, waitDone), runErr
	case <-runCtx.Done():
		return terminateAndWait(pid, e.termGrace, waitDone), executionContextError(runCtx, callerCtx)
	}
}

func (e *Executor) finishIO(
	runCtx, callerCtx context.Context,
	pipes *processPipes,
	readersDone, dispatchDone, stdinDone <-chan struct{},
	chunks chan OutputChunk,
	trigger <-chan error,
	runErr *error,
) error {
	drainTimer := time.NewTimer(e.drainGrace)
	defer drainTimer.Stop()

	select {
	case <-readersDone:
	case err := <-trigger:
		setFirstError(runErr, err)

		_ = pipes.closeReadEnds()

		<-readersDone
	case <-runCtx.Done():
		setFirstError(runErr, executionContextError(runCtx, callerCtx))

		_ = pipes.closeReadEnds()

		<-readersDone
	case <-drainTimer.C:
		_ = pipes.closeReadEnds()

		<-readersDone
	}

	close(chunks)

	dispatchErr := waitBounded(dispatchDone, e.drainGrace, "output dispatcher")
	stdinErr := waitBounded(stdinDone, e.drainGrace, "stdin writer")

	select {
	case err := <-trigger:
		setFirstError(runErr, err)
	default:
	}

	return errors.Join(dispatchErr, stdinErr)
}

func terminateAndWait(pid int, grace time.Duration, waitDone <-chan error) error {
	_ = signalProcessGroup(pid, syscall.SIGTERM)

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case waitErr := <-waitDone:
		return waitErr
	case <-timer.C:
		_ = signalProcessGroup(pid, syscall.SIGKILL)

		return <-waitDone
	}
}

func cleanupProcessGroup(pid int, grace time.Duration) error {
	exists, err := processGroupExists(pid)
	if err != nil || !exists {
		return err
	}

	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
		return err
	}

	timer := time.NewTimer(grace)
	ticker := time.NewTicker(min(grace, 10*time.Millisecond))

	defer timer.Stop()
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			exists, checkErr := processGroupExists(pid)
			if checkErr != nil || !exists {
				return checkErr
			}
		case <-timer.C:
			return signalProcessGroup(pid, syscall.SIGKILL)
		}
	}
}

func waitBounded(done <-chan struct{}, grace time.Duration, name string) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("coding execution: %s did not stop", name)
	}
}

func classifyProcessResult(result *Result, waitErr, runErr error) {
	if runErr != nil {
		switch {
		case errors.Is(runErr, ErrOutputLimit):
			result.Status = StatusOutputLimit
		case errors.Is(runErr, context.DeadlineExceeded):
			result.Status = StatusTimedOut
		default:
			result.Status = StatusCanceled
		}

		return
	}

	var exitErr *exec.ExitError

	if waitErr == nil {
		result.Status = StatusExited
		result.ExitCode = 0

		return
	}

	if !errors.As(waitErr, &exitErr) {
		return
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		result.Status = StatusSignaled
		result.Signal = unixSignalName(status.Signal())

		return
	}

	result.Status = StatusExited
	result.ExitCode = exitErr.ExitCode()
}

func processWaitError(waitErr error) error {
	if waitErr == nil {
		return nil
	}

	if isProcessExitError(waitErr) {
		return nil
	}

	return fmt.Errorf("coding execution: process wait: %w", waitErr)
}

func isProcessExitError(err error) bool {
	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}

func executionContextError(runCtx, callerCtx context.Context) error {
	if err := callerCtx.Err(); err != nil {
		return err
	}

	return runCtx.Err()
}

func notifyTrigger(trigger chan<- error, err error) {
	select {
	case trigger <- err:
	default:
	}
}

func setFirstError(target *error, candidate error) {
	if *target == nil {
		*target = candidate
	}
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

func processGroupExists(pid int) (bool, error) {
	err := syscall.Kill(-pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}

	return err == nil, err
}

func unixSignalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGHUP:
		return "hup"
	case syscall.SIGINT:
		return "int"
	case syscall.SIGQUIT:
		return "quit"
	case syscall.SIGKILL:
		return "kill"
	case syscall.SIGTERM:
		return "term"
	default:
		return strings.ToLower(signal.String())
	}
}

func streamName(stream Stream) string {
	if stream == StreamStderr {
		return "stderr"
	}

	return "stdout"
}

func (p *processPipes) closeChildEnds() error {
	err := errors.Join(closeFile(&p.stdinChild), closeFile(&p.stdoutChild), closeFile(&p.stderrChild))

	return err
}

func (p *processPipes) closeReadEnds() error {
	return errors.Join(closeFile(&p.stdoutParent), closeFile(&p.stderrParent))
}

func (p *processPipes) closeAll() error {
	return errors.Join(
		closeFile(&p.stdinChild),
		closeFile(&p.stdinParent),
		closeFile(&p.stdoutParent),
		closeFile(&p.stdoutChild),
		closeFile(&p.stderrParent),
		closeFile(&p.stderrChild),
	)
}

func closeFile(file **os.File) error {
	if file == nil || *file == nil {
		return nil
	}

	err := (*file).Close()
	*file = nil

	if errors.Is(err, os.ErrClosed) {
		return nil
	}

	return err
}
