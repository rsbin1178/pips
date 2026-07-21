package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent"
	agentmcp "github.com/rsbin/pips/agent/mcp"
	"github.com/rsbin/pips/ai"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var runCodingStdioHelper = flag.Bool("pips-coding-mcp-stdio-helper", false, "run MCP stdio test helper")

func TestCodingMCPStdioHelper(t *testing.T) {
	t.Parallel()

	if !*runCodingStdioHelper {
		return
	}

	server := sdk.NewServer(&sdk.Implementation{Name: "stdio-helper", Version: "v1"}, nil)
	server.AddTool(testSDKTool("environment"), func(
		context.Context,
		*sdk.CallToolRequest,
	) (*sdk.CallToolResult, error) {
		for _, name := range []string{
			"API_KEY", "HTTPS_PROXY", "OTEL_HEADERS", "SSH_AUTH_SOCK", "PIPS_HOME",
		} {
			if os.Getenv(name) != "" {
				return nil, fmt.Errorf("unsafe environment inherited: %s", name)
			}
		}

		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "clean"},
		}}, nil
	})

	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(2)
	}

	os.Exit(0)
}

func TestOpenConnectionsDegradesPerServerAndClosesInReverse(t *testing.T) {
	t.Parallel()

	servers := map[string]*sdk.Server{
		"one":      testMCPServer("one", "lookup"),
		"two":      testMCPServer("two", "search"),
		"bad-list": sdk.NewServer(&sdk.Implementation{Name: "bad", Version: "v1"}, nil),
	}
	for index := range 17 {
		servers["bad-list"].AddTool(testSDKTool(fmt.Sprintf("tool-%d", index)), nil)
	}

	var (
		mu         sync.Mutex
		closeOrder []string
	)

	factory := func(
		ctx context.Context,
		definition codingmcp.Definition,
	) (sdk.Transport, io.Closer, error) {
		if definition.ID == "bad-connect" {
			return nil, nil, errors.New("sentinel connect setup")
		}

		serverTransport, clientTransport := sdk.NewInMemoryTransports()

		session, err := servers[definition.ID].Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}

		return clientTransport, closeRecorder{
			close: session.Close,
			record: func() {
				mu.Lock()

				closeOrder = append(closeOrder, definition.ID)
				mu.Unlock()
			},
		}, nil
	}

	resolved := []codingmcp.ResolvedDefinition{
		enabledHTTPDefinition("one"),
		enabledHTTPDefinition("bad-connect"),
		enabledHTTPDefinition("bad-list"),
		enabledHTTPDefinition("two"),
		{Definition: enabledHTTPDefinition("disabled").Definition, Status: codingmcp.StatusDisabled},
	}

	connections, err := codingmcp.OpenConnections(t.Context(), resolved, codingmcp.ConnectionOptions{
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		MaxTools:       16,
		Transport:      factory,
	})
	require.NoError(t, err)

	snapshot := connections.Snapshot()
	assert.Equal(t, uint64(1), snapshot.Version)
	require.Len(t, snapshot.Entries, 2)
	assert.Equal(t, "one_lookup", snapshot.Entries[0].Tool.Decl().Name)
	assert.Equal(t, "two_search", snapshot.Entries[1].Tool.Decl().Name)

	diagnostics := connections.Diagnostics()
	require.Len(t, diagnostics, 2)
	assert.Equal(t, "bad-connect", diagnostics[0].ServerID)
	assert.Equal(t, "transport_failed", diagnostics[0].Code)
	assert.Equal(t, "bad-list", diagnostics[1].ServerID)
	assert.Equal(t, "list_failed", diagnostics[1].Code)

	for _, diagnostic := range diagnostics {
		assert.NotContains(t, diagnostic.Message, "sentinel")
	}

	require.NoError(t, connections.Close())
	require.NoError(t, connections.Close())
	mu.Lock()
	assert.Equal(t, []string{"bad-list", "two", "one"}, closeOrder)
	mu.Unlock()
}

func TestConnectionsRefreshChangedPreservesPriorSnapshotAndCoalescesFailure(t *testing.T) {
	t.Parallel()

	server := testMCPServer("dynamic", "initial")
	factory := func(ctx context.Context, _ codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := server.Connect(ctx, serverTransport, nil)

		return clientTransport, session, err
	}

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{enabledHTTPDefinition("dynamic")},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16,
			Transport:      factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	initial := connections.Snapshot()

	for index := range 16 {
		server.AddTool(testSDKTool(fmt.Sprintf("overflow-%d", index)), nil)
	}

	var (
		failed     agentmcp.RegistrySnapshot
		attempted  bool
		refreshErr error
	)

	require.Eventually(t, func() bool {
		failed, attempted, refreshErr = connections.RefreshChanged(t.Context())

		return attempted
	}, time.Second, 10*time.Millisecond)
	require.Error(t, refreshErr)
	assert.True(t, attempted)
	assert.Equal(t, initial.Version, failed.Version)
	assert.Equal(t, initial.Version, connections.Snapshot().Version)
	require.Len(t, connections.Diagnostics(), 1)

	server.AddTool(testSDKTool("also-overflow"), nil)
	require.Eventually(t, func() bool {
		_, attempted, refreshErr = connections.RefreshChanged(t.Context())

		return attempted
	}, time.Second, 10*time.Millisecond)
	require.Error(t, refreshErr)
	assert.True(t, attempted)
	assert.Len(t, connections.Diagnostics(), 1)
}

func TestOpenConnectionsWithNoEnabledServersCreatesUsableEmptyRegistry(t *testing.T) {
	t.Parallel()

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		nil,
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	assert.Equal(t, uint64(1), connections.Snapshot().Version)
	assert.Empty(t, connections.Snapshot().Entries)
}

func TestOpenConnectionsUsesDefaultStreamableHTTPTransport(t *testing.T) {
	t.Parallel()

	server := testMCPServer("http", "health")
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	definition := enabledHTTPDefinition("remote")
	definition.Definition.URL = httpServer.URL
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			HTTPClient:     &http.Client{Timeout: 5 * time.Second},
			MaxTools:       16,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	require.Len(t, connections.Snapshot().Entries, 1)
	assert.Equal(t, "remote_health", connections.Snapshot().Entries[0].Tool.Decl().Name)
}

func TestOpenConnectionsUsesDefaultStdioTransportWithoutAmbientSecrets(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)
	opened, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tempRoot := t.TempDir()
	require.NoError(t, os.Chmod(tempRoot, 0o700)) //nolint:gosec // Directories require owner traversal.

	parent := map[string]string{
		"API_KEY": "sentinel-api-key", "HTTPS_PROXY": "sentinel-proxy",
		"OTEL_HEADERS": "sentinel-otel", "SSH_AUTH_SOCK": "sentinel-agent",
		"PIPS_HOME": "sentinel-home", "HOME": t.TempDir(), "PATH": filepath.Dir(executable),
	}
	definition := codingmcp.ResolvedDefinition{
		Definition: codingmcp.Definition{
			ID: "stdio", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio,
			Command: executable,
			Args: []string{
				"-test.run=^TestCodingMCPStdioHelper$",
				"-pips-coding-mcp-stdio-helper=true",
			},
			ConnectTimeout: 10 * time.Second,
		},
		Status: codingmcp.StatusEnabled,
	}

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Workspace:      opened,
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			TempRoot:       tempRoot,
			Environment: func(name string) (string, bool) {
				value, ok := parent[name]

				return value, ok
			},
			MaxTools: 16,
		},
	)
	require.NoError(t, err)

	entries := connections.Snapshot().Entries
	require.Len(t, entries, 1)
	parts, err := entries[0].Tool.Exec(t.Context(), agent.ToolCall{
		ID: "env-1", Name: entries[0].Tool.Decl().Name, Args: ai.JSON(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("clean")}, parts)
	require.NoError(t, connections.Close())
}

func enabledHTTPDefinition(id string) codingmcp.ResolvedDefinition {
	return codingmcp.ResolvedDefinition{
		Definition: codingmcp.Definition{
			ID: id, Scope: codingmcp.ScopeUser,
			Transport:      codingmcp.TransportStreamableHTTP,
			URL:            "https://" + id + ".example.test/mcp",
			ConnectTimeout: 5 * time.Second,
		},
		Status: codingmcp.StatusEnabled,
	}
}

func testMCPServer(name, toolName string) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: name, Version: "v1"}, nil)
	server.AddTool(testSDKTool(toolName), func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})

	return server
}

func testSDKTool(name string) *sdk.Tool {
	return &sdk.Tool{
		Name: name,
		InputSchema: json.RawMessage(`{
          "type":"object",
          "additionalProperties":false
        }`),
	}
}

type closeRecorder struct {
	close  func() error
	record func()
}

func (c closeRecorder) Close() error {
	c.record()

	return c.close()
}
