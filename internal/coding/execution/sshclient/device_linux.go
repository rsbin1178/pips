//go:build linux

package sshclient

import "syscall"

func sshDeviceID(stat *syscall.Stat_t) uint64 {
	return stat.Dev
}
