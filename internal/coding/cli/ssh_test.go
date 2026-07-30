package cli

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/execution/sshclient"
	"github.com/rsbin/pips/internal/coding/imagebridge"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSHCommandBuildsValidatedRequest(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	var received sshclient.Options

	command, err := New(Dependencies{
		Build: cliTestBuild(),
		Paths: layout,
		Terminal: func(io.Reader, io.Writer) (bool, bool) {
			return true, true
		},
		RunSSH: func(_ context.Context, options sshclient.Options) error {
			received = options

			return nil
		},
		Environment: []string{"TERM=xterm-256color", "API_KEY=must-not-be-in-argv"},
	})
	require.NoError(t, err)

	input := bytes.NewBuffer(nil)
	output := new(bytes.Buffer)
	errorOutput := new(bytes.Buffer)

	command.SetIn(input)
	command.SetOut(output)
	command.SetErr(errorOutput)
	command.SetArgs([]string{"ssh", "deploy@example.internal", "--workspace", "/srv/project"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Equal(t, "deploy@example.internal", received.Request.Destination)
	assert.Equal(t, "/srv/project", received.Request.Workspace)
	assert.Equal(t, "1.2.3", received.Request.Version)
	require.NoError(t, imagebridge.ValidateNonce(received.Request.Nonce))
	assert.Same(t, input, received.Input)
	assert.Same(t, output, received.Output)
	assert.Same(t, errorOutput, received.ErrorOutput)
}

func TestSSHCommandRejectsIrrelevantFlagsAndNonTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		terminal TerminalDetector
	}{
		{
			name: "remote config flag",
			args: []string{"ssh", "host", "--model", "openai/gpt"},
			terminal: func(io.Reader, io.Writer) (bool, bool) {
				return true, true
			},
		},
		{
			name: "non terminal",
			args: []string{"ssh", "host"},
			terminal: func(io.Reader, io.Writer) (bool, bool) {
				return false, true
			},
		},
		{
			name: "hostile destination",
			args: []string{"ssh", "-oProxyCommand=bad"},
			terminal: func(io.Reader, io.Writer) (bool, bool) {
				return true, true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			layout, err := paths.New(t.TempDir())
			require.NoError(t, err)

			started := false

			command, err := New(Dependencies{
				Build:    cliTestBuild(),
				Paths:    layout,
				Terminal: test.terminal,
				RunSSH: func(context.Context, sshclient.Options) error {
					started = true

					return nil
				},
			})
			require.NoError(t, err)
			command.SetArgs(test.args)

			err = command.ExecuteContext(t.Context())
			require.Error(t, err)
			assert.False(t, started)
		})
	}
}

//nolint:paralleltest // The production bridge uses one UID-scoped endpoint directory.
func TestBridgeUploadDeliversOneImageWithoutOutput(t *testing.T) {
	nonce, err := imagebridge.NewNonce()
	require.NoError(t, err)

	server, err := imagebridge.Listen(t.Context(), nonce)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	versionToken, err := sshclient.EncodeVersion("1.2.3")
	require.NoError(t, err)

	image := cliBridgeImage(t)

	var encoded bytes.Buffer
	require.NoError(t, imagebridge.Encode(&encoded, imagebridge.Frame{
		Image: image, Deadline: time.Now().Add(10 * time.Second),
	}))

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	command, err := New(Dependencies{Build: cliTestBuild(), Paths: layout})
	require.NoError(t, err)

	output := new(bytes.Buffer)

	command.SetIn(&encoded)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"__bridge-upload",
		"--nonce", nonce,
		"--version-token", versionToken,
	})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Empty(t, output.String())

	delivered, err := server.Receive(t.Context())
	require.NoError(t, err)
	assert.True(t, image.Equal(delivered))
}

func TestBridgeSessionRequiresExactVersionBeforeTUI(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	nonce, err := imagebridge.NewNonce()
	require.NoError(t, err)

	versionToken, err := sshclient.EncodeVersion("different")
	require.NoError(t, err)

	workspaceToken, err := sshclient.EncodeWorkspace(t.TempDir())
	require.NoError(t, err)

	started := false

	command, err := New(Dependencies{
		Build: cliTestBuild(),
		Paths: layout,
		RunTUI: func(context.Context, tui.Options) error {
			started = true

			return nil
		},
	})
	require.NoError(t, err)
	command.SetArgs([]string{
		"__bridge-session",
		"--nonce", nonce,
		"--version-token", versionToken,
		"--workspace-token", workspaceToken,
	})

	err = command.ExecuteContext(t.Context())
	require.ErrorIs(t, err, sshclient.ErrInvalid)
	assert.False(t, started)
}

func TestExitCodePreservesRemoteStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 23, ExitCode(&sshclient.ExitError{Code: 23}))
	assert.Equal(t, ExitFailure, ExitCode(&sshclient.ExitError{Code: 0}))
	assert.Equal(t, ExitFailure, ExitCode(&sshclient.ExitError{Code: 300}))
}

func cliTestBuild() BuildInfo {
	return BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-30"}
}

func cliBridgeImage(t *testing.T) attachment.Image {
	t.Helper()

	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	source.SetRGBA(0, 0, color.RGBA{R: 64, G: 128, B: 255, A: 255})

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, source))

	normalized, err := attachment.NormalizeImage("clipboard.png", encoded.Bytes())
	require.NoError(t, err)

	return normalized
}
