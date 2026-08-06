//go:build windows

package pluginsupervisor

import (
	"net"

	"github.com/Microsoft/go-winio"
)

func listenFixtureNamedPipe(address string) (net.Listener, error) {
	return winio.ListenPipe(address, &winio.PipeConfig{})
}
