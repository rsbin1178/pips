//nolint:wsl_v5 // Gated MCP fixtures keep release, wait, and assertion steps adjacent.
package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/ai"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedMCPServer is a Streamable HTTP MCP server whose every request blocks
// until the gate opens, so a Runtime can be observed while the server is
// still connecting.
type gatedMCPServer struct {
	http    *httptest.Server
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newGatedMCPServer(t *testing.T, toolName string) *gatedMCPServer {
	t.Helper()

	server := sdk.NewServer(&sdk.Implementation{Name: "gated", Version: "v1"}, nil)
	server.AddTool(&sdk.Tool{
		Name: toolName, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	gated := &gatedMCPServer{gate: make(chan struct{}), started: make(chan struct{})}
	gated.http = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gated.once.Do(func() { close(gated.started) })
		select {
		case <-gated.gate:
		case <-request.Context().Done():
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(gated.http.Close)

	return gated
}

func (s *gatedMCPServer) release() {
	select {
	case <-s.gate:
	default:
		close(s.gate)
	}
}

func writeTestMCPDefinitions(t *testing.T, base, servers string) {
	t.Helper()

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.Root(), 0o700))
	require.NoError(t, os.WriteFile(layout.MCPFile(), fmt.Appendf(nil, `{
  "schema":"pips.mcp/v1alpha1",
  "servers":[%s]
}`, servers), 0o600))
}

func TestRuntimeOpensBeforeMCPConnectsAndFirstPromptWaitsForTools(t *testing.T) {
	t.Parallel()

	gated := newGatedMCPServer(t, "lookup")
	base := t.TempDir()
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"docs","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, gated.http.URL,
	))
	model := newRuntimeModel(runtimeTextResponse("done"))

	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	snapshot, err := runtime.MCP(t.Context())
	require.NoError(t, err)
	assert.False(t, snapshot.Settled)
	require.Len(t, snapshot.Servers, 1)
	assert.Equal(t, "docs", snapshot.Servers[0].ID)
	assert.Equal(t, codingmcp.ServerStateConnecting, snapshot.Servers[0].State)
	assert.Empty(t, snapshot.Servers[0].Tools)

	type outcome struct {
		events []Event
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		events, promptErr := collectRuntimeEventsAndError(
			t, runtime.Prompt(context.WithoutCancel(t.Context()), ai.UserText("use the docs")),
		)
		done <- outcome{events: events, err: promptErr}
	}()

	select {
	case <-gated.started:
	case <-time.After(30 * time.Second):
		t.Fatal("MCP server never received the initialize request")
	}
	require.Eventually(t, func() bool {
		return runtime.Snapshot().Interaction.ID != ""
	}, 30*time.Second, 10*time.Millisecond)
	assert.Empty(t, model.Requests(), "the prompt must wait for MCP before reaching the model")
	snapshot, err = runtime.MCP(t.Context())
	require.NoError(t, err)
	assert.Equal(t, codingmcp.ServerStateConnecting, snapshot.Servers[0].State)

	gated.release()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.NotEmpty(t, result.events)
	case <-time.After(30 * time.Second):
		t.Fatal("prompt did not finish after the MCP server was released")
	}

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.Contains(t, toolNamesFromRequest(requests[0]), "docs_lookup")
	snapshot, err = runtime.MCP(t.Context())
	require.NoError(t, err)
	assert.True(t, snapshot.Settled)
	assert.Equal(t, codingmcp.ServerStateConnected, snapshot.Servers[0].State)
	assert.Equal(t, []string{"docs_lookup"}, snapshot.Servers[0].Tools)
	assert.False(t, snapshot.Servers[0].SettledAt.IsZero())
	assert.Empty(t, runtime.Snapshot().Diagnostics)
}

func TestRuntimePromptCancelledWhileWaitingForMCPLeavesRuntimeUsable(t *testing.T) {
	t.Parallel()

	gated := newGatedMCPServer(t, "lookup")
	base := t.TempDir()
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"docs","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, gated.http.URL,
	))
	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	promptCtx, cancel := context.WithCancel(t.Context())
	failure := make(chan error, 1)
	go func() {
		_, err := collectRuntimeEventsAndError(t, runtime.Prompt(promptCtx, ai.UserText("wait")))
		failure <- err
	}()
	select {
	case <-gated.started:
	case <-time.After(30 * time.Second):
		t.Fatal("MCP server never received the initialize request")
	}
	require.Eventually(t, func() bool {
		return runtime.Snapshot().Interaction.ID != ""
	}, 30*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-failure:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled prompt did not return while waiting for MCP")
	}
	assert.Empty(t, model.Requests())
	require.Eventually(t, func() bool {
		return runtime.Snapshot().Phase == PhaseIdle
	}, 30*time.Second, 10*time.Millisecond)

	gated.release()
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("retry")))
	assert.NotEmpty(t, events)
	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.Contains(t, toolNamesFromRequest(requests[0]), "docs_lookup")
}

func TestRuntimeReportsBackgroundMCPFailureOnceAtInteractionBoundary(t *testing.T) {
	t.Parallel()

	healthy := newGatedMCPServer(t, "search")
	healthy.release()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	base := t.TempDir()
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"good","type":"streamable_http","url":%q,"connect_timeout":"10s"},`+
			`{"id":"bad","type":"streamable_http","url":%q,"connect_timeout":"10s"}`,
		healthy.http.URL, unreachableURL,
	))
	model := newRuntimeModel(runtimeTextResponse("first"), runtimeTextResponse("second"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("one")))
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("two")))

	diagnostics := make([]IntegrationDiagnostic, 0)
	for _, diagnostic := range runtime.Snapshot().Diagnostics {
		if diagnostic.Component == componentMCP {
			diagnostics = append(diagnostics, diagnostic)
		}
	}
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "connect_failed", diagnostics[0].Code)
	assert.True(t, diagnostics[0].Disabled)
	assert.NotContains(t, diagnostics[0].Message, unreachableURL)

	requests := model.Requests()
	require.Len(t, requests, 2)
	for _, request := range requests {
		assert.Contains(t, toolNamesFromRequest(request), "good_search")
	}
	snapshot, err := runtime.MCP(t.Context())
	require.NoError(t, err)
	assert.True(t, snapshot.Settled)
	require.Len(t, snapshot.Servers, 2)
	assert.Equal(t, codingmcp.ServerStateConnected, snapshot.Servers[0].State)
	assert.Equal(t, codingmcp.ServerStateFailed, snapshot.Servers[1].State)
	assert.Equal(t, "connect_failed", snapshot.Servers[1].Code)

	require.NoError(t, runtime.Close(t.Context()))
	_, err = runtime.MCP(t.Context())
	require.ErrorIs(t, err, ErrRuntimeClosed)
}
