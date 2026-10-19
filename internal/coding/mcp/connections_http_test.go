//nolint:wsl_v5 // HTTP recovery fixtures keep server, client, and assertion steps adjacent.
package mcp_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serverCloser struct {
	close func() error
}

func (c serverCloser) Close() error { return c.close() }

func TestHTTPConnectionsRecoverAfterTransportFailure(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		servers []*httptest.Server
	)

	factory := func(context.Context, codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		server := testMCPServer("recover", "lookup")
		handler := sdk.NewStreamableHTTPHandler(
			func(*http.Request) *sdk.Server { return server },
			&sdk.StreamableHTTPOptions{JSONResponse: true},
		)
		httpServer := httptest.NewServer(handler)

		mu.Lock()
		servers = append(servers, httpServer)
		mu.Unlock()

		return &sdk.StreamableClientTransport{
			Endpoint:             httpServer.URL,
			HTTPClient:           &http.Client{Timeout: 30 * time.Second},
			DisableStandaloneSSE: true,
		}, serverCloser{close: func() error {
			httpServer.Close()

			return nil
		}}, nil
	}

	definition := enabledHTTPDefinition("recover")
	definition.Definition.URL = "https://recover.example.test/mcp"
	definition.Definition.ConnectTimeout = 5 * time.Second
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(t.Context()))
	entries := connections.Entries(codingmcp.VisibilityAmbient)
	require.Len(t, entries, 1)

	mu.Lock()
	first := servers[0]
	mu.Unlock()
	first.CloseClientConnections()
	first.Close()

	// The closed transport fails the next call; that wakes the session
	// watcher, which rebuilds the session against a fresh server.
	callCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	_, _ = entries[0].Tool.Exec(callCtx, agent.ToolCall{
		ID: "call", Name: entries[0].Tool.Decl().Name, Args: ai.JSON(`{}`),
	})
	cancel()

	require.Eventually(t, func() bool {
		mu.Lock()
		count := len(servers)
		mu.Unlock()
		return count >= 2
	}, 30*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		status := connections.Servers()
		return len(status) == 1 && status[0].State == codingmcp.ServerStateConnected
	}, 30*time.Second, 10*time.Millisecond)

	entries = connections.Entries(codingmcp.VisibilityAmbient)
	require.Len(t, entries, 1)
	parts, err := entries[0].Tool.Exec(t.Context(), agent.ToolCall{
		ID: "call", Name: entries[0].Tool.Decl().Name, Args: ai.JSON(`{}`),
	})
	require.NoError(t, err)
	assert.NotNil(t, parts)
}
