//go:build !darwin && !linux

package teamworktree

import "io/fs"

type fileSignature struct{ links uint64 }

func signatureFromInfo(fs.FileInfo) (fileSignature, error) {
	return fileSignature{}, ErrUnsupported
}
