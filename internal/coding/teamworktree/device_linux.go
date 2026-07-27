//go:build linux

package teamworktree

import "syscall"

func deviceID(stat *syscall.Stat_t) (uint64, error) {
	return stat.Dev, nil
}
