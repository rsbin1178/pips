//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package pluginstore

func syncCreatedDirectoryParent(_, _ string) error { return nil }
