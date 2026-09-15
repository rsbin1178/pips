package cli

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveBuildInfoPrefersStampedValues(t *testing.T) {
	t.Parallel()

	stamped := BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-20T10:00:00Z"}
	assert.Equal(t, stamped, resolveBuildInfo(stamped, moduleInstalledBuildInfo()))
}

func TestResolveBuildInfoFillsUnstampedValuesFromBuildMetadata(t *testing.T) {
	t.Parallel()

	resolved := resolveBuildInfo(BuildInfo{Version: "dev", Commit: "unknown", Date: "unknown"}, moduleInstalledBuildInfo())

	assert.Equal(t, "v0.1.1", resolved.Version)
	assert.Equal(t, "8360f2c1234a", resolved.Commit, "the revision is trimmed to the build-stamp length")
	assert.Equal(t, "2026-09-15T14:08:59Z", resolved.Date)
}

func TestResolveBuildInfoFillsPartiallyStampedValues(t *testing.T) {
	t.Parallel()

	resolved := resolveBuildInfo(
		BuildInfo{Version: "dev", Commit: "custom-commit", Date: ""},
		moduleInstalledBuildInfo(),
	)

	assert.Equal(t, "v0.1.1", resolved.Version)
	assert.Equal(t, "custom-commit", resolved.Commit, "an explicit stamp is not replaced")
	assert.Equal(t, "2026-09-15T14:08:59Z", resolved.Date)
}

func TestResolveBuildInfoKeepsDevelopmentPlaceholders(t *testing.T) {
	t.Parallel()

	tests := map[string]*debug.BuildInfo{
		"nil metadata":   nil,
		"devel version":  {Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: ""}}},
		"empty metadata": {},
	}

	for name, info := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resolved := resolveBuildInfo(BuildInfo{Version: "dev"}, info)

			assert.Equal(t, "dev", resolved.Version)
			assert.Equal(t, "unknown", resolved.Commit)
			assert.Equal(t, "unknown", resolved.Date)
		})
	}
}

// moduleInstalledBuildInfo is the metadata `go install <module>/cmd/pips@v0.1.1`
// produces: module version plus the VCS revision and time of the tagged source.
func moduleInstalledBuildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/rsbin1178/pips/cmd/pips", Version: "v0.1.1"},
		Settings: []debug.BuildSetting{
			{Key: "GOARCH", Value: "arm64"},
			{Key: "vcs.revision", Value: "8360f2c1234a567890abcdef1234567890abcdef"},
			{Key: "vcs.time", Value: "2026-09-15T14:08:59Z"},
		},
	}
}
