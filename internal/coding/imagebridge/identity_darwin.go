//go:build darwin

package imagebridge

import (
	"io/fs"
	"syscall"
)

func fileIdentityAndOwner(info fs.FileInfo) (unixIdentity, int, bool) {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok || status == nil || status.Dev < 0 {
		return unixIdentity{}, 0, false
	}

	return unixIdentity{
		device: uint64(status.Dev),
		inode:  status.Ino,
	}, int(status.Uid), true
}
