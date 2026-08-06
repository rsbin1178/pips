//go:build windows

package pluginstore

import "os"

// Windows does not use POSIX mode bits to decide whether an executable can be
// launched. The launch/supervisor layer validates Windows executable format
// and policy instead.
func validateSourceArtifactMode(os.FileMode) error { return nil }
