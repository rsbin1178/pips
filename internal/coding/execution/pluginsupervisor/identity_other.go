//go:build !darwin && !linux && !windows

package pluginsupervisor

import "os"

func validateExecutableMode(mode os.FileMode) error {
	if !mode.IsRegular() {
		return invalidConfig("artifact is not a regular file")
	}
	return nil
}
