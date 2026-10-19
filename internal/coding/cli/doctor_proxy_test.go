//nolint:wsl_v5 // Proxy probe fixtures keep listener, response, and assertion steps adjacent.
package cli

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listenProxy(t *testing.T) net.Listener {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	return listener
}

func TestNativeProxyProbeAcceptsAConnectTunnel(t *testing.T) {
	t.Parallel()

	listener := listenProxy(t)

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		<-time.After(500 * time.Millisecond)
	}()

	result := nativeProxyProbe(t.Context(), "http://"+listener.Addr().String())
	assert.True(t, result.OK)
	assert.Empty(t, result.Detail)
	assert.Positive(t, result.Latency)
}

func TestNativeProxyProbeSendsProxyAuthorization(t *testing.T) {
	t.Parallel()

	listener := listenProxy(t)
	authorization := make(chan string, 1)

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			if strings.HasPrefix(strings.ToLower(line), "proxy-authorization:") {
				authorization <- strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		<-time.After(500 * time.Millisecond)
	}()

	result := nativeProxyProbe(
		t.Context(), "http://user:secret@"+listener.Addr().String(),
	)
	assert.True(t, result.OK)

	select {
	case value := <-authorization:
		assert.Equal(t, "Basic dXNlcjpzZWNyZXQ=", value)
	case <-time.After(30 * time.Second):
		t.Fatal("proxy authorization header was not observed")
	}
}

func TestNativeProxyProbeDetectsImmediateTunnelReset(t *testing.T) {
	t.Parallel()

	listener := listenProxy(t)

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}()

	result := nativeProxyProbe(t.Context(), "http://"+listener.Addr().String())
	assert.False(t, result.OK)
	assert.Equal(t, "tunnel_closed", result.Detail)
}

func TestNativeProxyProbeReportsTimeoutAndDialFailure(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		listener := listenProxy(t)

		go func() {
			conn, acceptErr := listener.Accept()
			if acceptErr == nil {
				<-time.After(5 * time.Second)
				_ = conn.Close()
			}
		}()

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		result := nativeProxyProbe(ctx, listener.Addr().String())
		assert.False(t, result.OK)
		assert.Equal(t, "no_response", result.Detail)
	})

	t.Run("dial failure", func(t *testing.T) {
		t.Parallel()

		listener := listenProxy(t)
		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		result := nativeProxyProbe(t.Context(), address)
		assert.False(t, result.OK)
		assert.Equal(t, "dial_failed", result.Detail)
	})

	t.Run("invalid URL", func(t *testing.T) {
		t.Parallel()

		result := nativeProxyProbe(t.Context(), "://")
		assert.False(t, result.OK)
		assert.Equal(t, "invalid_proxy_url", result.Detail)
	})
}
