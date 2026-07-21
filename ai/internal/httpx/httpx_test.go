package httpx_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/ai/internal/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localConfig permits talking to httptest servers (loopback, plain HTTP).
func localConfig() httpx.Config {
	return httpx.Config{AllowHTTP: true, AllowPrivateIPs: true}
}

func passErr(status int, retryAfter time.Duration, body []byte) error {
	return fmt.Errorf("status=%d retryAfter=%s body=%s", status, retryAfter, body)
}

func TestPostJSONRoundTrip(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		assert.Equal(t, "extra", r.Header.Get("X-Custom"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"echo":"ok"}`)
	}))
	defer server.Close()

	cfg := localConfig()
	cfg.Header = http.Header{"X-Custom": []string{"extra"}}
	client := httpx.New(cfg, server.URL+"/v1")

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-test")

	var out struct {
		Echo string `json:"echo"`
	}

	raw, err := client.PostJSON(t.Context(), "chat/completions", headers, map[string]string{"model": "x"}, &out, passErr)
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Echo)
	assert.JSONEq(t, `{"echo":"ok"}`, string(raw))
}

func TestPostJSONErrorDecoding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	var (
		gotStatus int
		gotRetry  time.Duration
		gotBody   []byte
	)

	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil,
		func(status int, retryAfter time.Duration, body []byte) error {
			gotStatus, gotRetry, gotBody = status, retryAfter, body
			return errors.New("decoded")
		})
	require.EqualError(t, err, "decoded")
	assert.Equal(t, http.StatusTooManyRequests, gotStatus)
	assert.Equal(t, 7*time.Second, gotRetry)
	assert.Contains(t, string(gotBody), "slow down")
}

func TestPostStream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		assert.Equal(t, "sse", r.URL.Query().Get("alt"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: hi\n\n")
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	body, err := client.PostStream(t.Context(), "models/gemini:streamGenerateContent?alt=sse", nil, struct{}{}, passErr)
	require.NoError(t, err)

	defer func() { _ = body.Close() }()

	buf := make([]byte, 64)
	n, _ := body.Read(buf)
	assert.Equal(t, "data: hi\n\n", string(buf[:n]))
}

func TestPostStreamErrorPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"bad"}`)
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)
	_, err := client.PostStream(t.Context(), "stream", nil, struct{}{}, passErr)
	require.ErrorContains(t, err, "status=400")
	require.ErrorContains(t, err, "bad")
}

func TestSchemeRejection(t *testing.T) {
	t.Parallel()

	client := httpx.New(httpx.Config{}, "http://api.example.com/v1")
	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil, passErr)
	require.ErrorContains(t, err, "allow-HTTP")
}

func TestInvalidBaseURL(t *testing.T) {
	t.Parallel()

	client := httpx.New(httpx.Config{BaseURL: "://not a url"}, "https://fallback")
	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil, passErr)
	require.ErrorContains(t, err, "invalid base URL")
}

func TestSSRFGuardBlocksLoopback(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "{}")
	}))
	defer server.Close()

	// AllowHTTP but NOT AllowPrivateIPs: the dial guard must reject loopback.
	client := httpx.New(httpx.Config{AllowHTTP: true}, server.URL)
	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil, passErr)
	require.Error(t, err)
	assert.ErrorIs(t, err, httpx.ErrPrivateAddress)
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var targetCalls atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)

		_, _ = fmt.Fprint(w, `{"unexpected":true}`)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/private")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	client := httpx.New(localConfig(), redirect.URL)
	_, err := client.PostJSON(t.Context(), "x", nil, struct{}{}, nil, passErr)
	require.ErrorContains(t, err, "status=307")
	assert.Zero(t, targetCalls.Load())
}

func TestBaseURLPathPrefixPreserved(t *testing.T) {
	t.Parallel()

	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		_, _ = fmt.Fprint(w, "{}")
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL+"/compat/v1")
	_, err := client.PostJSON(t.Context(), "chat/completions", nil, struct{}{}, nil, passErr)
	require.NoError(t, err)
	assert.Equal(t, "/compat/v1/chat/completions", gotPath)
}
