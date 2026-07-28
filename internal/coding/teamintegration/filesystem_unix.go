//go:build darwin || linux

package teamintegration

import (
	"fmt"
	"os"
	"syscall"
)

func fileLinkCount(root *os.Root, path string) (uint64, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return 0, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: filesystem metadata", ErrInvalid)
	}

	return uint64(stat.Nlink), nil
}

func fileObjectIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%w: filesystem metadata", ErrInvalid)
	}

	return uint64(stat.Dev), stat.Ino, nil //nolint:gosec // Kernel device IDs are persisted as opaque unsigned identity bits.
}
