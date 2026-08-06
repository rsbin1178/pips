package pluginstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// mkdirAllDurable creates each missing directory explicitly so Unix callers
// can sync the parent directory entry immediately after each successful mkdir.
// It deliberately does not use MkdirAll: walking the missing suffix avoids
// recursively treating a filesystem root as an application-owned directory.
//
//nolint:gocyclo // The ordered identity, symlink, and durability checks are one filesystem transaction.
func mkdirAllDurable(directory string, mode os.FileMode) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("%w: resolve directory: %w", ErrUnsafeArtifact, err)
	}

	var missing []string

	current := absolute
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: directory is not a real directory", ErrUnsafeArtifact)
			}

			break
		}

		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect directory: %w", ErrUnsafeArtifact, statErr)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("%w: directory has no existing root", ErrUnsafeArtifact)
		}

		missing = append(missing, current)
		current = parent
	}

	for _, v := range slices.Backward(missing) {
		created := false

		if err := os.Mkdir(v, mode.Perm()); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%w: create directory: %w", ErrConflict, err)
			}
		} else {
			created = true
		}

		info, statErr := os.Lstat(v)
		if statErr != nil {
			return fmt.Errorf("%w: inspect created directory: %w", ErrUnsafeArtifact, statErr)
		}

		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: directory is not a real directory", ErrUnsafeArtifact)
		}

		if created {
			if err := syncCreatedDirectoryParent(filepath.Dir(v), v); err != nil {
				return err
			}
		}
	}

	return nil
}
