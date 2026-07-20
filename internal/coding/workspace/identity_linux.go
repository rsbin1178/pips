//go:build linux

package workspace

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	return stat.Dev, stat.Ino, nil
}
