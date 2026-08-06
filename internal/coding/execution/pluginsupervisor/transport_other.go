//go:build !windows && !darwin && !linux

package pluginsupervisor

import (
	"context"
	"net"
)

func dialUnixTransport(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedTransport
}
func validateUnixEndpoint(string, string) error { return ErrUnsupportedTransport }
func dialNamedPipeTransport(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedTransport
}
func validateNamedPipeEndpoint(string) error { return ErrUnsupportedTransport }
