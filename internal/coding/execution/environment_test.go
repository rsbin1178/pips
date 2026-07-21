package execution

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentSnapshotUsesAllowlistAndPrivatePaths(t *testing.T) {
	t.Parallel()

	privateDir := t.TempDir()
	parent := map[string]string{
		"API_KEY":      "must-not-appear",
		"HOME":         "/home/test",
		"HTTPS_PROXY":  "must-not-appear",
		"LANG":         "C",
		"LD_PRELOAD":   "must-not-appear",
		"OTEL_HEADERS": "must-not-appear",
		"PATH":         strings.Join([]string{"/usr/bin", ".", "relative", "/bin", "/usr/bin", ""}, string(os.PathListSeparator)),
		"PIPS_HOME":    "must-not-appear",
	}

	environment, err := NewChildEnvironment(mapLookup(parent), privateDir, []EnvVar{
		{Name: "GIT_PAGER", Value: "cat"},
		{Name: "HOME", Value: "/override/home"},
		{Name: "TMPDIR", Value: "/must/not/win"},
	})
	require.NoError(t, err)

	values := parseEnvironment(environment)
	assert.Equal(t, "/override/home", values["HOME"])
	assert.Equal(t, strings.Join([]string{"/usr/bin", "/bin"}, string(os.PathListSeparator)), values["PATH"])
	assert.Equal(t, "cat", values["GIT_PAGER"])
	assert.Equal(t, filepath.Join(privateDir, "tmp"), values["TMPDIR"])
	assert.Equal(t, filepath.Join(privateDir, "cache"), values["XDG_CACHE_HOME"])
	assert.NotContains(t, values, "API_KEY")
	assert.NotContains(t, values, "HTTPS_PROXY")
	assert.NotContains(t, values, "LD_PRELOAD")
	assert.NotContains(t, values, "OTEL_HEADERS")
	assert.NotContains(t, values, "PIPS_HOME")
	assert.True(t, slices.IsSorted(environment))

	for _, name := range []string{"tmp", "cache", "go-cache", "go-tmp"} {
		info, statErr := os.Stat(filepath.Join(privateDir, name))
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
}

func TestEnvironmentSnapshotRejectsMissingLookup(t *testing.T) {
	t.Parallel()

	_, err := NewChildEnvironment(nil, t.TempDir(), nil)
	require.ErrorIs(t, err, ErrInvalidOperation)
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]

		return value, ok
	}
}

func parseEnvironment(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}

	return values
}
