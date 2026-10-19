//nolint:wsl_v5 // Probe steps keep dial, CONNECT, and classification adjacent.
package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SystemProxyProbe returns the host:port of the operating system's HTTPS
// proxy, or "" when none is enabled or the platform has no such setting.
type SystemProxyProbe func(context.Context) string

// ProxyProbeResult is a credential-free health result for one proxy endpoint.
// Detail is a fixed category such as "timeout", "dial_failed", or
// "status_502"; it never contains the proxy URL or a server response body.
type ProxyProbeResult struct {
	OK      bool
	Latency time.Duration
	Detail  string
}

// ProxyProbe checks that a configured proxy accepts a CONNECT tunnel. It is
// injectable so doctor tests do not depend on a local proxy.
type ProxyProbe func(context.Context, string) ProxyProbeResult

const (
	proxyProbeTimeout      = 3 * time.Second
	proxyProbeTarget       = "example.com:443"
	proxyProbeNoResponse   = "no_response"
	proxyProbeTunnelClosed = "tunnel_closed"
)

type proxyEndpoint struct {
	scheme        string
	host          string
	serverName    string
	authorization string
}

// doctorProxyStatus reports how remote HTTP connections reach the network.
// Go's HTTP client reads only HTTPS_PROXY/https_proxy for https URLs and never
// the macOS system proxy, so a system proxy without the variable means remote
// MCP servers and providers connect directly. Values are never printed; a
// proxy URL may carry credentials.
func doctorProxyStatus(ctx context.Context, dependencies Dependencies) string {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if value, ok := dependencies.LookupEnv(name); ok && value != "" {
			if dependencies.ProxyProbe == nil {
				return fmt.Sprintf("network ok proxy=environment variable=%s\n", name)
			}

			result := dependencies.ProxyProbe(ctx, value)
			if result.OK {
				line := fmt.Sprintf(
					"network ok proxy=environment variable=%s probe=ok latency=%s",
					name, result.Latency.Round(time.Microsecond),
				)
				if result.Detail != "" {
					line += " detail=" + result.Detail
				}

				return line + "\n"
			}

			return fmt.Sprintf(
				"network warning proxy=environment variable=%s probe=failed detail=%s\n"+
					"Next steps: check the proxy process, or bypass it for MCP hosts with NO_PROXY; "+
					"see pips mcp doctor\n",
				name, result.Detail,
			)
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

// nativeProxyProbe dials the proxy and, for HTTP(S) proxies, asks it to open a
// CONNECT tunnel to a neutral host. A proxy that answers any HTTP status is
// reachable; only dial, TLS, and response timeouts are probe failures.
//
//nolint:gocyclo // Dial, TLS, CONNECT, and status classification are one probe boundary.
func nativeProxyProbe(ctx context.Context, rawURL string) ProxyProbeResult {
	started := time.Now()

	endpoint, err := parseProxyEndpoint(rawURL)
	if err != nil {
		return ProxyProbeResult{Detail: "invalid_proxy_url"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()

	var dialer net.Dialer

	conn, err := dialer.DialContext(probeCtx, "tcp", endpoint.host)
	if err != nil {
		return ProxyProbeResult{Detail: proxyDialDetail(err)}
	}
	defer func() { _ = conn.Close() }()

	if endpoint.scheme == "socks5" || endpoint.scheme == "socks5h" {
		// A SOCKS5 greeting is deliberately out of scope; a reachable TCP
		// listener is the useful doctor signal here.
		return ProxyProbeResult{OK: true, Latency: time.Since(started)}
	}

	if endpoint.scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: endpoint.serverName,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(probeCtx); err != nil {
			return ProxyProbeResult{Detail: "tls_failed"}
		}
		conn = tlsConn
	}

	if deadline, ok := probeCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	request := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", proxyProbeTarget, proxyProbeTarget)
	if endpoint.authorization != "" {
		request += "Proxy-Authorization: " + endpoint.authorization + "\r\n"
	}
	request += "\r\n"

	if _, err := fmt.Fprint(conn, request); err != nil {
		return ProxyProbeResult{Detail: proxyProbeNoResponse}
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return ProxyProbeResult{Detail: proxyProbeNoResponse}
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return ProxyProbeResult{Detail: proxyProbeNoResponse}
	}
	statusCode, err := strconv.Atoi(fields[1])
	if err != nil {
		return ProxyProbeResult{Detail: proxyProbeNoResponse}
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			return ProxyProbeResult{Detail: proxyProbeNoResponse}
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	result := ProxyProbeResult{OK: true, Latency: time.Since(started)}
	switch {
	case statusCode == http.StatusProxyAuthRequired:
		result.OK = false
		result.Detail = "proxy_auth_required"

		return result
	case statusCode >= 200 && statusCode < 300:
		// A CONNECT 200 only proves the proxy accepted the request. Keep the
		// tunnel open long enough to notice a proxy that immediately resets
		// it, which is exactly the failure a TLS handshake would see.
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, readErr := reader.ReadByte(); readErr != nil {
			var netErr net.Error
			if !errors.As(readErr, &netErr) || !netErr.Timeout() {
				result.OK = false
				result.Detail = proxyProbeTunnelClosed
			}
		}

		return result
	default:
		result.Detail = fmt.Sprintf("status_%d", statusCode)
	}

	return result
}

func parseProxyEndpoint(raw string) (proxyEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return proxyEndpoint{}, errors.New("empty proxy URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return proxyEndpoint{}, errors.New("invalid proxy URL")
	}

	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}

	endpoint := proxyEndpoint{
		scheme:     scheme,
		host:       net.JoinHostPort(parsed.Hostname(), port),
		serverName: parsed.Hostname(),
	}
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		token := base64.StdEncoding.EncodeToString(
			[]byte(parsed.User.Username() + ":" + password),
		)
		endpoint.authorization = "Basic " + token
	}

	return endpoint, nil
}

func proxyDialDetail(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	return "dial_failed"
}
