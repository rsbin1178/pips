package cli

import (
	"context"
	"fmt"
)

// SystemProxyProbe returns the host:port of the operating system's HTTPS
// proxy, or "" when none is enabled or the platform has no such setting.
type SystemProxyProbe func(context.Context) string

// doctorProxyStatus reports how remote HTTP connections reach the network.
// Go's HTTP client reads only HTTPS_PROXY/https_proxy for https URLs and never
// the macOS system proxy, so a system proxy without the variable means remote
// MCP servers and providers connect directly. Values are never printed; a
// proxy URL may carry credentials.
func doctorProxyStatus(ctx context.Context, dependencies Dependencies) string {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if value, ok := dependencies.LookupEnv(name); ok && value != "" {
			return fmt.Sprintf("network ok proxy=environment variable=%s\n", name)
		}
	}

	system := ""
	if dependencies.SystemProxy != nil {
		system = dependencies.SystemProxy(ctx)
	}

	if system == "" {
		return "network ok proxy=none\n"
	}

	return fmt.Sprintf(
		"network warning proxy=none system_proxy=%s\n"+
			"Next steps: export HTTPS_PROXY=http://%s (and NO_PROXY=localhost,127.0.0.1); "+
			"pips ignores the system proxy, so remote MCP servers and providers connect directly\n",
		system, system,
	)
}
