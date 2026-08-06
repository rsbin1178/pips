//go:build darwin || linux

package pluginsupervisor

import "os"

//nolint:wsl_v5 // Opening, syncing, and closing the launch root are one durability boundary.
func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Path is a supervisor-owned launch root.
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
