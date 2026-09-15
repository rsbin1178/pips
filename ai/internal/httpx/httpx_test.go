package httpx_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai/internal/httpx"
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

func TestPostMultipart(t *testing.T) {
	t.Parallel()

	type partInfo struct {
		field       string
		filename    string
		contentType string
		body        string
	}

	var (
		gotContentType string
		gotParts       []partInfo
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")

		reader, err := r.MultipartReader()
		assert.NoError(t, err)

		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}

			assert.NoError(t, partErr)

			data, readErr := io.ReadAll(part)
			assert.NoError(t, readErr)

			gotParts = append(gotParts, partInfo{
				field:       part.FormName(),
				filename:    part.FileName(),
				contentType: part.Header.Get("Content-Type"),
				body:        string(data),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"b64_json":"QUJD"}]}`)
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	fields := []httpx.FormField{{Name: "model", Value: "gpt-image-1"}, {Name: "n", Value: "2"}}
	files := []httpx.FormFile{
		{Field: "image[]", Name: "image-0.png", ContentType: "image/png", Data: []byte("first")},
		{Field: "image[]", Name: "image-1.webp", ContentType: "image/webp", Data: []byte("second")},
		{Field: "mask", Name: "mask.png", ContentType: "image/png", Data: []byte("mask")},
	}

	var out struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}

	raw, err := client.PostMultipart(t.Context(), "images/edits", nil, fields, files, &out, passErr)
	require.NoError(t, err)
	assert.JSONEq(t, `{"data":[{"b64_json":"QUJD"}]}`, string(raw))
	assert.Equal(t, "QUJD", out.Data[0].B64JSON)
	assert.Contains(t, gotContentType, "multipart/form-data")

	require.Len(t, gotParts, 5)
	assert.Equal(t, partInfo{field: "model", body: "gpt-image-1"}, gotParts[0])
	assert.Equal(t, partInfo{field: "n", body: "2"}, gotParts[1])
	assert.Equal(t, partInfo{
		field: "image[]", filename: "image-0.png", contentType: "image/png", body: "first",
	}, gotParts[2])
	assert.Equal(t, partInfo{
		field: "image[]", filename: "image-1.webp", contentType: "image/webp", body: "second",
	}, gotParts[3])
	assert.Equal(t, partInfo{
		field: "mask", filename: "mask.png", contentType: "image/png", body: "mask",
	}, gotParts[4])
}

func TestPostMultipartErrorDecoding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	var (
		gotStatus int
		gotRetry  time.Duration
		gotBody   []byte
	)

	_, err := client.PostMultipart(t.Context(), "images/edits", nil, nil, nil, nil,
		func(status int, retryAfter time.Duration, body []byte) error {
			gotStatus, gotRetry, gotBody = status, retryAfter, body
			return errors.New("decoded")
		})
	require.EqualError(t, err, "decoded")
	assert.Equal(t, http.StatusServiceUnavailable, gotStatus)
	assert.Equal(t, 9*time.Second, gotRetry)
	assert.Contains(t, string(gotBody), "overloaded")
}

func TestPostMultipartStream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		reader, err := r.MultipartReader()
		assert.NoError(t, err)

		part, partErr := reader.NextPart()
		assert.NoError(t, partErr)
		assert.Equal(t, "stream", part.FormName())

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: image_edit.completed\ndata: {\"type\":\"image_edit.completed\"}\n\n")
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	body, err := client.PostMultipartStream(t.Context(), "images/edits", nil,
		[]httpx.FormField{{Name: "stream", Value: "true"}}, nil, passErr)
	require.NoError(t, err)

	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "image_edit.completed")
}

func TestPostMultipartStreamErrorPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"bad image"}`)
	}))
	defer server.Close()

	client := httpx.New(localConfig(), server.URL)

	_, err := client.PostMultipartStream(t.Context(), "images/edits", nil, nil, nil, passErr)
	require.ErrorContains(t, err, "status=400")
	require.ErrorContains(t, err, "bad image")
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
