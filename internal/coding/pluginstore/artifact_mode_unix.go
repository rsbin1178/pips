//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pluginstore

import (
	"fmt"
	"os"
)

func validateSourceArtifactMode(mode os.FileMode) error {
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("%w: source artifact must have at least one execute bit", ErrUnsafeArtifact)
	}

	return nil
}
