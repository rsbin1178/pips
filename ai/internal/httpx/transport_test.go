package httpx

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localConfig permits talking to httptest servers (loopback, plain HTTP).
func localConfig() Config {
	return Config{AllowHTTP: true, AllowPrivateIPs: true}
}

func TestDefaultUserAgentNamesTheClient(t *testing.T) {
	t.Parallel()

	agent := DefaultUserAgent()
	t.Logf("DefaultUserAgent() = %q", agent)

	assert.True(t, strings.HasPrefix(agent, "pips"), "the client names itself: %q", agent)
	assert.NotContains(t, agent, " ", "a User-Agent carries no spaces: %q", agent)
	assert.NotContains(t, agent, "Go-http-client", "the default client name is replaced: %q", agent)
}

func TestResolveUserAgent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unset selects the client name", "", DefaultUserAgent()},
		{"blank selects the client name", "   ", DefaultUserAgent()},
		{"explicit value wins", "gateway-probe/1", "gateway-probe/1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, resolveUserAgent(tc.in))
		})
	}
}

func TestResolveResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero selects the default", 0, DefaultResponseHeaderTimeout},
		{"negative selects the default", -time.Second, DefaultResponseHeaderTimeout},
		{"explicit value wins", 90 * time.Second, 90 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, resolveResponseHeaderTimeout(tc.in))
		})
	}
}

// TestDefaultTransportCarriesLivenessSettings pins the transport-level bounds
// a peer that stops answering is caught by: the HTTP/2 pings and the explicit
// TCP keep-alive probes.
func TestDefaultTransportCarriesLivenessSettings(t *testing.T) {
	t.Parallel()

	client := New(Config{}, "https://example.test/v1")

	transport, ok := client.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "the built-in client dials through the tuned transport")

	require.NotNil(t, transport.HTTP2, "HTTP/2 liveness settings are configured")
	assert.Equal(t, DefaultSendPingTimeout, transport.HTTP2.SendPingTimeout)
	assert.Equal(t, DefaultPingTimeout, transport.HTTP2.PingTimeout)

	dialer := newDialer(true)
	assert.Equal(t, net.KeepAliveConfig{
		Enable:   true,
		Idle:     DefaultTCPKeepAliveIdle,
		Interval: DefaultTCPKeepAliveInterval,
		Count:    DefaultTCPKeepAliveCount,
	}, dialer.KeepAliveConfig)
	assert.Nil(t, dialer.ControlContext, "an allowed private address needs no guard")

	assert.NotNil(t, newDialer(false).ControlContext, "the SSRF guard stays installed by default")
}

// TestResponseHeaderTimeoutLeavesNonStreamingCallsAlone pins the bound's scope:
// a non-streaming call generates for minutes before its headers arrive, so it
// keeps no bound of its own.
func TestResponseHeaderTimeoutLeavesNonStreamingCallsAlone(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)

		_, _ = fmt.Fprint(w, `{"echo":"ok"}`)
	}))
	defer server.Close()

	cfg := localConfig()
	cfg.ResponseHeaderTimeout = 50 * time.Millisecond
	client := New(cfg, server.URL)

	var out struct {
		Echo string `json:"echo"`
	}

	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, &out, func(int, time.Duration, []byte) error {
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Echo)
}

func TestRequestCarriesTheUserAgent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config func(*Config)
		want   func() string
	}{
		{
			name:   "the client names itself",
			config: func(*Config) {},
			want:   DefaultUserAgent,
		},
		{
			name:   "an explicit config value wins",
			config: func(cfg *Config) { cfg.UserAgent = "gateway-probe/1" },
			want:   func() string { return "gateway-probe/1" },
		},
		{
			name:   "a config header wins over both",
			config: func(cfg *Config) { cfg.Header = http.Header{"User-Agent": []string{"header-agent/2"}} },
			want:   func() string { return "header-agent/2" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seen := make(chan string, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get("User-Agent")

				_, _ = fmt.Fprint(w, `{}`)
			}))
			defer server.Close()

			cfg := localConfig()
			tc.config(&cfg)

			client := New(cfg, server.URL)

			_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil, func(int, time.Duration, []byte) error {
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want(), <-seen)
		})
	}
}

// parkedServer accepts a request and never answers it, which is the shape the
// response-header bound exists for. The handler is released before the server
// closes so Close does not wait on it.
func parkedServer(t *testing.T) *httptest.Server {
	t.Helper()

	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))

	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	return server
}

func noErrorDecoder(int, time.Duration, []byte) error {
	return nil
}

// TestResponseHeaderTimeoutAbortsAParkedStream covers the failure the bound
// exists for: a peer that accepts a streaming request and never answers. The
// abort is a class of its own, so it stays inside the retryable classes rather
// than looking like a deadline the caller set.
func TestResponseHeaderTimeoutAbortsAParkedStream(t *testing.T) {
	t.Parallel()

	server := parkedServer(t)

	cfg := localConfig()
	cfg.ResponseHeaderTimeout = 100 * time.Millisecond
	client := New(cfg, server.URL)

	started := time.Now()
	body, err := client.PostStream(t.Context(), "x", nil, struct{}{}, noErrorDecoder)
	elapsed := time.Since(started)

	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrResponseTimeout, "the abort names its own class: %v", err)
	assert.Nil(t, body)
	assert.Contains(t, err.Error(), "no response headers within")
	assert.Less(t, elapsed, 5*time.Second, "the bound, not the caller's patience, ends the wait")
	assert.True(t, ai.IsRetryable(err), "a parked request is retryable: %v", err)
}

// TestResponseHeaderTimeoutAppliesToABringYourOwnClient keeps the bound with
// the client rather than the transport: a caller-supplied http.Client carries
// no such option, so it must not lose the guard.
func TestResponseHeaderTimeoutAppliesToABringYourOwnClient(t *testing.T) {
	t.Parallel()

	server := parkedServer(t)

	cfg := localConfig()
	cfg.HTTPClient = &http.Client{Transport: &http.Transport{}}
	cfg.ResponseHeaderTimeout = 100 * time.Millisecond
	client := New(cfg, server.URL)

	_, err := client.PostStream(t.Context(), "x", nil, struct{}{}, noErrorDecoder)
	require.ErrorIs(t, err, ai.ErrResponseTimeout)
}

// TestResponseHeaderTimeoutLeavesTheBodyAlone guards the bound's scope: it
// covers the wait for headers only, so a body that keeps delivering is read to
// the end however long it takes.
func TestResponseHeaderTimeoutLeavesTheBodyAlone(t *testing.T) {
	t.Parallel()

	const (
		headerBound = 50 * time.Millisecond
		gap         = 200 * time.Millisecond
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprint(w, "data: one\n\n")

		flusher, ok := w.(http.Flusher)
		if !ok {
			panic("httptest writer does not implement http.Flusher")
		}

		flusher.Flush()

		time.Sleep(gap)

		_, _ = fmt.Fprint(w, "data: two\n\n")

		flusher.Flush()
	}))
	defer server.Close()

	cfg := localConfig()
	cfg.ResponseHeaderTimeout = headerBound
	client := New(cfg, server.URL)

	body, err := client.PostStream(t.Context(), "x", nil, struct{}{}, noErrorDecoder)
	require.NoError(t, err)

	defer func() { _ = body.Close() }()

	want := "data: one\n\ndata: two\n\n"

	// A read may hand back its last bytes together with EOF, so the loop stops
	// at either the full body or the end of the stream.
	buf := make([]byte, 0, len(want))

	deadline := time.Now().Add(10 * time.Second)
	for len(buf) < len(want) && time.Now().Before(deadline) {
		chunk := make([]byte, 64)

		n, readErr := body.Read(chunk)
		buf = append(buf, chunk[:n]...)

		if readErr != nil {
			require.ErrorIs(t, readErr, io.EOF, "the body is read past the header bound")

			break
		}
	}

	assert.Equal(t, want, string(buf))
}
