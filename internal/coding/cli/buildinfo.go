package cli

import (
	"runtime/debug"
	"strings"
)

// Build stamps that mean "the build did not set this value".
const (
	unstampedVersion = "dev"
	unstampedCommit  = "unknown"
)

// revisionLength matches the abbreviation used by the release build stamp.
const revisionLength = 12

// ResolveBuildInfo completes stamped build information from the Go build
// metadata. Release builds inject every field with -ldflags, but
// `go install github.com/rsbin1178/pips/cmd/pips@<version>` cannot, so those
// binaries fall back to the module version and the VCS revision Go embeds in
// them. Values the build already stamped always win.
func ResolveBuildInfo(stamped BuildInfo) BuildInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return normalizeBuildInfo(stamped)
	}

	return resolveBuildInfo(stamped, info)
}

func resolveBuildInfo(stamped BuildInfo, info *debug.BuildInfo) BuildInfo {
	if info != nil {
		fillBuildInfo(&stamped, info)
	}

	return normalizeBuildInfo(stamped)
}

func fillBuildInfo(stamped *BuildInfo, info *debug.BuildInfo) {
	if version, ok := moduleVersion(info); ok && unstamped(stamped.Version) {
		stamped.Version = version
	}

	if revision, ok := buildSetting(info, "vcs.revision"); ok && unstamped(stamped.Commit) {
		stamped.Commit = shortRevision(revision)
	}

	if revised, ok := buildSetting(info, "vcs.time"); ok && unstamped(stamped.Date) {
		stamped.Date = revised
	}
}

// moduleVersion reports the version a module-installed binary was built from.
// Development builds of the main module report an empty or "(devel)" version
// and are left to the caller.
func moduleVersion(info *debug.BuildInfo) (string, bool) {
	version := strings.TrimSpace(info.Main.Version)
	if version == "" || version == "(devel)" {
		return "", false
	}

	return version, true
}

func buildSetting(info *debug.BuildInfo, key string) (string, bool) {
	for _, setting := range info.Settings {
		if setting.Key != key {
			continue
		}

		if value := strings.TrimSpace(setting.Value); value != "" {
			return value, true
		}

		return "", false
	}

	return "", false
}

func unstamped(value string) bool {
	switch strings.TrimSpace(value) {
	case "", unstampedVersion, unstampedCommit:
		return true
	default:
		return false
	}
}

func shortRevision(revision string) string {
	if len(revision) > revisionLength {
		return revision[:revisionLength]
	}

	return revision
}
