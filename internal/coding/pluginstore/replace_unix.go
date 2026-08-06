//go:build !windows

package pluginstore

import "os"

// replaceFile atomically installs source at destination. Unix rename replaces
// an existing destination as one filesystem operation.
func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
