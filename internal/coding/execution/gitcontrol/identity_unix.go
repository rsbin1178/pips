//go:build darwin || linux

package gitcontrol

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func resolveExecutable(path string) (executableIdentity, error) {
	if err := validateAbsolutePath(path); err != nil {
		return executableIdentity{}, err
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return executableIdentity{}, fmt.Errorf("%w: resolve executable: %w", ErrInvalid, err)
	}

	if !filepath.IsAbs(resolved) {
		return executableIdentity{}, fmt.Errorf("%w: resolved executable is not absolute", ErrInvalid)
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return executableIdentity{}, fmt.Errorf("%w: stat executable: %w", ErrInvalid, err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return executableIdentity{}, fmt.Errorf("%w: executable is not an executable regular file", ErrInvalid)
	}

	device, err := deviceID(stat)
	if err != nil {
		return executableIdentity{}, err
	}

	return executableIdentity{
		path: resolved, device: device, inode: stat.Ino,
	}, nil
}

func (identity executableIdentity) validate() error {
	current, err := resolveExecutable(identity.path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrExecutableChanged, err)
	}

	if current != identity {
		return ErrExecutableChanged
	}

	return nil
}
