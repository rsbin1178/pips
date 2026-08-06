//go:build windows

package pluginsupervisor

import (
	"context"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func dialUnixTransport(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedTransport
}
func validateUnixEndpoint(string, string) error { return ErrUnsupportedTransport }

func dialNamedPipeTransport(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

func validateNamedPipeEndpoint(endpoint string) error {
	const prefix = `\\.\pipe\pips-plugin-`
	if !strings.HasPrefix(endpoint, prefix) || len(endpoint) <= len(prefix) || len(endpoint) > len(prefix)+128 {
		return ErrNonLocalEndpoint
	}
	for _, r := range endpoint[len(prefix):] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return ErrNonLocalEndpoint
		}
	}
	// Named-pipe ACLs and peer identity are deployment/Windows-CI concerns;
	// the bootstrap token remains an additional application-level check.
	return nil
}
