//go:build darwin

package workspace

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
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
