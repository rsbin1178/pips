//go:build darwin

package sshclient

import "syscall"

func sshDeviceID(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev)
}
