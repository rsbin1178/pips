//go:build darwin

package execution

import (
	"fmt"
	"io/fs"
	"strconv"
	"syscall"
)

type fileObject struct {
	path   string
	device uint64
	inode  uint64
	mode   fs.FileMode
}

func fileIdentity(info fs.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	device, err := strconv.ParseUint(strconv.FormatInt(int64(stat.Dev), 10), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid filesystem device: %w", ErrUnsupportedPlatform, err)
	}

	return device, stat.Ino, nil
}

func executableMode(mode fs.FileMode) bool { return mode.Perm()&0o111 != 0 }

func fileLinkCount(info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	return uint64(stat.Nlink), nil
}
