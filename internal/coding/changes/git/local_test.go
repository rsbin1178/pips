package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectorTracksAndLocallyExcludesProjectFile(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write(".pips/permissions.toml", "schema = 'pips.permissions/v1alpha1'\n")

	tracked, repository, err := fixture.inspector.Tracked(
		t.Context(),
		".pips/permissions.toml",
	)
	require.NoError(t, err)
	assert.True(t, repository)
	assert.False(t, tracked)

	require.NoError(t, fixture.inspector.ExcludeLocal(
		t.Context(),
		".pips/permissions.toml",
	))
	require.NoError(t, fixture.inspector.ExcludeLocal(
		t.Context(),
		".pips/permissions.toml",
	))

	content, err := os.ReadFile(filepath.Join(fixture.root, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "/.pips/permissions.toml\n")
	assert.Equal(t, 1, strings.Count(string(content), "/.pips/permissions.toml\n"))

	fixture.git("add", "-f", "--", ".pips/permissions.toml")
	tracked, repository, err = fixture.inspector.Tracked(t.Context(), ".pips/permissions.toml")
	require.NoError(t, err)
	assert.True(t, repository)
	assert.True(t, tracked)
	require.Error(t, fixture.inspector.ExcludeLocal(t.Context(), ".pips/permissions.toml"))
}

func TestInspectorTrackedReportsNonRepository(t *testing.T) {
	t.Parallel()

	fixture := newGitFixtureWithoutRepository(t, DefaultLimits())
	tracked, repository, err := fixture.inspector.Tracked(t.Context(), ".pips/permissions.toml")
	require.NoError(t, err)
	assert.False(t, tracked)
	assert.False(t, repository)
}
