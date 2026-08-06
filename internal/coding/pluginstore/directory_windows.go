//go:build windows

package pluginstore

// Windows directory-entry durability is intentionally deferred to the
// platform-specific filesystem/installation layer. The store still creates
// and validates every directory without following symlinks or reparse points
// detectable through its portable checks.
func syncCreatedDirectoryParent(_, _ string) error { return nil }
