// Package systemproxy reads the operating system's HTTPS proxy setting that
// Go's HTTP client ignores.
package systemproxy

import (
	"bufio"
	"net"
	"strings"
)

// parseScutilHTTPSProxy extracts the enabled HTTPS proxy from `scutil --proxy`
// output as a host:port pair, or "" when no HTTPS proxy is enabled.
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
