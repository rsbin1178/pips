//go:build darwin || linux

package pluginsupervisor

import (
	"context"
	"net"
)

func dialUnixTransport(ctx context.Context, endpoint string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}

func validateUnixEndpoint(endpoint, tempRoot string) error {
	return validateUnixEndpointCommon(endpoint, tempRoot)
}

func dialNamedPipeTransport(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedTransport
}
func validateNamedPipeEndpoint(string) error { return ErrUnsupportedTransport }
