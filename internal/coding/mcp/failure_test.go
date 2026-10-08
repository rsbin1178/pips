//nolint:wsl_v5 // Failure fixtures keep server setup and assertions adjacent.
package mcp_test

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenConnectionsNamesSafeConnectFailureCauses(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(hanging.Close)
	t.Cleanup(func() { close(release) })

	status := func(code int) string {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(code)
			_, _ = writer.Write([]byte(`{"error":"secret-server-detail"}`))
		}))
		t.Cleanup(server.Close)

		return server.URL
	}

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := "http://" + listener.Addr().String() + "/mcp"
	require.NoError(t, listener.Close())

	tests := []struct {
		id   string
		url  string
		want string
	}{
		{
			id: "hanging", url: hanging.URL,
			want: "server connection could not be initialized: timed out after 200ms; " +
				"check network access or HTTPS_PROXY (see pips doctor)",
		},
		{
			id: "unauthorized", url: status(http.StatusUnauthorized),
			want: "server connection could not be initialized: the server rejected the credentials (HTTP 401)",
		},
		{
			id: "broken", url: status(http.StatusInternalServerError),
			want: "server connection could not be initialized: the server answered HTTP 500",
		},
		{
			id: "refused", url: refused,
			want: "server connection could not be initialized: the connection was refused",
		},
	}

	resolved := make([]codingmcp.ResolvedDefinition, 0, len(tests))
	for _, test := range tests {
		definition := enabledHTTPDefinition(test.id)
		definition.Definition.URL = test.url
		definition.Definition.ConnectTimeout = 200 * time.Millisecond
		resolved = append(resolved, definition)
	}

	connections, err := codingmcp.OpenConnections(t.Context(), resolved, codingmcp.ConnectionOptions{
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		MaxTools:       16,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	started := time.Now()
	require.NoError(t, connections.Wait(t.Context()))
	assert.Less(t, time.Since(started), 3*time.Second)

	diagnostics := connections.TakeDiagnostics()
	require.Len(t, diagnostics, len(tests))
	for index, test := range tests {
		assert.Equal(t, test.id, diagnostics[index].ServerID)
		assert.Equal(t, "connect", diagnostics[index].Stage)
		assert.Equal(t, "connect_failed", diagnostics[index].Code)
		assert.Equal(t, test.want, diagnostics[index].Message)
		assert.NotContains(t, diagnostics[index].Message, "secret-server-detail")
	}
}

func TestConnectionsSettleListTimeoutBeforeReleasingSession(t *testing.T) {
	t.Parallel()

	server := testMCPServer("slow-list", "health")
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	release := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)

			return
		}
		// Hang tools/list and the cancellation that follows it, so releasing
		// the session waits on an unresponsive peer.
		if bytes.Contains(body, []byte(`"tools/list"`)) || bytes.Contains(body, []byte(`"notifications/cancelled"`)) {
			<-release

			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	definition := enabledHTTPDefinition("slow")
	definition.Definition.URL = httpServer.URL
	definition.Definition.ConnectTimeout = time.Second
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			HTTPClient:     &http.Client{Timeout: 30 * time.Second},
			MaxTools:       16,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	t.Cleanup(func() { close(release) })

	started := time.Now()
	require.NoError(t, connections.Wait(t.Context()))
	assert.Less(t, time.Since(started), 3*time.Second)

	diagnostics := connections.TakeDiagnostics()
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "list_failed", diagnostics[0].Code)
	assert.Equal(t,
		"server tools could not be listed: timed out after 1s; check network access or HTTPS_PROXY (see pips doctor)",
		diagnostics[0].Message,
	)
}
