//go:build darwin || linux

package pluginsupervisor

import (
	"os"
	"syscall"
)

func unixEndpointOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(os.Getuid()) == uint64(stat.Uid) //nolint:gosec // Unix UIDs are non-negative on supported platforms.
}
