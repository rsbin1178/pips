//go:build darwin

package teamworktree

import (
	"fmt"
	"strconv"
	"syscall"
)

func deviceID(stat *syscall.Stat_t) (uint64, error) {
	device, err := strconv.ParseUint(strconv.FormatInt(int64(stat.Dev), 10), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid filesystem device identity", ErrUnsupported)
	}

	return device, nil
}
