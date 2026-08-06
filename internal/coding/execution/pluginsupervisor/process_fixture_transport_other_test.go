//go:build !windows

package pluginsupervisor

import (
	"errors"
	"net"
)

func listenFixtureNamedPipe(string) (net.Listener, error) {
	return nil, errors.New("pluginsupervisor: Windows named pipes are unavailable")
}
