// Package clipboard isolates desktop clipboard initialization and image reads
// from the Coding TUI.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"slices"

	desktop "golang.design/x/clipboard"
)

var (
	// ErrUnavailable means the host clipboard cannot be initialized or read.
	ErrUnavailable = errors.New("coding clipboard: unavailable")
	// ErrEmpty means the clipboard does not currently contain PNG image data.
	ErrEmpty = errors.New("coding clipboard: no image available")
)

// Reader provides copied image bytes from the host clipboard.
type Reader struct {
	initialize func() error
	read       func() []byte
}

// New returns the production desktop clipboard reader. Initialization remains
// lazy until the first ReadImage call.
func New() *Reader {
	return newReader(desktop.Init, func() []byte {
		return desktop.Read(desktop.FmtImage)
	})
}

func newReader(initialize func() error, read func() []byte) *Reader {
	return &Reader{initialize: initialize, read: read}
}

// ReadImage initializes the backend, reads PNG image data, and returns a
// detached copy. Backend panics are translated without exposing panic values.
func (r *Reader) ReadImage(ctx context.Context) (data []byte, returnErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if r == nil || r.initialize == nil || r.read == nil {
		return nil, ErrUnavailable
	}

	defer func() {
		if recover() != nil {
			data = nil
			returnErr = ErrUnavailable
		}
	}()

	if err := r.initialize(); err != nil {
		return nil, fmt.Errorf("%w: initialize: %w", ErrUnavailable, err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data = r.read()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, ErrEmpty
	}

	return slices.Clone(data), nil
}
