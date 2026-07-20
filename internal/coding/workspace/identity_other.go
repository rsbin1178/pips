//go:build !darwin && !linux

package workspace

import "os"

func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, ErrUnsupportedPlatform
}
