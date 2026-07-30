//go:build !darwin && !linux

package imagebridge

import (
	"context"

	"github.com/rsbin/pips/internal/coding/attachment"
)

// Server is unavailable on unsupported operating systems.
type Server struct{}

// Listen returns ErrUnavailable on unsupported operating systems.
func Listen(context.Context, string) (*Server, error) { return nil, ErrUnavailable }

// Receive returns ErrUnavailable on unsupported operating systems.
func (*Server) Receive(context.Context) (attachment.Image, error) {
	return attachment.Image{}, ErrUnavailable
}

// Close is a no-op for an unavailable Server.
func (*Server) Close() error { return nil }

// Send returns ErrUnavailable on unsupported operating systems.
func Send(context.Context, string, Frame) error { return ErrUnavailable }
