package paths_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(base)
	require.NoError(t, err)

	root := filepath.Join(base, "pips")
	assert.Equal(t, root, layout.Root())
	assert.Equal(t, filepath.Join(root, "config.toml"), layout.ConfigFile())
	assert.Equal(t, filepath.Join(root, "trust.json"), layout.TrustFile())
	assert.Equal(t, filepath.Join(root, "sessions"), layout.SessionsDir())
}

func TestNewRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := paths.New("")
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestDefault(t *testing.T) {
	t.Parallel()

	layout, err := paths.Default()
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(layout.Root()))
	assert.Equal(t, "pips", filepath.Base(layout.Root()))
}
