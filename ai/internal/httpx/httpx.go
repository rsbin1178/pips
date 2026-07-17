// Package httpx is the shared HTTP plumbing for provider adapters: a tuned
// transport, JSON request/response execution, SSE stream setup, an SSRF
// guard, and Retry-After parsing.
//
// Design notes, mirrored from the package design doc:
//
//   - No http.Client.Timeout is ever set: it would cap the total time to read
//     a response body and kill long streams. Callers bound requests with
//     context deadlines instead.
//   - ResponseHeaderTimeout is likewise unset because non-streaming LLM calls
//     legitimately spend minutes before the first response byte.
//   - By default the client refuses plain HTTP and connections to private,
//     loopback, link-local, or unspecified addresses (SSRF guard). Local
//     endpoints opt out via Config.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/rsbin/pips/ai/internal/jsonx"
)

// Config carries the transport-level options every provider constructor
// accepts. Provider option funcs write into it.
type Config struct {
	// APIKey is the provider credential. How it is sent (bearer header,
	// x-api-key, x-goog-api-key) is up to the adapter's Authorize hook.
	APIKey string
	// BaseURL overrides the provider's default endpoint. It should include
	// any version prefix (for example "https://api.openai.com/v1").
	BaseURL string
	// HTTPClient replaces the built-in client entirely. The bring-your-own
	// client is used as-is: transport tuning and the SSRF dial guard are the
	// owner's responsibility. Scheme checks still apply.
	HTTPClient *http.Client
	// Header is applied to every request. Adapter-set headers are written
	// first, so Header entries override them on key collision.
	Header http.Header
	// AllowHTTP permits plain-HTTP base URLs (local inference servers).
	AllowHTTP bool
	// AllowPrivateIPs disables the SSRF dial guard (local inference servers).
	AllowPrivateIPs bool
	// MaxStreamLineSize caps a single SSE line; zero selects the sse package
	// default (1 MiB).
	MaxStreamLineSize int
}

// Client executes JSON and SSE requests against one provider endpoint.
// It is built once per adapter instance and is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	base       *url.URL
	header     http.Header

	maxStreamLineSize int

	// initErr defers construction failures (bad base URL, forbidden scheme)
	// to the first call, letting provider constructors stay single-valued.
	initErr error
}

// New builds a Client from cfg, falling back to defaultBaseURL when
// cfg.BaseURL is empty. Construction never fails; configuration errors
// surface on the first request.
func New(cfg Config, defaultBaseURL string) *Client {
	c := &Client{
		header:            cfg.Header.Clone(),
		maxStreamLineSize: cfg.MaxStreamLineSize,
	}

	rawURL := cfg.BaseURL
	if rawURL == "" {
		rawURL = defaultBaseURL
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		c.initErr = fmt.Errorf("invalid base URL %q: %w", rawURL, err)
		return c
	}
	switch {
	case base.Scheme == "https":
	case base.Scheme == "http" && cfg.AllowHTTP:
	case base.Scheme == "http":
		c.initErr = fmt.Errorf("plain-HTTP base URL %q requires the allow-HTTP option", rawURL)
		return c
	default:
		c.initErr = fmt.Errorf("unsupported base URL scheme %q", base.Scheme)
		return c
	}
	c.base = base

	if cfg.HTTPClient != nil {
		c.httpClient = cfg.HTTPClient
	} else {
		c.httpClient = &http.Client{Transport: newTransport(cfg.AllowPrivateIPs)}
	}
	return c
}

// MaxStreamLineSize exposes the configured SSE line ceiling for adapters.
func (c *Client) MaxStreamLineSize() int {
	return c.maxStreamLineSize
}

// ErrorDecoder turns a non-2xx response into an error. Adapters parse their
// provider's error body shape and build an *ai.Error; retryAfter is the
// parsed Retry-After header (zero when absent).
type ErrorDecoder func(status int, retryAfter time.Duration, body []byte) error

// maxErrorBodySize caps how much of an error response is read into memory.
const maxErrorBodySize = 1 << 20

// PostJSON executes a JSON POST and decodes the 2xx response body into out.
// It returns the raw response bytes for Response.Raw. Non-2xx responses are
// passed to decodeErr.
func (c *Client) PostJSON(ctx context.Context, path string, headers http.Header, body, out any, decodeErr ErrorDecoder) ([]byte, error) {
	resp, err := c.send(ctx, path, headers, body)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeErr(resp.StatusCode, retryAfter(resp), raw)
	}
	if out != nil {
		if err := jsonx.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decoding response body: %w", err)
		}
	}
	return raw, nil
}

// PostStream executes a JSON POST expecting an SSE response and returns the
// response body ready for the sse package. Non-2xx responses are fully read
// and passed to decodeErr, so stream setup failures carry provider error
// details.
func (c *Client) PostStream(ctx context.Context, path string, headers http.Header, body any, decodeErr ErrorDecoder) (io.ReadCloser, error) {
	if headers == nil {
		headers = http.Header{}
	}
	headers = headers.Clone()
	headers.Set("Accept", "text/event-stream")

	resp, err := c.send(ctx, path, headers, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer drainClose(resp.Body)
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		if readErr != nil {
			return nil, fmt.Errorf("reading error body (status %d): %w", resp.StatusCode, readErr)
		}
		return nil, decodeErr(resp.StatusCode, retryAfter(resp), raw)
	}
	return resp.Body, nil
}

func (c *Client) send(ctx context.Context, path string, headers http.Header, body any) (*http.Response, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}

	u := c.base.JoinPath(path)
	// Preserve query parameters passed in the path (Gemini's ?alt=sse).
	if rawPath, query, ok := splitQuery(path); ok {
		u = c.base.JoinPath(rawPath)
		u.RawQuery = query
	}

	payload, err := jsonx.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, values := range headers {
		req.Header[key] = values
	}
	// Config-level headers win over adapter headers.
	for key, values := range c.header {
		req.Header[key] = values
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func splitQuery(path string) (rawPath, query string, ok bool) {
	for i := range len(path) {
		if path[i] == '?' {
			return path[:i], path[i+1:], true
		}
	}
	return path, "", false
}

// retryAfter parses a Retry-After header as delay seconds or an HTTP date.
func retryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// drainClose drains and closes a response body so the connection is reusable.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxErrorBodySize))
	_ = body.Close()
}

// newTransport builds the tuned default transport. See the package comment
// for why no header/overall timeouts appear here.
func newTransport(allowPrivateIPs bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if !allowPrivateIPs {
		dialer.ControlContext = guardControl
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// ErrPrivateAddress is returned (wrapped) when the SSRF guard blocks a dial
// to a non-public address.
var ErrPrivateAddress = errors.New("dial to private, loopback, or link-local address blocked (enable the allow-private-IPs option for local endpoints)")

// guardControl runs after DNS resolution for every connection attempt and
// rejects non-public destinations, defeating DNS-rebinding tricks.
func guardControl(_ context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf guard: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf guard: unexpected non-IP address %q", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("ssrf guard: %s: %w", ip, ErrPrivateAddress)
	}
	return nil
}
