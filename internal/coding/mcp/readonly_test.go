package mcp_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadOnlyToolsRunConcurrently checks that only user-marked MCP tools run
// concurrently within one response; every other MCP tool is a serial barrier.
func TestReadOnlyToolsRunConcurrently(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		readOnly map[string][]string
		want     int32
	}{
		{name: "unmarked", want: 1},
		{name: "marked by name", readOnly: map[string][]string{"exa": {"web_search_exa"}}, want: 2},
		{name: "marked by wildcard", readOnly: map[string][]string{"exa": {codingmcp.ReadOnlyWildcard}}, want: 2},
		{name: "other server", readOnly: map[string][]string{"other": {"*"}}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var active, peak atomic.Int32

			var once sync.Once

			both := make(chan struct{})
			server := sdk.NewServer(&sdk.Implementation{Name: "exa", Version: "v1"}, nil)
			server.AddTool(testSDKTool("web_search_exa"), func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				current := active.Add(1)
				defer active.Add(-1)

				for {
					seen := peak.Load()
					if current <= seen || peak.CompareAndSwap(seen, current) {
						break
					}
				}

				if current == 2 {
					once.Do(func() { close(both) })
				}
				// Wait briefly for a concurrent peer; a serial run times out.
				select {
				case <-both:
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
				}

				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
			})
			factory := func(ctx context.Context, _ codingmcp.Definition) (sdk.Transport, io.Closer, error) {
				serverTransport, clientTransport := sdk.NewInMemoryTransports()
				session, err := server.Connect(ctx, serverTransport, nil)

				return clientTransport, session, err
			}
			connections, err := codingmcp.OpenConnections(
				t.Context(), []codingmcp.ResolvedDefinition{enabledHTTPDefinition("exa")},
				codingmcp.ConnectionOptions{
					Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
					MaxTools:       16, Transport: factory, ReadOnlyTools: tc.readOnly,
				},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, connections.Close()) })
			require.NoError(t, connections.Wait(t.Context()))

			entries := connections.Entries(codingmcp.VisibilityAmbient)
			require.Len(t, entries, 1)
			name := entries[0].Tool.Decl().Name
			model := &batchModel{responses: []*ai.Response{
				{
					Message: ai.Assistant(
						ai.ToolCallPart{ID: "c1", Name: name, Args: ai.JSON(`{}`)},
						ai.ToolCallPart{ID: "c2", Name: name, Args: ai.JSON(`{}`)},
					),
					FinishReason: ai.FinishToolCalls,
				},
				{Message: ai.AssistantText("done"), FinishReason: ai.FinishStop},
			}}
			runner, err := agent.New(model, agent.WithTools(entries[0].Tool))
			require.NoError(t, err)
			_, err = runner.Run(t.Context(), agent.NewSession(), ai.UserText("search twice"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, peak.Load())
		})
	}
}

type batchModel struct {
	mu        sync.Mutex
	responses []*ai.Response
}

func (m *batchModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.responses) == 0 {
		return nil, errors.New("script exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *batchModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("streaming is not scripted"))
	}
}

func (m *batchModel) Provider() ai.Provider { return ai.Provider("scripted") }
func (m *batchModel) ModelID() string       { return "scripted" }
func (m *batchModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}
