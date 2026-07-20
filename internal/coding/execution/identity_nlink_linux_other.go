//go:build linux && !amd64 && !arm64

package execution

import (
	"fmt"
	"io/fs"
	"reflect"
	"syscall"
)

func fileLinkCount(info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: filesystem metadata unavailable", ErrUnsupportedPlatform)
	}

	return reflect.ValueOf(stat.Nlink).Uint(), nil
}
