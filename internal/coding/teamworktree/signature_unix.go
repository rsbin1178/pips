//go:build darwin || linux

package teamworktree

import (
	"io/fs"
	"syscall"
)

type fileSignature struct {
	device  uint64
	inode   uint64
	size    int64
	mode    fs.FileMode
	modNano int64
	links   uint64
}

func signatureFromInfo(info fs.FileInfo) (fileSignature, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileSignature{}, ErrUnsupported
	}

	device, err := deviceID(stat)
	if err != nil {
		return fileSignature{}, err
	}

	return fileSignature{
		device: device, inode: stat.Ino, size: info.Size(),
		mode: info.Mode(), modNano: info.ModTime().UnixNano(), links: uint64(stat.Nlink),
	}, nil
}
