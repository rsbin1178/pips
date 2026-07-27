//go:build darwin || linux

package teamworktree

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func canonicalDirectory(path string) (FileIdentity, error) {
	if err := validateAbsolutePath(path); err != nil {
		return FileIdentity{}, err
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: resolve directory: %w", ErrIdentity, err)
	}

	resolved = filepath.Clean(resolved)

	info, err := os.Lstat(resolved)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: stat directory: %w", ErrIdentity, err)
	}

	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return FileIdentity{}, fmt.Errorf("%w: path is not a real directory", ErrIdentity)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, ErrUnsupported
	}

	device, err := deviceID(stat)
	if err != nil {
		return FileIdentity{}, err
	}

	return FileIdentity{Path: resolved, Device: device, Inode: stat.Ino}, nil
}

func fileIdentity(path string) (FileIdentity, error) {
	if err := validateAbsolutePath(path); err != nil {
		return FileIdentity{}, err
	}

	info, err := os.Lstat(path)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: stat path: %w", ErrIdentity, err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, ErrUnsupported
	}

	device, err := deviceID(stat)
	if err != nil {
		return FileIdentity{}, err
	}

	return FileIdentity{Path: path, Device: device, Inode: stat.Ino}, nil
}

func sameFileIdentity(expected FileIdentity) error {
	actual, err := fileIdentity(expected.Path)
	if err != nil {
		return err
	}

	if actual != expected {
		return ErrIdentity
	}

	return nil
}
