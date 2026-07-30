//go:build linux

package imagebridge

import (
	"io/fs"
	"syscall"
)

func fileIdentityAndOwner(info fs.FileInfo) (unixIdentity, int, bool) {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok || status == nil {
		return unixIdentity{}, 0, false
	}

	return unixIdentity{device: status.Dev, inode: status.Ino}, int(status.Uid), true
}
