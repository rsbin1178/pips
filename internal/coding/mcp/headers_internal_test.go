package mcp

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type headerCaptureTransport func(*http.Request) (*http.Response, error)

func (f headerCaptureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestConfiguredHeadersAreAttachedOnlyToConfiguredOrigin(t *testing.T) {
	t.Parallel()

	requests := make(chan http.Header, 2)
	base := &http.Client{
		Timeout: time.Second,
		Transport: headerCaptureTransport(func(request *http.Request) (*http.Response, error) {
			requests <- request.Header.Clone()

			return nil, assert.AnError
		}),
	}
	client, err := definitionHTTPClient(base, Definition{
		URL: "https://example.com/mcp",
		Headers: []HTTPHeader{
			{Name: "X-Plugin", Value: "configured"},
			{Name: "Content-Type", Value: "configured"},
		},
	})
	require.NoError(t, err)

	sameRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/other", nil)
	require.NoError(t, err)
	sameRequest.Header.Set("Content-Type", "client")

	response, _ := client.Transport.RoundTrip(sameRequest)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}

	sameHeaders := <-requests
	assert.Equal(t, "configured", sameHeaders.Get("X-Plugin"))
	assert.Equal(t, "client", sameHeaders.Get("Content-Type"))

	otherRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://other.example/mcp", nil)
	require.NoError(t, err)

	response, _ = client.Transport.RoundTrip(otherRequest)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}

	otherHeaders := <-requests
	assert.Empty(t, otherHeaders.Get("X-Plugin"))
}
