//go:build darwin

package systemproxy

import (
	"context"
	"os/exec"
	"time"
)

// probeTimeout bounds the system-proxy lookup so a stalled `scutil` cannot stall
// the doctor command.
const probeTimeout = 2 * time.Second

// Probe returns the host:port of the operating system's HTTPS proxy, or "" when
// none is enabled.
func Probe(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return ""
	}

	return parseScutilHTTPSProxy(string(output))
}
