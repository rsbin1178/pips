// Package httpx is the shared HTTP plumbing for provider adapters: a tuned
// transport, JSON request/response execution, SSE stream setup, an SSRF
// guard, and Retry-After parsing.
//
// Design notes, mirrored from the package design doc:
//
//   - No http.Client.Timeout is ever set: it would cap the total time to read
//     a response body and kill long streams. Callers bound requests with
//     context deadlines instead.
//   - A streaming request is bounded in two places instead, and both live here
//     rather than in an http.Client option so a bring-your-own client gets them
//     too. Config.ResponseHeaderTimeout caps the wait for response headers, and
//     Config.StreamIdleTimeout bounds the silence between reads on the body.
//     Neither bounds the response's total duration, and a non-streaming call
//     keeps no bound at all: it legitimately generates for minutes before its
//     headers arrive. Without them a peer that accepts a request and then goes
//     quiet parks the caller forever, and a caller parked on a read reports no
//     error for any recovery layer to act on.
//   - The transport runs its own liveness checks, so a peer that stops
//     answering is reported rather than parked: an HTTP/2 ping after a stretch
//     of silence, a close when the ping goes unanswered, and explicit TCP
//     keep-alive probes for a path that drops the connection's state. They
//     mirror what the other agent frontends configure.
//   - Every request carries a User-Agent naming this client and its build, so a
//     gateway's logs can attribute the traffic. Config.UserAgent overrides it,
//     and an adapter or Config.Header entry that sets the header wins over both.
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
	"maps"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// DefaultStreamIdleTimeout bounds the silence between reads on a streaming
// response body unless Config.StreamIdleTimeout sets one of its own. It sits
// far above any provider's legitimate pause — measured against a live gateway,
// the longest silence was 4.5s to the first byte and 1.2s between chunks — so
// it only fires on a stream that has genuinely stopped delivering.
//
// It is [ai.DefaultStreamIdleTimeout], so the transport and any caller deriving
// policy from the bound share one number.
const DefaultStreamIdleTimeout = ai.DefaultStreamIdleTimeout

// DefaultResponseHeaderTimeout caps how long a streaming call waits for its
// response headers unless Config.ResponseHeaderTimeout sets a bound of its own.
// A streaming peer returns headers in seconds — measured against a live
// gateway, in under four — so the bound only fires on a request the peer has
// parked without answering at all. Non-streaming calls keep no bound: they
// legitimately generate for minutes before their headers arrive.
const DefaultResponseHeaderTimeout = 10 * time.Minute

// Liveness settings for the built-in transport. They exist so a peer that stops
// answering, or a path that forgets the connection, is reported as an error
// within about a minute rather than parking the caller until the operating
// system gives up, which on the common defaults runs to minutes.
const (
	// DefaultSendPingTimeout is how long an HTTP/2 connection may receive no
	// frame at all before the client pings it.
	DefaultSendPingTimeout = 30 * time.Second
	// DefaultPingTimeout closes an HTTP/2 connection whose ping goes
	// unanswered.
	DefaultPingTimeout = 10 * time.Second
	// DefaultTCPKeepAliveIdle is how long a connection may sit idle before the
	// kernel sends its first keep-alive probe.
	DefaultTCPKeepAliveIdle = 30 * time.Second
	// DefaultTCPKeepAliveInterval is the gap between those probes.
	DefaultTCPKeepAliveInterval = 10 * time.Second
	// DefaultTCPKeepAliveCount is how many probes may go unanswered before the
	// kernel drops the connection.
	DefaultTCPKeepAliveCount = 3
)

// version is the build stamp the User-Agent reports. Release builds inject it
// with -X; a module-installed binary falls back to the version Go embeds.
var version = ""

// DefaultUserAgent names this client and its build on every request, so a
// gateway's logs can attribute the traffic to pips instead of to an anonymous
// HTTP client. A build that carries no version reports "pips" alone.
func DefaultUserAgent() string {
	if stamped := strings.TrimSpace(version); stamped != "" {
		return "pips/" + stamped
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		if module := strings.TrimSpace(info.Main.Version); module != "" && module != "(devel)" {
			return "pips/" + module
		}
	}

	return "pips"
}

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
	// UserAgent is sent as the request's User-Agent; empty sends
	// DefaultUserAgent. A Header entry or an adapter-set User-Agent wins over
	// both.
	UserAgent string
	// AllowHTTP permits plain-HTTP base URLs (local inference servers).
	AllowHTTP bool
	// AllowPrivateIPs disables the SSRF dial guard (local inference servers).
	AllowPrivateIPs bool
	// MaxStreamLineSize caps a single SSE line; zero selects the sse package
	// default (1 MiB).
	MaxStreamLineSize int
	// StreamIdleTimeout bounds the silence between reads on a streaming
	// response body; zero or negative selects DefaultStreamIdleTimeout. It
	// applies to every streaming call, including one made through HTTPClient:
	// the bound is not expressible as an http.Client option, so a bring-your-own
	// client cannot supply it and this field stays the override.
	StreamIdleTimeout time.Duration
	// ResponseHeaderTimeout caps the wait for response headers on a streaming
	// call; zero or negative selects DefaultResponseHeaderTimeout. Like the idle
	// bound it is not expressible as an http.Client option, so a bring-your-own
	// HTTPClient gets it too.
	ResponseHeaderTimeout time.Duration
}

// Client executes JSON and SSE requests against one provider endpoint.
// It is built once per adapter instance and is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	base       *url.URL
	header     http.Header
	userAgent  string

	maxStreamLineSize     int
	streamIdleTimeout     time.Duration
	responseHeaderTimeout time.Duration

	// initErr defers construction failures (bad base URL, forbidden scheme)
	// to the first call, letting provider constructors stay single-valued.
	initErr error
}

// New builds a Client from cfg, falling back to defaultBaseURL when
// cfg.BaseURL is empty. Construction never fails; configuration errors
// surface on the first request.
func New(cfg Config, defaultBaseURL string) *Client {
	c := &Client{
		header:                cfg.Header.Clone(),
		userAgent:             resolveUserAgent(cfg.UserAgent),
		maxStreamLineSize:     cfg.MaxStreamLineSize,
		streamIdleTimeout:     resolveStreamIdleTimeout(cfg.StreamIdleTimeout),
		responseHeaderTimeout: resolveResponseHeaderTimeout(cfg.ResponseHeaderTimeout),
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
		c.httpClient = &http.Client{
			Transport: newTransport(cfg.AllowPrivateIPs),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
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

// FormField is one text field of a multipart/form-data request.
type FormField struct {
	// Name is the multipart part name.
	Name string
	// Value is the field's text value.
	Value string
}

// FormFile is one binary part of a multipart/form-data request. Part names and
// filenames are generated by the adapter, never copied from caller input.
type FormFile struct {
	// Field is the multipart part name (for example "image[]").
	Field string
	// Name is the filename recorded in the part's Content-Disposition.
	Name string
	// ContentType is the part's media type. Empty omits the part header.
	ContentType string
	// Data is the part's body.
	Data []byte
}

// PostJSON executes a JSON POST and decodes the 2xx response body into out.
// It returns the raw response bytes for Response.Raw. Non-2xx responses are
// passed to decodeErr.
func (c *Client) PostJSON(ctx context.Context, path string, headers http.Header, body, out any, decodeErr ErrorDecoder) ([]byte, error) {
	payload, err := jsonx.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}

	return c.post(ctx, path, headers, "application/json", payload, out, decodeErr)
}

// PostMultipart executes a multipart/form-data POST and decodes the 2xx
// response body into out. The body is buffered in memory before sending so it
// can be replayed for retries; the caller bounds the payload size. Non-2xx
// responses are passed to decodeErr.
func (c *Client) PostMultipart(ctx context.Context, path string, headers http.Header, fields []FormField, files []FormFile, out any, decodeErr ErrorDecoder) ([]byte, error) {
	payload, contentType, err := encodeMultipart(fields, files)
	if err != nil {
		return nil, err
	}

	return c.post(ctx, path, headers, contentType, payload, out, decodeErr)
}

// post sends one encoded payload and decodes the 2xx response body into out.
func (c *Client) post(ctx context.Context, path string, headers http.Header, contentType string, payload []byte, out any, decodeErr ErrorDecoder) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodPost, path, headers, contentType, payload) //nolint:bodyclose // closed by readJSON
	if err != nil {
		return nil, err
	}

	return c.readJSON(resp, out, decodeErr)
}

// GetJSON executes a GET request and decodes the 2xx response body into out.
// Non-2xx responses are passed to decodeErr. Task-based provider APIs use it
// to poll a previously submitted job.
func (c *Client) GetJSON(ctx context.Context, path string, headers http.Header, out any, decodeErr ErrorDecoder) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, path, headers, "", nil) //nolint:bodyclose // closed by readJSON
	if err != nil {
		return nil, err
	}

	return c.readJSON(resp, out, decodeErr)
}

// readJSON drains and decodes one response, mapping non-2xx status through
// decodeErr.
func (c *Client) readJSON(resp *http.Response, out any, decodeErr ErrorDecoder) ([]byte, error) {
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
	payload, err := jsonx.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}

	return c.postStream(ctx, path, headers, "application/json", payload, decodeErr)
}

// PostMultipartStream executes a multipart/form-data POST expecting an SSE
// response and returns the response body ready for the sse package. Like
// [Client.PostMultipart], the body is buffered in memory before sending.
// Non-2xx responses are fully read and passed to decodeErr.
func (c *Client) PostMultipartStream(ctx context.Context, path string, headers http.Header, fields []FormField, files []FormFile, decodeErr ErrorDecoder) (io.ReadCloser, error) {
	payload, contentType, err := encodeMultipart(fields, files)
	if err != nil {
		return nil, err
	}

	return c.postStream(ctx, path, headers, contentType, payload, decodeErr)
}

// postStream sends one encoded payload and returns the SSE response body,
// guarded by the client's response-header bound and stream idle bound, so a
// peer that never answers and a body that stops delivering both fail rather
// than parking the reader.
func (c *Client) postStream(ctx context.Context, path string, headers http.Header, contentType string, payload []byte, decodeErr ErrorDecoder) (io.ReadCloser, error) {
	if headers == nil {
		headers = http.Header{}
	}

	headers = headers.Clone()
	headers.Set("Accept", "text/event-stream")

	// The wait for response headers gets a bound of its own. net/http exposes no
	// deadline for that phase, so the bound is applied by cancelling the
	// request's context. The body inherits that context, so the cancel is handed
	// to the reader and runs when the body closes.
	headerCtx, cancelHeader := context.WithCancel(ctx)

	timer := time.AfterFunc(c.responseHeaderTimeout, cancelHeader)
	resp, err := c.send(headerCtx, http.MethodPost, path, headers, contentType, payload)
	expired := !timer.Stop()

	switch {
	case err != nil:
		cancelHeader()

		if expired && ctx.Err() == nil {
			// The cause is reported as text: wrapping it would put a transport
			// deadline back into the chain, where it reads as the caller's.
			return nil, fmt.Errorf("no response headers within %s (last error: %s): %w",
				c.responseHeaderTimeout, err.Error(), ai.ErrResponseTimeout)
		}

		return nil, err
	case expired && ctx.Err() == nil:
		// The bound fired as the headers arrived, so the body's context is
		// already cancelled and the body cannot be read: report the bound.
		cancelHeader()

		_ = resp.Body.Close()

		return nil, fmt.Errorf("no response headers within %s: %w", c.responseHeaderTimeout, ai.ErrResponseTimeout)
	}

	body := &cancelOnClose{ReadCloser: resp.Body, cancel: cancelHeader}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer drainClose(body)

		raw, readErr := io.ReadAll(io.LimitReader(body, maxErrorBodySize))
		if readErr != nil {
			return nil, fmt.Errorf("reading error body (status %d): %w", resp.StatusCode, readErr)
		}

		return nil, decodeErr(resp.StatusCode, retryAfter(resp), raw)
	}

	return newIdleReadCloser(body, c.streamIdleTimeout), nil
}

// cancelOnClose hands the request context a streaming call was opened with to
// the body's reader, so that context is released when the body closes rather
// than when its headers arrive.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

// Close releases the body and the context its request was opened with.
func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()

	return err
}

// resolveStreamIdleTimeout maps the unset values callers leave behind onto the
// default, in the same shape as MaxStreamLineSize: there is no way to turn the
// bound off, because it is the only guard against a stream that never fails.
func resolveStreamIdleTimeout(configured time.Duration) time.Duration {
	if configured <= 0 {
		return DefaultStreamIdleTimeout
	}

	return configured
}

// resolveResponseHeaderTimeout maps the unset values callers leave behind onto
// the default. Like the idle bound it cannot be switched off: a peer that
// accepts a request and never answers is the failure nothing else reaches.
func resolveResponseHeaderTimeout(configured time.Duration) time.Duration {
	if configured <= 0 {
		return DefaultResponseHeaderTimeout
	}

	return configured
}

// resolveUserAgent maps an unset User-Agent onto this client's own name.
func resolveUserAgent(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}

	return DefaultUserAgent()
}

// idleReadCloser fails a read that stays blocked longer than its bound, and
// closes the body to unblock it: net/http exposes no read deadline on a
// response body, on the HTTP/1 or the HTTP/2 path, so closing is the only way
// to end a read that is already parked. The read then reports
// [ai.ErrStreamIdle] in place of the close error, which keeps the abort inside
// the retryable failure classes the recovery layers act on.
//
// The bound is measured per read, never as a total, so a stream that keeps
// producing is never aborted however long it runs. Each read arms its own timer
// carrying that read's generation and retires the generation when the read
// returns, so a timer that has not fired by then becomes a no-op. One window
// remains: a callback can land in the few instructions between the read
// returning and the retire, so a read finishing within that window of the
// deadline can still be reported idle. At the default bound that means a read
// returning ten minutes in, and the cost is one re-issued turn.
type idleReadCloser struct {
	body    io.ReadCloser
	timeout time.Duration

	mu      sync.Mutex
	gen     uint64
	expired bool
	closed  bool
}

// newIdleReadCloser guards body with timeout. A non-positive timeout returns
// body unguarded; callers resolve an unset timeout first.
func newIdleReadCloser(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}

	return &idleReadCloser{body: body, timeout: timeout}
}

// Read reads one chunk, aborting the read when it outlasts the idle bound.
func (r *idleReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()

	if r.closed {
		r.mu.Unlock()

		return r.body.Read(p)
	}

	r.gen++
	gen := r.gen

	r.mu.Unlock()

	timer := time.AfterFunc(r.timeout, func() { r.expire(gen) })

	n, err := r.body.Read(p)

	timer.Stop()

	r.mu.Lock()
	r.gen++
	expired := r.expired
	r.mu.Unlock()

	if expired {
		return n, fmt.Errorf("no data for %s: %w", r.timeout, ai.ErrStreamIdle)
	}

	return n, err
}

// Close releases the body and stops the bound, so a closed stream is never
// reported as idle.
func (r *idleReadCloser) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	return r.body.Close()
}

// expire ends the read that armed it, when that read is still in flight.
func (r *idleReadCloser) expire(gen uint64) {
	r.mu.Lock()

	current := r.gen == gen && !r.closed && !r.expired
	if current {
		r.expired = true
	}
	r.mu.Unlock()

	if !current {
		return
	}

	_ = r.body.Close()
}

// encodeMultipart renders fields and files into a multipart/form-data body and
// its Content-Type header value.
func encodeMultipart(fields []FormField, files []FormFile) ([]byte, string, error) {
	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)

	for _, field := range fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			return nil, "", fmt.Errorf("encoding multipart field %q: %w", field.Name, err)
		}
	}

	for _, file := range files {
		if err := writeFormFile(writer, file); err != nil {
			return nil, "", err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("encoding multipart body: %w", err)
	}

	return buf.Bytes(), writer.FormDataContentType(), nil
}

func writeFormFile(writer *multipart.Writer, file FormFile) error {
	disposition := mime.FormatMediaType("form-data", map[string]string{
		"name":     file.Field,
		"filename": file.Name,
	})
	if disposition == "" {
		return fmt.Errorf("encoding multipart part %q: invalid name or filename", file.Field)
	}

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", disposition)

	if file.ContentType != "" {
		header.Set("Content-Type", file.ContentType)
	}

	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("encoding multipart part %q: %w", file.Field, err)
	}

	if _, err := part.Write(file.Data); err != nil {
		return fmt.Errorf("encoding multipart part %q: %w", file.Field, err)
	}

	return nil
}

func (c *Client) send(ctx context.Context, method, path string, headers http.Header, contentType string, payload []byte) (*http.Response, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}

	u := c.base.JoinPath(path)
	// Preserve query parameters passed in the path (Gemini's ?alt=sse).
	if rawPath, query, ok := splitQuery(path); ok {
		u = c.base.JoinPath(rawPath)
		u.RawQuery = query
	}

	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	maps.Copy(req.Header, headers)
	// Config-level headers win over adapter headers.
	maps.Copy(req.Header, c.header)

	// The client names itself unless the caller already did: an adapter's own
	// header or a Config.Header entry wins over the default.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
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
// for why no overall timeout appears here and which liveness settings it
// carries.
func newTransport(allowPrivateIPs bool) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newDialer(allowPrivateIPs).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// A connection that receives no frame for a stretch of silence is
		// pinged, and closed when the ping goes unanswered. Without this the
		// caller waits for the operating system to notice, which runs to
		// minutes.
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: DefaultSendPingTimeout,
			PingTimeout:     DefaultPingTimeout,
		},
	}
}

// newDialer builds the dialer the transport dials with: a bounded connect, and
// keep-alive probes configured explicitly rather than left at the operating
// system's defaults.
func newDialer(allowPrivateIPs bool) *net.Dialer {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     DefaultTCPKeepAliveIdle,
			Interval: DefaultTCPKeepAliveInterval,
			Count:    DefaultTCPKeepAliveCount,
		},
	}
	if !allowPrivateIPs {
		dialer.ControlContext = guardControl
	}

	return dialer
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
