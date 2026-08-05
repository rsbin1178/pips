//nolint:gosec,wsl_v5 // Internal persistence tests intentionally inspect controlled temp paths.
package config

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyThemeRevisionRejectsChangedContentAndReplacement(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := []byte("[tui]\ntheme = \"auto\"\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	parent, err := os.Lstat(directory)
	require.NoError(t, err)
	identity, err := os.Lstat(path)
	require.NoError(t, err)
	revision := themeRevision{
		exists:         true,
		identity:       identity,
		parentIdentity: parent,
		digest:         sha256.Sum256(original),
	}

	require.NoError(t, os.WriteFile(path, []byte("[tui]\ntheme = \"nord\"\n"), 0o600))
	require.ErrorIs(t, verifyThemeRevision(path, directory, revision), ErrConflict)

	require.NoError(t, os.Remove(path))
	replacement := filepath.Join(directory, "replacement.toml")
	require.NoError(t, os.WriteFile(replacement, original, 0o600))
	require.NoError(t, os.Rename(replacement, path))
	require.ErrorIs(t, verifyThemeRevision(path, directory, revision), ErrConflict)
}

func TestVerifyThemeRevisionRejectsReplacedParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "config")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "config.toml")
	original := []byte("[tui]\ntheme = \"auto\"\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	parentIdentity, err := os.Lstat(parent)
	require.NoError(t, err)
	identity, err := os.Lstat(path)
	require.NoError(t, err)
	revision := themeRevision{
		exists:         true,
		identity:       identity,
		parentIdentity: parentIdentity,
		digest:         sha256.Sum256(original),
	}

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.ErrorIs(t, verifyThemeRevision(path, parent, revision), ErrConflict)
}

//nolint:paralleltest // These tests temporarily replace package-level persistence hooks.
func TestSaveThemeRejectsExternalChangeBeforeReplacement(t *testing.T) {
	originalHook := beforeThemeReplace
	beforeThemeReplace = func(path string) {
		_ = os.WriteFile(path, []byte("[tui]\ntheme = \"nord\"\n"), 0o600)
	}
	defer func() { beforeThemeReplace = originalHook }()

	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[tui]\ntheme = \"auto\"\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	err := SaveThemeWithOptions(path, "dracula", ThemeSaveOptions{})
	require.ErrorIs(t, err, ErrConflict)
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "[tui]\ntheme = \"nord\"\n", string(data))
}

//nolint:paralleltest // This test temporarily replaces a package-level sync hook.
func TestSaveThemeReportsCommittedDurabilityFailure(t *testing.T) {
	originalSync := syncThemeDirectory
	syncThemeDirectory = func(string) error { return errors.New("injected directory sync failure") }
	defer func() { syncThemeDirectory = originalSync }()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[tui]\ntheme = \"auto\"\n"), 0o600))

	err := SaveThemeWithOptions(path, "nord", ThemeSaveOptions{})
	require.ErrorIs(t, err, ErrThemeDurability)
	if err != nil {
		// The rename has already happened; this is a durability warning rather
		// than a report that the selection was not applied.
		if !strings.Contains(err.Error(), "replacement committed") {
			t.Errorf("error does not describe committed replacement: %v", err)
		}
	}
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	if string(data) != "[tui]\ntheme = \"nord\"\n" {
		t.Fatalf("unexpected committed data: %q", data)
	}
}
