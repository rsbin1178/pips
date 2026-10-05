package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noEnvironment(string) (string, bool) { return "", false }

func TestOperatorTextSaverDefaultsToTheHomeRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workspace := t.TempDir()
	saver := operatorTextSaver(root, workspace, noEnvironment)

	path, err := saver(t.Context(), tui.TextSaveRequest{
		Kind: tui.TextKindCopy, Content: "copied text",
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, defaultCopyFileName), path)

	data, err := os.ReadFile(path) //nolint:gosec // The test controls the temporary root.
	require.NoError(t, err)
	assert.Equal(t, "copied text", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"an exported conversation must not be readable by other users")

	exportPath, err := saver(t.Context(), tui.TextSaveRequest{
		Kind: tui.TextKindExport, Content: "document",
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, defaultExportFileName), exportPath)
}

func TestOperatorTextSaverHonorsTheCopyFileOverride(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	override := filepath.Join(t.TempDir(), "shared", "copy.txt")
	lookup := func(name string) (string, bool) {
		if name == copyFileEnv {
			return override, true
		}

		return "", false
	}

	path, err := operatorTextSaver(root, t.TempDir(), lookup)(t.Context(), tui.TextSaveRequest{
		Kind: tui.TextKindCopy, Content: "override",
	})
	require.NoError(t, err)
	assert.Equal(t, override, path)

	data, err := os.ReadFile(override) //nolint:gosec // The test controls the temporary override.
	require.NoError(t, err)
	assert.Equal(t, "override", string(data))
}

func TestOperatorTextSaverResolvesRelativePathsInTheWorkspace(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	saver := operatorTextSaver(t.TempDir(), workspace, noEnvironment)

	path, err := saver(t.Context(), tui.TextSaveRequest{
		Kind: tui.TextKindExport, Path: filepath.Join("notes", "session.md"), Content: "doc",
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(workspace, "notes", "session.md"), path)
	assert.FileExists(t, path)
}

func TestOperatorTextSaverRejectsADirectoryTarget(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	directory := filepath.Join(workspace, "notes")
	require.NoError(t, os.MkdirAll(directory, 0o750))

	_, err := operatorTextSaver(t.TempDir(), workspace, noEnvironment)(t.Context(), tui.TextSaveRequest{
		Kind: tui.TextKindExport, Path: "notes", Content: "doc",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory")
}

func TestOperatorTextSaverRejectsAnUnknownKind(t *testing.T) {
	t.Parallel()

	_, err := operatorTextSaver(t.TempDir(), t.TempDir(), noEnvironment)(t.Context(), tui.TextSaveRequest{
		Kind: "unknown", Content: "doc",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown text kind")
}

func TestOperatorTextSaverReplacesThePreviousFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	saver := operatorTextSaver(root, t.TempDir(), noEnvironment)

	first, err := saver(t.Context(), tui.TextSaveRequest{Kind: tui.TextKindCopy, Content: "first"})
	require.NoError(t, err)
	second, err := saver(t.Context(), tui.TextSaveRequest{Kind: tui.TextKindCopy, Content: "second"})
	require.NoError(t, err)
	require.Equal(t, first, second)

	data, err := os.ReadFile(first) //nolint:gosec // The test controls the temporary root.
	require.NoError(t, err)
	assert.Equal(t, "second", string(data))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the staging file must not survive a successful write")
}

func TestOperatorTextSaverStopsOnACanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	root := t.TempDir()
	_, err := operatorTextSaver(root, t.TempDir(), noEnvironment)(ctx, tui.TextSaveRequest{
		Kind: tui.TextKindExport, Content: "doc",
	})
	require.ErrorIs(t, err, context.Canceled)

	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

// TestInteractiveWiresTheTextSaver pins that the interactive command actually
// hands the TUI a saver whose defaults follow the Pips home directory.
func TestInteractiveWiresTheTextSaver(t *testing.T) {
	t.Parallel()

	userRoot := t.TempDir()
	layout, err := paths.New(userRoot)
	require.NoError(t, err)

	workspaceRoot := t.TempDir()
	require.NoError(t, os.WriteFile(layout.ConfigFile(), []byte(`
[providers.openai.models."test-model"]
default = true
`), 0o600))

	controller := &stubInteractiveController{}
	called := false
	command, err := New(Dependencies{
		Paths:      layout,
		WorkingDir: func() (string, error) { return workspaceRoot, nil },
		LookupEnv: func(name string) (string, bool) {
			if name == "API_KEY" {
				return "test-key", true
			}

			return "", false
		},
		Terminal: func(_ io.Reader, _ io.Writer) (bool, bool) { return true, true },
		OpenControl: func(_ context.Context, _ coding.OpenOptions) (tui.Controller, error) {
			return controller, nil
		},
		RunTUI: func(ctx context.Context, options tui.Options) error {
			_, bootstrapErr := options.Bootstrap(ctx, false)
			require.NoError(t, bootstrapErr)
			require.NotNil(t, options.SaveText)

			path, saveErr := options.SaveText(ctx, tui.TextSaveRequest{
				Kind: tui.TextKindCopy, Content: "REPLY",
			})
			require.NoError(t, saveErr)
			assert.Equal(t, filepath.Join(userRoot, defaultCopyFileName), path)

			called = true

			return nil
		},
	})
	require.NoError(t, err)
	command.SetIn(bytes.NewBuffer(nil))
	command.SetOut(new(bytes.Buffer))

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.True(t, called)

	data, err := os.ReadFile( //nolint:gosec // The test controls the temporary user root.
		filepath.Join(userRoot, defaultCopyFileName),
	)
	require.NoError(t, err)
	assert.Contains(t, string(data), "REPLY")
}
