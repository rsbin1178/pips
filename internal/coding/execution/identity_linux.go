//go:build linux

package execution

import (
	"fmt"
	"io/fs"
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

	return stat.Dev, stat.Ino, nil
}

func executableMode(mode fs.FileMode) bool { return mode.Perm()&0o111 != 0 }

func fileLinkCount(info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	return uint64(stat.Nlink), nil
}
