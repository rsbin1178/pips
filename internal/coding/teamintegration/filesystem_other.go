//go:build !darwin && !linux

package teamintegration

import (
	"fmt"
	"os"
)

func fileLinkCount(*os.Root, string) (uint64, error) {
	return 0, fmt.Errorf("%w: unsupported platform", ErrInvalid)
}

func fileObjectIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("%w: unsupported platform", ErrInvalid)
}
