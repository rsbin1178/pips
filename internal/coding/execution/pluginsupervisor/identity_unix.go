//go:build darwin || linux

//nolint:wsl_v5 // Platform-specific executable validation is intentionally tiny.
package pluginsupervisor

import "os"

func validateExecutableMode(mode os.FileMode) error {
	if mode.Perm()&0o111 == 0 {
		return invalidConfig("artifact is not executable")
	}
	return nil
}
