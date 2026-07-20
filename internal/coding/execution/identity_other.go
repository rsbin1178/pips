//go:build !darwin && !linux

package execution

import "io/fs"

type fileObject struct {
	path   string
	device uint64
	inode  uint64
	mode   fs.FileMode
}

func fileIdentity(fs.FileInfo) (uint64, uint64, error) { return 0, 0, ErrUnsupportedPlatform }

func executableMode(fs.FileMode) bool { return false }

func fileLinkCount(fs.FileInfo) (uint64, error) { return 0, ErrUnsupportedPlatform }
