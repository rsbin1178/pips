//nolint:wsl_v5 // Fixture setup and its assertions stay adjacent.
package session_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/require"
)

// snapshotContext is a strict-decodable payload used by the JSON tests.
type snapshotContext struct {
	Model string   `json:"model"`
	Tools []string `json:"tools"`
}

func openSnapshot(t *testing.T) *session.ModelSnapshot {
	t.Helper()

	snap, err := session.OpenModelSnapshot(filepath.Join(t.TempDir(), "model"))
	require.NoError(t, err)

	return snap
}

func TestModelSnapshotSystemPromptRoundTrip(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "model")
	snap, err := session.OpenModelSnapshot(dir)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(dir), snap.Dir())

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	const prompt = "You are a helpful coding agent.\n"
	require.NoError(t, snap.WriteSystemPrompt(prompt))

	text, ok, err := snap.ReadSystemPrompt()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, prompt, text)

	fileInfo, err := os.Stat(filepath.Join(dir, session.SystemPromptFileName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}

func TestModelSnapshotReadSystemPromptMissing(t *testing.T) {
	t.Parallel()

	text, ok, err := openSnapshot(t).ReadSystemPrompt()
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, text)
}

func TestModelSnapshotSystemPromptOversizedWrite(t *testing.T) {
	t.Parallel()

	snap := openSnapshot(t)
	err := snap.WriteSystemPrompt(strings.Repeat("x", (4<<20)+1))
	require.ErrorIs(t, err, session.ErrSnapshot)
}

func TestModelSnapshotSystemPromptOversizedRead(t *testing.T) {
	t.Parallel()

	snap := openSnapshot(t)
	path := filepath.Join(snap.Dir(), session.SystemPromptFileName)
	require.NoError(t, os.WriteFile(path, make([]byte, (4<<20)+1), 0o600))

	_, ok, err := snap.ReadSystemPrompt()
	require.ErrorIs(t, err, session.ErrSnapshot)
	require.False(t, ok)
}

func TestModelSnapshotSystemPromptRejectsNonRegularFile(t *testing.T) {
	t.Parallel()

	snap := openSnapshot(t)
	require.NoError(t, os.Mkdir(filepath.Join(snap.Dir(), session.SystemPromptFileName), 0o700))

	_, ok, err := snap.ReadSystemPrompt()
	require.ErrorIs(t, err, session.ErrSnapshot)
	require.False(t, ok)
}

func TestModelSnapshotJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, name := range []string{session.PromptContextFileName, session.ToolDefinitionsFileName} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			snap := openSnapshot(t)
			want := snapshotContext{Model: "test-model", Tools: []string{"read", "write"}}
			require.NoError(t, snap.WriteJSON(name, want))

			var got snapshotContext
			ok, err := snap.ReadJSON(name, &got)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, want, got)
		})
	}
}

func TestModelSnapshotJSONRejectsNonJSONName(t *testing.T) {
	t.Parallel()

	snap := openSnapshot(t)
	require.ErrorIs(t, snap.WriteJSON("prompt_context", map[string]string{}), session.ErrSnapshot)

	var target map[string]string
	_, err := snap.ReadJSON("prompt_context", &target)
	require.ErrorIs(t, err, session.ErrSnapshot)
}

func TestModelSnapshotRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	names := []string{"", ".", "..", "a/b", "a\\b", "bad\x00name", strings.Repeat("a", 129)}
	for _, name := range names {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			t.Parallel()

			snap := openSnapshot(t)
			require.ErrorIs(t, snap.WriteJSON(name, map[string]string{}), session.ErrSnapshot)

			var target map[string]string
			_, err := snap.ReadJSON(name, &target)
			require.ErrorIs(t, err, session.ErrSnapshot)
		})
	}
}

func TestModelSnapshotReadJSONRejectsUnknownField(t *testing.T) {
	t.Parallel()

	snap := openSnapshot(t)
	require.NoError(t, snap.WriteJSON(session.PromptContextFileName, map[string]string{
		"model": "test-model",
		"extra": "not a known field",
	}))

	var target snapshotContext
	_, err := snap.ReadJSON(session.PromptContextFileName, &target)
	require.ErrorIs(t, err, session.ErrSnapshot)
}

func TestModelSnapshotReadJSONMissing(t *testing.T) {
	t.Parallel()

	var target snapshotContext
	ok, err := openSnapshot(t).ReadJSON(session.PromptContextFileName, &target)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestOpenModelSnapshotRejectsInvalidDir(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"", "   ", "bad\x00dir"} {
		t.Run(fmt.Sprintf("%q", dir), func(t *testing.T) {
			t.Parallel()

			_, err := session.OpenModelSnapshot(dir)
			require.ErrorIs(t, err, session.ErrSnapshot)
		})
	}
}

func TestOpenModelSnapshotRejectsSymlinkDirectory(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	target := filepath.Join(t.TempDir(), "real")
	require.NoError(t, os.Mkdir(target, 0o700))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(target, link))

	_, err := session.OpenModelSnapshot(link)
	require.Error(t, err)
}
