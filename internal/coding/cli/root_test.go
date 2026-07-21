package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding/cli"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelpGolden(t *testing.T) {
	t.Parallel()

	output, err := execute(t, "--help")
	require.NoError(t, err)
	assert.Equal(t, readGolden(t, "help.golden"), output)
}

func TestVersion(t *testing.T) {
	t.Parallel()

	output, err := execute(t, "version")
	require.NoError(t, err)
	assert.Equal(t, "pips 1.2.3\ncommit abc123\nbuilt 2026-07-20\n", output)
}

func TestCompletionGoldenDigests(t *testing.T) {
	t.Parallel()

	want := parseDigests(readGolden(t, "completion.sha256.golden"))
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()

			output, err := execute(t, "completion", shell)
			require.NoError(t, err)
			assert.NotEmpty(t, output)

			sum := sha256.Sum256([]byte(output))
			assert.Equal(t, want[shell], hex.EncodeToString(sum[:]))
		})
	}
}

func TestNewReturnsFreshCommandTrees(t *testing.T) {
	t.Parallel()

	first, err := execute(t, "--provider", "openai", "version")
	require.NoError(t, err)
	second, err := execute(t, "version")
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestExecuteContextCancellation(t *testing.T) {
	t.Parallel()

	command := newCommand(t)
	command.SetArgs([]string{"version"})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := command.ExecuteContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCompletionRejectsUnsupportedShell(t *testing.T) {
	t.Parallel()

	_, err := execute(t, "completion", "unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported shell")
}

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	command := newCommand(t)
	buffer := new(bytes.Buffer)
	command.SetOut(buffer)
	command.SetErr(buffer)
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())

	return buffer.String(), err
}

func newCommand(t *testing.T) *cobra.Command {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	command, err := cli.New(cli.Dependencies{
		Build: cli.BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-20"},
		Paths: layout,
	})
	require.NoError(t, err)

	return command
}

func readGolden(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path) //nolint:gosec // testdata path is fixed by the test
	require.NoError(t, err)

	return string(data)
}

func parseDigests(value string) map[string]string {
	digests := make(map[string]string)

	for line := range strings.SplitSeq(strings.TrimSpace(value), "\n") {
		shell, digest, ok := strings.Cut(line, " ")
		if ok {
			digests[shell] = digest
		}
	}

	return digests
}
