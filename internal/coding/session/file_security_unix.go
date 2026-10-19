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

// secureMediaFile applies the same private-file policy to session media that
// session files use, so a media file replaced with a looser mode is refused
// rather than hydrated into a provider request.
func secureMediaFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding session: inspect media file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: media file is not a regular file", ErrMediaInvalid)
	}

	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf(
			"%w: media file mode %04o must be 0600",
			ErrMediaInvalid,
			info.Mode().Perm(),
		)
	}

	return nil
}
