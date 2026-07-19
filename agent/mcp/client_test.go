package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const emptyInputSchema = `{"type":"object","additionalProperties":false}`

type fakeToolSession struct {
	tools   []*mcp.Tool
	listErr error
	call    func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

func (s *fakeToolSession) Tools(_ context.Context, _ *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error] {
	return func(yield func(*mcp.Tool, error) bool) {
		if s.listErr != nil {
			yield(nil, s.listErr)

			return
		}

		for _, tool := range s.tools {
			if !yield(tool, nil) {
				return
			}
		}
	}
}

func (s *fakeToolSession) CallTool(
	ctx context.Context,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	if s.call == nil {
		return &mcp.CallToolResult{}, nil
	}

	return s.call(ctx, params)
}

type failingTransport struct {
	err error
}

func (t failingTransport) Connect(context.Context) (mcp.Connection, error) {
	return nil, t.err
}

func rawTool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: "remote " + name,
		InputSchema: json.RawMessage(emptyInputSchema),
	}
}

func newFakeClient(t *testing.T, session toolSession, options ...Option) *Client {
	t.Helper()

	cfg, err := buildConfig(options)
	require.NoError(t, err)

	return &Client{
		tools:           session,
		maxTools:        cfg.maxTools,
		nameMapper:      cfg.nameMapper,
		namePrefix:      cfg.namePrefix,
		toolListChanged: make(chan struct{}, 1),
		progress:        make(map[string]func(ai.Part)),
	}
}

func connectTestServer(
	t *testing.T,
	server *mcp.Server,
	options ...Option,
) (*Client, *mcp.ServerSession) {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, serverSession.Close())
	})

	client, err := Connect(
		t.Context(),
		&mcp.Implementation{Name: "pips-test", Version: "v0.1.0"},
		clientTransport,
		options...,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
	})

	return client, serverSession
}

func findRemoteTool(t *testing.T, tools []agent.Tool, remoteName string) *Tool {
	t.Helper()

	for _, candidate := range tools {
		tool, ok := candidate.(*Tool)
		if ok && tool.RemoteName() == remoteName {
			return tool
		}
	}

	t.Fatalf("remote tool %q not found", remoteName)

	return nil
}

func TestConnectValidatesAndWrapsFailures(t *testing.T) {
	t.Parallel()

	var nilClient *Client
	assert.NoError(t, nilClient.Close())
	assert.NoError(t, (&Client{}).Close())

	implementation := &mcp.Implementation{Name: "test", Version: "v1"}

	_, err := Connect(t.Context(), nil, failingTransport{})
	require.ErrorContains(t, err, "nil client implementation")

	_, err = Connect(t.Context(), implementation, nil)
	require.ErrorContains(t, err, "nil transport")

	_, err = Connect(t.Context(), implementation, failingTransport{}, WithClientOptions(nil))
	require.ErrorContains(t, err, "nil client options")

	want := errors.New("dial failed")
	_, err = Connect(t.Context(), implementation, failingTransport{err: want})
	require.Error(t, err)
	require.ErrorIs(t, err, want)
	assert.ErrorContains(t, err, "agent/mcp: connect")
}

func TestClientToolsPaginatesAndExposesSessionFeatures(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(
		&mcp.Implementation{Name: "feature-server", Version: "v1"},
		&mcp.ServerOptions{PageSize: 1},
	)
	server.AddTool(&mcp.Tool{
		Name:        "alpha.tool",
		Description: "Alpha tool.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"value":{"type":["string","integer"],"oneOf":[{"type":"string"},{"type":"integer"}]}}
		}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	server.AddTool(rawTool("beta"), nil)
	server.AddTool(rawTool("gamma"), nil)
	server.AddResource(&mcp.Resource{
		Name: "guide",
		URI:  "test://guide",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:  "test://guide",
			Text: "guide text",
		}}}, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "review"}, func(
		context.Context,
		*mcp.GetPromptRequest,
	) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
			Role:    mcp.Role("user"),
			Content: &mcp.TextContent{Text: "Review this."},
		}}}, nil
	})

	client, _ := connectTestServer(t, server, WithToolNamePrefix("docs"))
	tools, err := client.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 3)

	alpha := findRemoteTool(t, tools, "alpha.tool")
	assert.Equal(t, "docs_alpha_tool", alpha.Decl().Name)
	assert.Equal(t, "Alpha tool.", alpha.Decl().Description)

	schemaJSON, err := json.Marshal(alpha.Decl().InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schemaJSON), `"oneOf"`)
	assert.Contains(t, string(schemaJSON), `"type":["string","integer"]`)

	session := client.Session()
	require.NotNil(t, session)
	assert.Equal(t, "feature-server", session.InitializeResult().ServerInfo.Name)

	resources, err := session.ListResources(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, resources.Resources, 1)
	assert.Equal(t, "test://guide", resources.Resources[0].URI)

	prompts, err := session.ListPrompts(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, prompts.Prompts, 1)
	assert.Equal(t, "review", prompts.Prompts[0].Name)
}

func TestConnectUsesStreamableHTTPTransport(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "http-server", Version: "v1"}, nil)
	server.AddTool(rawTool("health"), func(
		context.Context,
		*mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "healthy"},
		}}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client, err := Connect(
		t.Context(),
		&mcp.Implementation{Name: "http-client", Version: "v1"},
		&mcp.StreamableClientTransport{Endpoint: httpServer.URL},
	)
	require.NoError(t, err)

	tools, err := client.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	parts, err := tools[0].Exec(t.Context(), agent.ToolCall{})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("healthy")}, parts)

	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
}

func TestClientToolsRejectsInvalidSnapshots(t *testing.T) {
	t.Parallel()

	wantListErr := errors.New("list failed")
	tests := []struct {
		name      string
		session   *fakeToolSession
		options   []Option
		wantErr   string
		wantErrIs error
	}{
		{
			name:      "list error",
			session:   &fakeToolSession{listErr: wantListErr},
			wantErr:   "list tools",
			wantErrIs: wantListErr,
		},
		{
			name:    "nil tool",
			session: &fakeToolSession{tools: []*mcp.Tool{nil}},
			wantErr: "nil tool",
		},
		{
			name:    "missing schema",
			session: &fakeToolSession{tools: []*mcp.Tool{{Name: "bad"}}},
			wantErr: "missing schema",
		},
		{
			name: "non-object schema",
			session: &fakeToolSession{tools: []*mcp.Tool{{
				Name:        "bad",
				InputSchema: json.RawMessage("null"),
			}}},
			wantErr: "must be a JSON object",
		},
		{
			name: "mapped collision",
			session: &fakeToolSession{tools: []*mcp.Tool{
				rawTool("a.b"),
				rawTool("a_b"),
			}},
			wantErr: "duplicate name",
		},
		{
			name: "tool limit",
			session: &fakeToolSession{tools: []*mcp.Tool{
				rawTool("one"),
				rawTool("two"),
			}},
			options: []Option{WithMaxTools(1)},
			wantErr: "more than 1 tools",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := newFakeClient(t, test.session, test.options...)
			_, err := client.Tools(t.Context())
			require.ErrorContains(t, err, test.wantErr)

			if test.wantErrIs != nil {
				assert.ErrorIs(t, err, test.wantErrIs)
			}
		})
	}
}

func TestClientToolListChangedCoalescesAndChainsHandler(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "changes", Version: "v1"}, nil)
	server.AddTool(rawTool("initial"), nil)

	chained := make(chan struct{}, 1)
	client, _ := connectTestServer(t, server, WithClientOptions(&mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case chained <- struct{}{}:
			default:
			}
		},
	}))

	server.AddTool(rawTool("added"), nil)

	select {
	case <-client.ToolListChanged():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bridge list-change signal")
	}

	select {
	case <-chained:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chained SDK handler")
	}

	client.handleToolListChanged()
	client.handleToolListChanged()
	<-client.ToolListChanged()

	select {
	case <-client.ToolListChanged():
		t.Fatal("coalescing channel contained a duplicate signal")
	default:
	}
}

func TestClientToolsCanBeListedConcurrently(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "concurrent", Version: "v1"}, nil)
	server.AddTool(rawTool("lookup"), nil)
	client, _ := connectTestServer(t, server)

	var succeeded atomic.Int64

	results := make(chan error, 4)

	for range 4 {
		go func() {
			tools, err := client.Tools(t.Context())
			if err == nil && len(tools) == 1 {
				succeeded.Add(1)
			}

			results <- err
		}()
	}

	for range 4 {
		require.NoError(t, <-results)
	}

	assert.EqualValues(t, 4, succeeded.Load())
}

func TestClientProgressRoutingStopsAfterUnregister(t *testing.T) {
	t.Parallel()

	client := newFakeClient(t, &fakeToolSession{})
	started := make(chan struct{})
	release := make(chan struct{})
	handled := make(chan struct{})
	unregistered := make(chan struct{})

	const token = "pips-test"

	client.progress[token] = func(part ai.Part) {
		assert.Equal(t, ai.Text("working"), part)
		close(started)
		<-release
	}

	req := &mcp.ProgressNotificationClientRequest{Params: &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      1,
		Total:         2,
		Message:       "working",
	}}

	go func() {
		client.handleProgress(req)
		close(handled)
	}()

	<-started

	go func() {
		client.unregisterProgress(token)
		close(unregistered)
	}()

	select {
	case <-unregistered:
		t.Fatal("progress unregistered before active reporting completed")
	default:
	}

	close(release)
	<-handled
	<-unregistered

	client.handleProgress(req)
	client.handleProgress(nil)
	client.handleProgress(&mcp.ProgressNotificationClientRequest{})
	client.handleProgress(&mcp.ProgressNotificationClientRequest{Params: &mcp.ProgressNotificationParams{
		ProgressToken: 1,
	}})
}

func TestProgressText(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "server message", progressText(&mcp.ProgressNotificationParams{
		Message: "server message",
	}))
	assert.Equal(t, "MCP progress: 2/5", progressText(&mcp.ProgressNotificationParams{
		Progress: 2,
		Total:    5,
	}))
	assert.Equal(t, "MCP progress: 3", progressText(&mcp.ProgressNotificationParams{
		Progress: 3,
	}))
}
