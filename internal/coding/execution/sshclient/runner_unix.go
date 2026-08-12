//go:build darwin || linux

package sshclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	term "github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/rsbin1178/pips/internal/coding/clipboard"
	"github.com/rsbin1178/pips/internal/coding/imagebridge"
	"golang.org/x/sys/unix"
)

const (
	proxyPollMilliseconds = 100
	processStopGrace      = 500 * time.Millisecond
	uploadTimeout         = 10 * time.Second
	frameLifetime         = 15 * time.Second
)

type runnerDependencies struct {
	executable executableIdentity
	command    func(context.Context, string, ...string) *exec.Cmd
	now        func() time.Time
}

type uploadOutcome uint8

const (
	uploadAccepted uploadOutcome = iota + 1
	uploadFailed
)

type uploadResult struct {
	outcome uploadOutcome
}

// Run starts one fixed OpenSSH PTY proxy and owns terminal restoration.
func Run(ctx context.Context, options Options) error {
	executable, err := resolveSSHExecutable()
	if err != nil {
		return err
	}

	return run(ctx, options, runnerDependencies{
		executable: executable,
		command:    exec.CommandContext,
		now:        time.Now,
	})
}

//nolint:gocyclo // Terminal, child, upload worker, and cleanup share one strict ownership lifecycle.
func run(ctx context.Context, options Options, deps runnerDependencies) (returnErr error) {
	if err := validateRunOptions(ctx, options, deps); err != nil {
		return err
	}

	input, output, err := terminalFiles(options)
	if err != nil {
		return err
	}

	endpoint, err := newControlEndpoint()
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, endpoint.close()) }()

	arguments, err := primaryArguments(options.Request, endpoint.path)
	if err != nil {
		return err
	}

	if err := deps.executable.validate(); err != nil {
		return err
	}

	width, height, err := terminalSize(output)
	if err != nil {
		return err
	}

	state, err := term.MakeRaw(input.Fd())
	if err != nil {
		return fmt.Errorf("%w: enter raw mode", errors.Join(ErrTerminal, err))
	}

	restored := false

	restore := func() error {
		if restored {
			return nil
		}

		restored = true

		if err := term.Restore(input.Fd(), state); err != nil {
			return fmt.Errorf("%w: restore terminal", errors.Join(ErrTerminal, err))
		}

		return nil
	}
	defer func() { returnErr = errors.Join(returnErr, restore()) }()

	command := deps.command(ctx, deps.executable.path, arguments...)
	if command == nil {
		return fmt.Errorf("%w: command constructor returned nil", ErrUnavailable)
	}

	command.Cancel = func() error { return nil }
	command.Env = sshEnvironment(options.Environment)

	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: height, Cols: width})
	if err != nil {
		return fmt.Errorf("%w: start system OpenSSH", errors.Join(ErrUnavailable, err))
	}
	defer func() { returnErr = errors.Join(returnErr, terminal.Close()) }()

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	uploadCtx, cancelUploads := context.WithCancel(ctx)
	uploadRequests := make(chan struct{}, 1)
	uploadResults := make(chan uploadResult, 1)
	workerDone := startUploadWorker(
		uploadCtx,
		options,
		deps,
		endpoint.path,
		uploadRequests,
		uploadResults,
	)

	defer func() {
		cancelUploads()
		<-workerDone
	}()

	resize := make(chan os.Signal, 1)

	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)

	waitErr, proxyErr := proxyTerminal(
		ctx,
		input,
		output,
		options.ErrorOutput,
		terminal,
		command.Process.Pid,
		waitDone,
		resize,
		uploadRequests,
		uploadResults,
		endpoint,
	)
	if proxyErr != nil {
		return proxyErr
	}

	if waitErr == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return &ExitError{Code: exitErr.ExitCode()}
	}

	return fmt.Errorf("%w: wait for system OpenSSH", errors.Join(ErrUnavailable, waitErr))
}

func validateRunOptions(ctx context.Context, options Options, deps runnerDependencies) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ValidateRequest(options.Request); err != nil {
		return err
	}

	if options.Input == nil || options.Output == nil || options.ErrorOutput == nil ||
		deps.command == nil || deps.now == nil || deps.executable.path == "" {
		return fmt.Errorf("%w: incomplete runner options", ErrInvalid)
	}

	return nil
}

func terminalFiles(options Options) (*os.File, *os.File, error) {
	input, inputOK := options.Input.(*os.File)
	output, outputOK := options.Output.(*os.File)

	if !inputOK || !outputOK || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
		return nil, nil, fmt.Errorf("%w: stdin and stdout must be terminals", ErrTerminal)
	}

	return input, output, nil
}

func terminalSize(output *os.File) (uint16, uint16, error) {
	width, height, err := term.GetSize(output.Fd())
	if err != nil || width <= 0 || height <= 0 || width > math.MaxUint16 || height > math.MaxUint16 {
		return 0, 0, fmt.Errorf("%w: read terminal size", errors.Join(ErrTerminal, err))
	}

	return uint16(width), uint16(height), nil
}

func startUploadWorker(
	ctx context.Context,
	options Options,
	deps runnerDependencies,
	controlPath string,
	requests <-chan struct{},
	results chan<- uploadResult,
) <-chan struct{} {
	done := make(chan struct{})

	reader := options.Clipboard
	if reader == nil {
		reader = clipboard.New()
	}

	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return
			case <-requests:
				outcome := uploadOne(
					ctx,
					options.Request,
					options.Environment,
					reader,
					deps,
					controlPath,
				)

				select {
				case results <- uploadResult{outcome: outcome}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return done
}

func uploadOne(
	ctx context.Context,
	request Request,
	environment []string,
	reader ImageClipboard,
	deps runnerDependencies,
	controlPath string,
) uploadOutcome {
	uploadCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	if err := validateUploadEndpoint(controlPath); err != nil {
		return uploadFailed
	}

	data, err := reader.ReadImage(uploadCtx)
	if err != nil {
		return uploadFailed
	}

	image, err := attachment.NormalizeImageContext(uploadCtx, "clipboard.png", data)
	if err != nil {
		return uploadFailed
	}

	arguments, err := uploadArguments(request, controlPath)
	if err != nil || deps.executable.validate() != nil {
		return uploadFailed
	}

	var frame bytes.Buffer

	err = imagebridge.Encode(&frame, imagebridge.Frame{
		Image: image, Deadline: deps.now().Add(frameLifetime),
	})
	if err != nil {
		return uploadFailed
	}

	command := deps.command(uploadCtx, deps.executable.path, arguments...)
	if command == nil {
		return uploadFailed
	}

	command.Env = sshEnvironment(environment)
	command.Stdin = &frame
	command.Stdout = io.Discard
	command.Stderr = io.Discard

	if err := command.Run(); err != nil {
		return uploadFailed
	}

	return uploadAccepted
}

//nolint:gocyclo // Poll, child, resize, and upload events share one serialized terminal writer.
func proxyTerminal(
	ctx context.Context,
	input, output *os.File,
	errorOutput io.Writer,
	terminal *os.File,
	pid int,
	waitDone <-chan error,
	resize <-chan os.Signal,
	uploadRequests chan<- struct{},
	uploadResults <-chan uploadResult,
	endpoint *controlEndpoint,
) (error, error) {
	inputFD, terminalFD := input.Fd(), terminal.Fd()
	if inputFD > math.MaxInt32 || terminalFD > math.MaxInt32 {
		return terminateProcess(pid, waitDone), fmt.Errorf("%w: file descriptor out of range", ErrTerminal)
	}

	pollDescriptors := []unix.PollFd{
		{Fd: int32(inputFD), Events: unix.POLLIN},
		{Fd: int32(terminalFD), Events: unix.POLLIN},
	}

	filter := &InputFilter{}
	buffer := make([]byte, 32<<10)
	inputOpen := true
	terminalOpen := true
	childExited := false

	var (
		waitErr error
		err     error
	)

	for {
		if err := ctx.Err(); err != nil {
			if !childExited {
				waitErr = terminateProcess(pid, waitDone)
			}

			return waitErr, err
		}

		select {
		case waitErr = <-waitDone:
			childExited = true
		case <-resize:
			if err := resizeTerminal(output, terminal); err != nil {
				return terminateIfRunning(pid, waitDone, childExited, waitErr), err
			}
		case result := <-uploadResults:
			if result.outcome == uploadFailed {
				if err := writeAll(errorOutput, []byte("\r\nPips: image upload failed\r\n")); err != nil {
					return terminateIfRunning(pid, waitDone, childExited, waitErr), err
				}
			}
		default:
		}

		if err := endpoint.capture(); err != nil {
			return terminateIfRunning(pid, waitDone, childExited, waitErr), err
		}

		if childExited && !terminalOpen {
			return waitErr, nil
		}

		pollDescriptors[0].Events = 0
		if inputOpen {
			pollDescriptors[0].Events = unix.POLLIN
		}

		pollDescriptors[0].Revents = 0
		pollDescriptors[1].Revents = 0

		if _, err := unix.Poll(pollDescriptors, proxyPollMilliseconds); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			return terminateIfRunning(pid, waitDone, childExited, waitErr),
				fmt.Errorf("%w: poll terminal", errors.Join(ErrTerminal, err))
		}

		if inputOpen && descriptorReady(pollDescriptors[0]) {
			inputOpen, err = proxyInput(input, terminal, errorOutput, filter, buffer, uploadRequests)
			if err != nil && !childExited {
				return terminateIfRunning(pid, waitDone, childExited, waitErr), err
			}
		}

		if terminalOpen && descriptorReady(pollDescriptors[1]) {
			terminalOpen, err = proxyOutput(
				terminal,
				output,
				buffer,
				pollDescriptors[1].Revents&unix.POLLHUP != 0,
			)
			if err != nil && !childExited {
				return terminateProcess(pid, waitDone), err
			}
		}
	}
}

func descriptorReady(descriptor unix.PollFd) bool {
	return descriptor.Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0
}

func proxyInput(
	input, terminal *os.File,
	errorOutput io.Writer,
	filter *InputFilter,
	buffer []byte,
	uploadRequests chan<- struct{},
) (bool, error) {
	count, readErr := input.Read(buffer)
	if count > 0 {
		forward, requests := filter.Push(buffer[:count])
		if err := writeAll(terminal, forward); err != nil {
			return true, err
		}

		if err := queueUploads(uploadRequests, errorOutput, requests); err != nil {
			return true, err
		}
	}

	if readErr == nil {
		return true, nil
	}

	forward, _ := filter.Flush()

	return false, writeAll(terminal, forward)
}

func queueUploads(requests chan<- struct{}, errorOutput io.Writer, count int) error {
	for range count {
		select {
		case requests <- struct{}{}:
		default:
			if err := writeAll(errorOutput, []byte("\r\nPips: image upload already pending\r\n")); err != nil {
				return err
			}
		}
	}

	return nil
}

func proxyOutput(terminal, output *os.File, buffer []byte, hungUp bool) (bool, error) {
	count, readErr := terminal.Read(buffer)
	if count > 0 {
		if err := writeAll(output, buffer[:count]); err != nil {
			return true, err
		}
	}

	if readErr == nil {
		return count > 0 || !hungUp, nil
	}

	if errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.EIO) {
		return false, nil
	}

	return false, fmt.Errorf(
		"%w: read OpenSSH terminal",
		errors.Join(ErrTerminal, readErr),
	)
}

func resizeTerminal(output, terminal *os.File) error {
	width, height, err := terminalSize(output)
	if err != nil {
		return err
	}

	if err := pty.Setsize(terminal, &pty.Winsize{Rows: height, Cols: width}); err != nil {
		return fmt.Errorf("%w: resize OpenSSH terminal", errors.Join(ErrTerminal, err))
	}

	return nil
}

func terminateIfRunning(pid int, waitDone <-chan error, exited bool, waitErr error) error {
	if exited {
		return waitErr
	}

	return terminateProcess(pid, waitDone)
}

func terminateProcess(pid int, waitDone <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	timer := time.NewTimer(processStopGrace)
	defer timer.Stop()

	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)

		return <-waitDone
	}
}

func writeAll(writer io.Writer, value []byte) error {
	if len(value) == 0 {
		return nil
	}

	if _, err := io.Copy(writer, bytes.NewReader(value)); err != nil {
		return fmt.Errorf("%w: proxy terminal output", errors.Join(ErrTerminal, err))
	}

	return nil
}

func sshEnvironment(environment []string) []string {
	byName := make(map[string]string, len(environment))

	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || !allowedSSHEnvironmentName(name) {
			continue
		}

		byName[name] = entry
	}

	allowed := make([]string, 0, len(byName))
	for _, entry := range byName {
		allowed = append(allowed, entry)
	}

	slices.Sort(allowed)

	return allowed
}

func allowedSSHEnvironmentName(name string) bool {
	switch name {
	case "COLORTERM", "DISPLAY", "HOME", "LANG", "LANGUAGE", "LOGNAME", "PATH", "SHELL",
		"SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "SSH_AUTH_SOCK", "TERM", "USER", "WAYLAND_DISPLAY",
		"XAUTHORITY", "XDG_RUNTIME_DIR":
		return true
	default:
		return len(name) > 3 && name[:3] == "LC_"
	}
}
