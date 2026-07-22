//go:build darwin || linux

package session

import (
	"fmt"
	"os"
)

func secureSessionFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding session: inspect session file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: session file is not a regular file", ErrInvalid)
	}

	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf(
			"%w: session file mode %04o must be 0600; run chmod 600 on the file",
			ErrInvalid,
			info.Mode().Perm(),
		)
	}

	return nil
}
