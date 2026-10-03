package cli

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// SystemProxyProbe returns the host:port of the operating system's HTTPS
// proxy, or "" when none is enabled or the platform has no such setting.
type SystemProxyProbe func(context.Context) string

const systemProxyProbeTimeout = 2 * time.Second

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

func nativeSystemProxy(ctx context.Context) string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, systemProxyProbeTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return ""
	}

	return parseScutilHTTPSProxy(string(output))
}

// parseScutilHTTPSProxy extracts the enabled HTTPS proxy from `scutil --proxy`
// output.
func parseScutilHTTPSProxy(output string) string {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), " : ")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	host, port := values["HTTPSProxy"], values["HTTPSPort"]
	if values["HTTPSEnable"] != "1" || host == "" || port == "" {
		return ""
	}

	return net.JoinHostPort(host, port)
}
