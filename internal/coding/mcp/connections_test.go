//nolint:wsl_v5 // Connection fixtures keep gate, wait, and assertion steps adjacent.
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
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	agentmcp "github.com/rsbin1178/pips/agent/mcp"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
		if os.Getenv("PIPS_TEST_MCP_SCOPE") != "session-only" {
			return nil, errors.New("session environment overlay is missing")
		}

		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "clean"},
		}}, nil
	})

	server.AddTool(testSDKTool("credential"), func(
		context.Context,
		*sdk.CallToolRequest,
	) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: os.Getenv("MCP_SERVICE_API_KEY")},
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
	require.NoError(t, connections.Wait(t.Context()))
	assert.True(t, connections.Settled())

	snapshot := connections.Snapshot()
	// The initial empty snapshot is version 1; each materially changed join
	// installs the next version regardless of goroutine completion order.
	assert.Equal(t, uint64(3), snapshot.Version)
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

	statuses := connections.Servers()
	require.Len(t, statuses, 5)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[0].State)
	assert.Equal(t, []string{"one_lookup"}, statuses[0].Tools)
	assert.Equal(t, codingmcp.ServerStateFailed, statuses[1].State)
	assert.Equal(t, "transport", statuses[1].Stage)
	assert.Equal(t, codingmcp.ServerStateFailed, statuses[2].State)
	assert.Equal(t, "list_failed", statuses[2].Code)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[3].State)
	assert.Equal(t, codingmcp.ServerStateDisabled, statuses[4].State)
	assert.True(t, statuses[4].StartedAt.IsZero())

	require.NoError(t, connections.Close())
	require.NoError(t, connections.Close())
	mu.Lock()
	assert.Equal(t, []string{"bad-list", "two", "one"}, closeOrder)
	mu.Unlock()
}

func TestOpenConnectionsReturnsBeforeSlowServerJoinsAndPublishesIncrementally(t *testing.T) {
	t.Parallel()

	servers := map[string]*sdk.Server{
		"fast": testMCPServer("fast", "lookup"),
		"slow": testMCPServer("slow", "search"),
	}
	gate := make(chan struct{})
	factory := func(ctx context.Context, definition codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		if definition.ID == "slow" {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := servers[definition.ID].Connect(ctx, serverTransport, nil)

		return clientTransport, session, err
	}

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{enabledHTTPDefinition("fast"), enabledHTTPDefinition("slow")},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	assert.False(t, connections.Settled())

	require.Eventually(t, func() bool {
		return len(connections.Snapshot().Entries) == 1
	}, 30*time.Second, 10*time.Millisecond)
	assert.Equal(t, "fast_lookup", connections.Snapshot().Entries[0].Tool.Decl().Name)
	assert.Equal(t, uint64(2), connections.Snapshot().Version)
	statuses := connections.Servers()
	require.Len(t, statuses, 2)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[0].State)
	assert.Equal(t, codingmcp.ServerStateConnecting, statuses[1].State)
	assert.True(t, statuses[1].SettledAt.IsZero())
	assert.False(t, connections.Settled())

	waitCtx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, connections.Wait(waitCtx), context.Canceled)

	close(gate)
	require.NoError(t, connections.Wait(t.Context()))
	assert.True(t, connections.Settled())
	snapshot := connections.Snapshot()
	assert.Equal(t, uint64(3), snapshot.Version)
	require.Len(t, snapshot.Entries, 2)
	assert.Equal(t, "fast_lookup", snapshot.Entries[0].Tool.Decl().Name)
	assert.Equal(t, "slow_search", snapshot.Entries[1].Tool.Decl().Name)
	statuses = connections.Servers()
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[1].State)
	assert.Equal(t, []string{"slow_search"}, statuses[1].Tools)
	assert.False(t, statuses[1].SettledAt.Before(statuses[1].StartedAt))
	assert.Empty(t, connections.Diagnostics())
}

func TestConnectionsCloseCancelsInFlightConnectWithoutInstallingLateClient(t *testing.T) {
	t.Parallel()

	server := testMCPServer("slow", "search")
	gate := make(chan struct{})
	var sessions sync.WaitGroup
	factory := func(ctx context.Context, _ codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		<-gate
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := server.Connect(context.WithoutCancel(ctx), serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		sessions.Add(1)

		return clientTransport, closeRecorder{
			close: session.Close, record: sessions.Done,
		}, nil
	}

	definition := enabledHTTPDefinition("slow")
	definition.Definition.ConnectTimeout = time.Minute
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)

	closed := make(chan error, 1)
	go func() { closed <- connections.Close() }()
	// The factory ignores cancellation until the gate opens; Close must still
	// finish once the late client arrives, and that client must be closed
	// rather than installed.
	close(gate)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return after the in-flight connect finished")
	}
	sessions.Wait()
	assert.True(t, connections.Settled())
	assert.Empty(t, connections.Snapshot().Entries)
	assert.Equal(t, uint64(1), connections.Snapshot().Version)
	require.Len(t, connections.Servers(), 1)
	assert.NotEqual(t, codingmcp.ServerStateConnected, connections.Servers()[0].State)
}

func TestConnectionsDiagnosticsStayOrderedAndTakeOnce(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	factory := func(ctx context.Context, definition codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		if definition.ID == "first" {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}

		return nil, nil, errors.New("sentinel transport failure")
	}

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{enabledHTTPDefinition("first"), enabledHTTPDefinition("second")},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
			Diagnostics: []codingmcp.ConnectionDiagnostic{{
				ServerID: "plugin", Stage: "configuration", Code: "definition_limit",
			}},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.Eventually(t, func() bool {
		return connections.Servers()[1].State == codingmcp.ServerStateFailed
	}, 30*time.Second, 10*time.Millisecond)
	taken := connections.TakeDiagnostics()
	require.Len(t, taken, 2)
	assert.Equal(t, "plugin", taken[0].ServerID)
	assert.Equal(t, "second", taken[1].ServerID)

	close(gate)
	require.NoError(t, connections.Wait(t.Context()))
	all := connections.Diagnostics()
	require.Len(t, all, 3)
	assert.Equal(t, []string{"plugin", "first", "second"}, []string{
		all[0].ServerID, all[1].ServerID, all[2].ServerID,
	})
	for _, diagnostic := range all {
		assert.NotContains(t, diagnostic.Message, "sentinel")
	}
	taken = connections.TakeDiagnostics()
	require.Len(t, taken, 1)
	assert.Equal(t, "first", taken[0].ServerID)
	assert.Empty(t, connections.TakeDiagnostics())
	assert.Len(t, connections.Diagnostics(), 3)
}

func TestConnectionsServersProjectionIsCredentialFree(t *testing.T) {
	t.Parallel()

	factory := func(context.Context, codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		return nil, nil, errors.New("sentinel transport failure")
	}
	secrets := []string{
		"https://secret-host.example.test/mcp", "Bearer sentinel-token",
		"/usr/local/bin/sentinel-server", "--sentinel-arg", "sentinel-env-value",
	}
	resolved := []codingmcp.ResolvedDefinition{
		{
			Definition: codingmcp.Definition{
				ID: "remote", Scope: codingmcp.ScopeAgentPlugin,
				Transport: codingmcp.TransportStreamableHTTP, URL: secrets[0],
				Headers:        []codingmcp.HTTPHeader{{Name: "Authorization", Value: secrets[1]}},
				ConnectTimeout: time.Second,
			},
			Status: codingmcp.StatusEnabled,
		},
		{
			Definition: codingmcp.Definition{
				ID: "local", Scope: codingmcp.ScopeProject, Transport: codingmcp.TransportStdio,
				Command: secrets[2], Args: []string{secrets[3]},
				Environment:    []execution.EnvVar{{Name: "TOKEN", Value: secrets[4]}},
				Visibility:     codingmcp.VisibilityAgentPrivate,
				ConnectTimeout: time.Second,
			},
			Status: codingmcp.StatusPending,
		},
	}

	connections, err := codingmcp.OpenConnections(t.Context(), resolved, codingmcp.ConnectionOptions{
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		MaxTools:       16, Transport: factory,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	require.NoError(t, connections.Wait(t.Context()))

	statuses := connections.Servers()
	require.Len(t, statuses, 2)
	assert.Equal(t, codingmcp.ServerStateFailed, statuses[0].State)
	assert.Equal(t, codingmcp.ScopeAgentPlugin, statuses[0].Scope)
	assert.Equal(t, codingmcp.TransportStreamableHTTP, statuses[0].Transport)
	assert.Equal(t, codingmcp.VisibilityAmbient, statuses[0].Visibility)
	assert.Equal(t, codingmcp.ServerStatePending, statuses[1].State)
	assert.Equal(t, codingmcp.VisibilityAgentPrivate, statuses[1].Visibility)
	assert.True(t, statuses[1].StartedAt.IsZero())

	encoded, err := json.Marshal(statuses)
	require.NoError(t, err)
	for _, secret := range secrets {
		assert.NotContains(t, string(encoded), secret)
	}
	assert.NotContains(t, string(encoded), "sentinel")
}

func TestConfiguredHeadersNeverCrossOriginRedirect(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	t.Cleanup(target.Close)

	sourceHeaders := make(chan http.Header, 1)
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sourceHeaders <- request.Header.Clone()

		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{{
			Definition: codingmcp.Definition{
				ID: "ap-redirect", Scope: codingmcp.ScopeAgentPlugin,
				Transport: codingmcp.TransportStreamableHTTP, URL: source.URL,
				Headers: []codingmcp.HTTPHeader{
					{Name: "X-Public", Value: "visible"},
					{Name: "Content-Type", Value: "text/plain"},
				},
				ConnectTimeout: time.Second,
			},
			Status: codingmcp.StatusEnabled,
		}},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			HTTPClient:     &http.Client{Timeout: 2 * time.Second}, MaxTools: 16,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	headers := <-sourceHeaders
	assert.Equal(t, "visible", headers.Get("X-Public"))
	assert.NotEqual(t, "text/plain", headers.Get("Content-Type"))
	require.NoError(t, connections.Wait(t.Context()))
	assert.Zero(t, targetRequests.Load())
	require.Len(t, connections.Diagnostics(), 1)
	assert.Equal(t, "connect_failed", connections.Diagnostics()[0].Code)
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
	require.NoError(t, connections.Wait(t.Context()))

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
	}, 30*time.Second, 10*time.Millisecond)
	require.Error(t, refreshErr)
	assert.True(t, attempted)
	assert.Equal(t, initial.Version, failed.Version)
	assert.Equal(t, initial.Version, connections.Snapshot().Version)
	require.Len(t, connections.Diagnostics(), 1)

	server.AddTool(testSDKTool("also-overflow"), nil)
	require.Eventually(t, func() bool {
		_, attempted, refreshErr = connections.RefreshChanged(t.Context())

		return attempted
	}, 30*time.Second, 10*time.Millisecond)
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

func TestConnectionsPartitionAmbientAndAgentPrivateEntries(t *testing.T) {
	t.Parallel()

	servers := map[string]*sdk.Server{
		"ambient": testMCPServer("ambient", "lookup"),
		"private": testMCPServer("private", "search"),
	}
	factory := func(ctx context.Context, definition codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := servers[definition.ID].Connect(ctx, serverTransport, nil)

		return clientTransport, session, err
	}
	ambient := enabledHTTPDefinition("ambient")
	private := enabledHTTPDefinition("private")
	private.Definition.Visibility = codingmcp.VisibilityAgentPrivate
	connections, err := codingmcp.OpenConnections(
		t.Context(), []codingmcp.ResolvedDefinition{ambient, private},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	require.NoError(t, connections.Wait(t.Context()))
	require.Len(t, connections.Snapshot().Entries, 2)
	require.Len(t, connections.Entries(codingmcp.VisibilityAmbient), 1)
	assert.Equal(t, "ambient_lookup", connections.Entries(codingmcp.VisibilityAmbient)[0].Tool.Decl().Name)
	require.Len(t, connections.Entries(codingmcp.VisibilityAgentPrivate), 1)
	assert.Equal(t, "private_search", connections.Entries(codingmcp.VisibilityAgentPrivate)[0].Tool.Decl().Name)

	bindings := connections.ConnectedServers(codingmcp.VisibilityAgentPrivate)
	require.Len(t, bindings, 1)
	assert.Equal(t, "private", bindings[0].ID)
	assert.Equal(t, private.Definition.Fingerprint(), bindings[0].Fingerprint)
	bindings[0].Entries[0].Tags = append(bindings[0].Entries[0].Tags, "mutated")
	assert.NotContains(t, connections.Entries(codingmcp.VisibilityAgentPrivate)[0].Tags, "mutated")
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
	require.NoError(t, connections.Wait(t.Context()))
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
			Environment: []execution.EnvVar{{
				Name: "PIPS_TEST_MCP_SCOPE", Value: "session-only",
			}},
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
	require.NoError(t, connections.Wait(t.Context()))

	entries := connections.Snapshot().Entries
	require.Len(t, entries, 2)
	tool := findEntry(t, entries, "stdio_environment")
	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "env-1", Name: tool.Decl().Name, Args: ai.JSON(`{}`),
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

func TestNativeCredentialReferencesExpandOnlyInsideTheTransport(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)
	opened, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tempRoot := t.TempDir()
	require.NoError(t, os.Chmod(tempRoot, 0o700)) //nolint:gosec // Directories require owner traversal.

	received := make(chan http.Header, 16)
	server := testMCPServer("docs", "search")
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case received <- request.Header.Clone():
		default:
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	parent := map[string]string{
		"DOCS_API_KEY": "docs-secret", "SERVICE_API_KEY": "stdio-secret",
		"HOME": t.TempDir(), "PATH": filepath.Dir(executable),
	}
	remote := codingmcp.Definition{
		ID: "docs", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStreamableHTTP,
		URL:            httpServer.URL,
		Headers:        []codingmcp.HTTPHeader{{Name: "Authorization", Value: "Bearer ${env:DOCS_API_KEY}"}},
		ConnectTimeout: 10 * time.Second,
	}
	local := codingmcp.Definition{
		ID: "stdio", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio,
		Command: executable,
		Args: []string{
			"-test.run=^TestCodingMCPStdioHelper$",
			"-pips-coding-mcp-stdio-helper=true",
		},
		Environment: []execution.EnvVar{
			{Name: "MCP_SERVICE_API_KEY", Value: "${env:SERVICE_API_KEY}"},
			{Name: "PIPS_TEST_MCP_SCOPE", Value: "session-only"},
		},
		ConnectTimeout: 10 * time.Second,
	}
	missing := codingmcp.Definition{
		ID: "missing", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStreamableHTTP,
		URL:            httpServer.URL,
		Headers:        []codingmcp.HTTPHeader{{Name: "X-Api-Key", Value: "${env:UNSET_API_KEY}"}},
		ConnectTimeout: 10 * time.Second,
	}
	definitions, err := codingmcp.NewDefinitions([]codingmcp.Definition{remote, local, missing}, 3)
	require.NoError(t, err)
	fingerprints := make([]string, 0, 3)
	resolved := make([]codingmcp.ResolvedDefinition, 0, 3)
	for _, definition := range definitions.List() {
		fingerprints = append(fingerprints, definition.Fingerprint())
		resolved = append(resolved, codingmcp.ResolvedDefinition{
			Definition: definition, Status: codingmcp.StatusEnabled,
		})
	}

	connections, err := codingmcp.OpenConnections(t.Context(), resolved, codingmcp.ConnectionOptions{
		Workspace:      opened,
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		TempRoot:       tempRoot,
		Environment: func(name string) (string, bool) {
			value, ok := parent[name]

			return value, ok
		},
		MaxTools: 16,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	require.NoError(t, connections.Wait(t.Context()))

	headers := <-received
	assert.Equal(t, "Bearer docs-secret", headers.Get("Authorization"))

	tool := findEntry(t, connections.Snapshot().Entries, "stdio_credential")
	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "credential-1", Name: tool.Decl().Name, Args: ai.JSON(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("stdio-secret")}, parts)

	statuses := connections.Servers()
	require.Len(t, statuses, 3)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[0].State)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[1].State)
	assert.Equal(t, codingmcp.ServerStateFailed, statuses[2].State)
	assert.Equal(t, "credential_unavailable", statuses[2].Code)
	assert.Contains(t, statuses[2].Message, "UNSET_API_KEY")

	for index, definition := range definitions.List() {
		assert.Equal(t, fingerprints[index], definition.Fingerprint())
	}
	encoded, err := json.Marshal(struct {
		Statuses    []codingmcp.ServerStatus
		Diagnostics []codingmcp.ConnectionDiagnostic
		Definitions []codingmcp.Definition
	}{statuses, connections.Diagnostics(), definitions.List()})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "docs-secret")
	assert.NotContains(t, string(encoded), "stdio-secret")
}

func TestAgentPluginHeadersAreNeverExpanded(t *testing.T) {
	t.Parallel()

	received := make(chan http.Header, 16)
	server := testMCPServer("plugin", "search")
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case received <- request.Header.Clone():
		default:
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)

	connections, err := codingmcp.OpenConnections(t.Context(), []codingmcp.ResolvedDefinition{{
		Definition: codingmcp.Definition{
			ID: "plugin", Scope: codingmcp.ScopeAgentPlugin, Transport: codingmcp.TransportStreamableHTTP,
			URL:            httpServer.URL,
			Headers:        []codingmcp.HTTPHeader{{Name: "X-Literal", Value: "${env:HOME}"}},
			ConnectTimeout: 10 * time.Second,
		},
		Status: codingmcp.StatusEnabled,
	}}, codingmcp.ConnectionOptions{
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		MaxTools:       16,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })
	require.NoError(t, connections.Wait(t.Context()))

	assert.Equal(t, "${env:HOME}", (<-received).Get("X-Literal"))
}

func TestCredentialReferencesRequireEnvironmentLookup(t *testing.T) {
	t.Parallel()

	definition := enabledHTTPDefinition("docs")
	definition.Definition.Headers = []codingmcp.HTTPHeader{{Name: "X-Api-Key", Value: "${env:DOCS_API_KEY}"}}
	_, err := codingmcp.OpenConnections(t.Context(), []codingmcp.ResolvedDefinition{definition}, codingmcp.ConnectionOptions{
		Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		MaxTools:       16,
	})
	require.ErrorIs(t, err, codingmcp.ErrInvalid)
}

func findEntry(t *testing.T, entries []catalog.Entry, name string) agent.Tool {
	t.Helper()

	for _, entry := range entries {
		if entry.Tool.Decl().Name == name {
			return entry.Tool
		}
	}
	t.Fatalf("tool %q not found", name)

	return nil
}
