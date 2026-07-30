//go:build darwin || linux

//nolint:paralleltest,wsl_v5 // PTY signal tests are serial; ownership setup stays auditable.
package sshclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	term "github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/imagebridge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sshHelperMarker = "__pips_sshclient_test_helper"

func TestSSHClientPTYUploadResizeExitAndRestore(t *testing.T) {
	if runSSHClientTestHelper(t) {
		return
	}

	harness := newSSHPTYHarness(t)

	done := make(chan error, 1)
	go func() { done <- run(t.Context(), harness.options, harness.dependencies) }()

	waitSSHOutput(t, harness.output, "SIZE=80x24", 5*time.Second)

	_, err := harness.master.Write([]byte("echo-me\x16"))
	require.NoError(t, err)
	waitSSHOutput(t, harness.output, "echo-me", 5*time.Second)
	waitForFile(t, harness.uploadLog, 5*time.Second)

	require.NoError(t, pty.Setsize(harness.master, &pty.Winsize{Rows: 37, Cols: 101}))
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGWINCH))
	waitSSHOutput(t, harness.output, "SIZE=101x37", 5*time.Second)

	_, err = harness.master.Write([]byte("x"))
	require.NoError(t, err)

	select {
	case runErr := <-done:
		var exitErr *ExitError
		require.ErrorAs(t, runErr, &exitErr)
		assert.Equal(t, 7, exitErr.ExitCode())
	case <-time.After(5 * time.Second):
		t.Fatal("SSH PTY runner did not preserve remote exit")
	}

	after, err := term.GetState(harness.master.Fd())
	require.NoError(t, err)
	assert.True(t, reflect.DeepEqual(harness.before, after), "terminal mode was not restored")

	uploadedDigest, err := os.ReadFile(harness.uploadLog)
	require.NoError(t, err)

	expectedDigest := harness.image.Digest()
	assert.Equal(t, hex.EncodeToString(expectedDigest[:]), string(uploadedDigest))
}

func TestSSHClientPTYCancellationReapsAndRestores(t *testing.T) {
	if runSSHClientTestHelper(t) {
		return
	}

	harness := newSSHPTYHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- run(ctx, harness.options, harness.dependencies) }()

	waitSSHOutput(t, harness.output, "SIZE=80x24", 5*time.Second)
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("SSH PTY runner did not reap after cancellation")
	}

	after, err := term.GetState(harness.master.Fd())
	require.NoError(t, err)
	assert.True(t, reflect.DeepEqual(harness.before, after), "terminal mode was not restored")
}

func TestSSHEnvironmentExcludesLocalSecrets(t *testing.T) {
	t.Parallel()

	got := sshEnvironment([]string{
		"TERM=xterm-256color",
		"API_KEY=private",
		"PIPS_HOME=/private/config",
		"SSH_AUTH_SOCK=/run/user/1000/agent",
		"LC_ALL=C",
		"TERM=screen-256color",
		"HTTP_PROXY=http://private",
		"malformed",
	})

	assert.Equal(t, []string{
		"LC_ALL=C",
		"SSH_AUTH_SOCK=/run/user/1000/agent",
		"TERM=screen-256color",
	}, got)
}

func TestUploadRefusesMissingControlSocketBeforeClipboardAccess(t *testing.T) {
	t.Parallel()

	nonce, err := imagebridge.NewNonce()
	require.NoError(t, err)

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700)) //nolint:gosec // Private directory requires owner traversal.

	reader := new(countingClipboard)
	outcome := uploadOne(
		t.Context(),
		Request{Destination: "host", Workspace: ".", Version: "test", Nonce: nonce},
		nil,
		reader,
		runnerDependencies{},
		directory+"/master.sock",
	)

	assert.Equal(t, uploadFailed, outcome)
	assert.Zero(t, reader.calls)
}

type sshPTYHarness struct {
	master       *os.File
	before       *term.State
	output       *lockedBuffer
	uploadLog    string
	image        attachment.Image
	options      Options
	dependencies runnerDependencies
}

func newSSHPTYHarness(t *testing.T) sshPTYHarness {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)

	identity, err := inspectSSHExecutable(executable)
	require.NoError(t, err)

	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = master.Close()
		_ = slave.Close()
	})
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}))

	before, err := term.GetState(master.Fd())
	require.NoError(t, err)

	nonce, err := imagebridge.NewNonce()
	require.NoError(t, err)

	image := sshClientImage(t)
	uploadLog := t.TempDir() + "/upload.digest"
	output := new(lockedBuffer)
	go func() { _, _ = io.Copy(output, master) }()

	command := func(ctx context.Context, _ string, arguments ...string) *exec.Cmd {
		helperArguments := []string{
			"-test.run=TestSSHClientPTYUploadResizeExitAndRestore|TestSSHClientPTYCancellationReapsAndRestores",
			"--",
			sshHelperMarker,
			uploadLog,
		}
		helperArguments = append(helperArguments, arguments...)

		return exec.CommandContext(ctx, executable, helperArguments...) //nolint:gosec // Test helper executes its own fixed binary.
	}

	return sshPTYHarness{
		master: master, before: before, output: output, uploadLog: uploadLog, image: image,
		options: Options{
			Request: Request{
				Destination: "test-host",
				Workspace:   "/remote/workspace",
				Version:     "test",
				Nonce:       nonce,
			},
			Input: slave, Output: slave, ErrorOutput: slave,
			Environment: []string{"TERM=xterm-256color", "API_KEY=must-not-reach-child"},
			Clipboard:   &staticImageClipboard{data: image.Bytes()},
		},
		dependencies: runnerDependencies{executable: identity, command: command, now: time.Now},
	}
}

func runSSHClientTestHelper(t *testing.T) bool {
	t.Helper()

	marker := slices.Index(os.Args, sshHelperMarker)
	if marker < 0 {
		return false
	}

	if marker+2 >= len(os.Args) {
		os.Exit(90)
	}

	uploadLog := os.Args[marker+1]
	arguments := os.Args[marker+2:]

	if slices.Contains(arguments, "__bridge-upload") {
		os.Exit(runSSHUploadHelper(uploadLog))
	}

	os.Exit(runSSHPrimaryHelper(arguments))

	return true
}

func runSSHUploadHelper(uploadLog string) int {
	frame, err := imagebridge.Decode(context.Background(), os.Stdin, time.Now())
	if err != nil || !frame.Image.Valid() {
		return 91
	}

	digest := frame.Image.Digest()
	if err := os.WriteFile( //nolint:gosec // Test parent supplies a private temporary path.
		uploadLog,
		[]byte(hex.EncodeToString(digest[:])),
		0o600,
	); err != nil {
		return 92
	}

	return 0
}

func runSSHPrimaryHelper(arguments []string) int {
	controlPath := argumentValue(arguments, "-S")
	if controlPath == "" {
		return 93
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlPath, Net: "unix"})
	if err != nil {
		return 94
	}
	listener.SetUnlinkOnClose(true)

	if err := os.Chmod(controlPath, 0o600); err != nil { //nolint:gosec // Test parent supplies the private ControlPath.
		_ = listener.Close()

		return 95
	}
	defer func() { _ = listener.Close() }()

	state, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return 96
	}
	defer func() { _ = term.Restore(os.Stdin.Fd(), state) }()

	if !printSSHHelperSize() {
		return 97
	}

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)

	input := make(chan byte, 1)
	go func() {
		one := []byte{0}

		for {
			if _, err := io.ReadFull(os.Stdin, one); err != nil {
				return
			}

			input <- one[0]
		}
	}()

	for {
		select {
		case <-resize:
			if !printSSHHelperSize() {
				return 97
			}
		case value := <-input:
			switch value {
			case 'x':
				return 7
			case 'q':
				return 0
			default:
				_, _ = os.Stdout.Write([]byte{value})
			}
		}
	}
}

func printSSHHelperSize() bool {
	width, height, err := term.GetSize(os.Stdin.Fd())
	if err != nil {
		return false
	}

	_, _ = fmt.Fprintf(os.Stdout, "SIZE=%dx%d\n", width, height)

	return true
}

func argumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}

	return ""
}

func sshClientImage(t *testing.T) attachment.Image {
	t.Helper()

	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	source.SetRGBA(0, 0, color.RGBA{R: 16, G: 128, B: 240, A: 255})

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, source))

	normalized, err := attachment.NormalizeImage("clipboard.png", encoded.Bytes())
	require.NoError(t, err)

	return normalized
}

type staticImageClipboard struct {
	data []byte
}

type countingClipboard struct {
	calls int
}

func (clipboard *countingClipboard) ReadImage(context.Context) ([]byte, error) {
	clipboard.calls++

	return nil, nil
}

func (clipboard *staticImageClipboard) ReadImage(context.Context) ([]byte, error) {
	return bytes.Clone(clipboard.data), nil
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return buffer.buffer.Write(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return buffer.buffer.String()
}

func waitSSHOutput(t *testing.T, output *lockedBuffer, text string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("SSH helper output did not contain %q; got %q", text, output.String())
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("upload helper did not create %s", path)
}
