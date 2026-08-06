//nolint:wsl_v5 // Transport validation and dialing form one local-channel boundary.
package pluginsupervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
)

func dialTransport(ctx context.Context, bootstrap Bootstrap, tempRoot string) (net.Conn, error) {
	if err := validateTransportEndpoint(bootstrap.Transport, bootstrap.Endpoint, tempRoot); err != nil {
		return nil, err
	}
	switch bootstrap.Transport {
	case TransportTCP:
		return (&net.Dialer{}).DialContext(ctx, "tcp", bootstrap.Endpoint)
	case TransportUnix:
		conn, err := dialUnixTransport(ctx, bootstrap.Endpoint)
		if err != nil {
			return nil, err
		}
		if err := validateUnixEndpoint(bootstrap.Endpoint, tempRoot); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	case TransportNamedPipe:
		return dialNamedPipeTransport(ctx, bootstrap.Endpoint)
	default:
		return nil, ErrUnsupportedTransport
	}
}

//nolint:gocyclo // Endpoint validation keeps path, symlink, and ownership checks together.
func validateUnixEndpointCommon(endpoint, tempRoot string) error {
	if endpoint == "" || !filepath.IsAbs(endpoint) || tempRoot == "" {
		return ErrNonLocalEndpoint
	}
	root, err := filepath.Abs(tempRoot)
	if err != nil {
		return ErrNonLocalEndpoint
	}
	path, err := filepath.Abs(endpoint)
	if err != nil {
		return ErrNonLocalEndpoint
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return ErrNonLocalEndpoint
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ErrNonLocalEndpoint
	}
	current := root
	for _, part := range splitRelativePath(rel) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				break
			}
			return ErrNonLocalEndpoint
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrNonLocalEndpoint
		}
		if current != path && !info.IsDir() {
			return ErrNonLocalEndpoint
		}
		if current == path && (info.Mode()&os.ModeSocket == 0 || !unixEndpointOwnedByCurrentUser(info)) {
			return ErrNonLocalEndpoint
		}
	}
	return nil
}

func splitRelativePath(path string) []string {
	path = filepath.Clean(path)
	if path == "." || path == "" {
		return nil
	}
	return strings.Split(path, string(filepath.Separator))
}
