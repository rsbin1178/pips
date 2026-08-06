//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package pluginstore

import "os"

func validateSourceArtifactMode(os.FileMode) error { return nil }
