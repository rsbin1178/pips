//go:build linux && arm64

package execution

import (
	"fmt"
	"io/fs"
	"syscall"
)

func fileLinkCount(info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	return uint64(stat.Nlink), nil
}
