package tuiconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/statusline"
	"github.com/rsbin/pips/internal/coding/tuiconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreDefaultsAndRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private", "tui.json")
	store := tuiconfig.NewStore(path)
	settings, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, statusline.Default(), settings.StatusLine)

	want := tuiconfig.Settings{StatusLine: []statusline.Item{
		statusline.Model, statusline.ContextUsed, statusline.TaskProgress,
	}}
	require.NoError(t, store.Save(want))
	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStoreRejectsUnsafeOrMalformedPreferences(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	path := filepath.Join(directory, "tui.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schema":"pips.tui/v1alpha1","status_line":{"items":["unknown"]}}`), 0o600))
	_, err := tuiconfig.NewStore(path).Load()
	require.Error(t, err)

	require.NoError(t, os.WriteFile(path, []byte(`{"schema":"pips.tui/v1alpha1","status_line":{"items":[]},"extra":true}`), 0o600))
	_, err = tuiconfig.NewStore(path).Load()
	require.Error(t, err)

	require.NoError(t, os.WriteFile(path, []byte(`{"schema":"pips.tui/v1alpha1","schema":"other","status_line":{"items":[]}}`), 0o600))
	_, err = tuiconfig.NewStore(path).Load()
	require.Error(t, err)

	require.NoError(t, os.Chmod(path, 0o644))
	_, err = tuiconfig.NewStore(path).Load()
	require.ErrorContains(t, err, "0600")
}
