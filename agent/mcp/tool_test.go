package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolExecForwardsArgumentsAndConvertsResults(t *testing.T) {
	t.Parallel()

	arguments := make(chan json.RawMessage, 1)
	server := mcp.NewServer(&mcp.Implementation{Name: "tools", Version: "v1"}, nil)
	server.AddTool(rawTool("mixed.content"), func(
		_ context.Context,
		req *mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		arguments <- req.Params.Arguments

		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "done"},
			&mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"},
			&mcp.AudioContent{Data: []byte("audio"), MIMEType: "audio/wav"},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI:  "test://text",
				Text: "embedded text",
			}},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI:      "test://report.pdf",
				MIMEType: "application/pdf",
				Blob:     []byte("pdf"),
			}},
			&mcp.ResourceLink{URI: "https://example.invalid/report", Name: "report"},
		}}, nil
	})
	server.AddTool(rawTool("structured"), func(
		context.Context,
		*mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"answer": 42}}, nil
	})
	server.AddTool(rawTool("fails"), func(
		context.Context,
		*mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "invalid location"}},
			IsError: true,
		}, nil
	})

	client, _ := connectTestServer(t, server)
	tools, err := client.Tools(t.Context())
	require.NoError(t, err)

	mixed := findRemoteTool(t, tools, "mixed.content")
	parts, err := mixed.Exec(t.Context(), agent.ToolCall{
		ID:   "call-1",
		Name: mixed.Decl().Name,
		Args: ai.JSON(`{"city":"Paris","days":2}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 6)
	assert.Equal(t, ai.Text("done"), parts[0])
	assert.Equal(t, ai.ImagePart{Source: ai.MediaSource{
		Data:     []byte("image"),
		MIMEType: "image/png",
	}}, parts[1])
	assert.Equal(t, ai.FilePart{Source: ai.MediaSource{
		Data:     []byte("audio"),
		MIMEType: "audio/wav",
	}}, parts[2])
	assert.Equal(t, ai.Text("embedded text"), parts[3])
	assert.Equal(t, ai.FilePart{
		Source: ai.MediaSource{Data: []byte("pdf"), MIMEType: "application/pdf"},
		Name:   "test://report.pdf",
	}, parts[4])

	link, ok := parts[5].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, link.Text, `"uri":"https://example.invalid/report"`)
	assert.JSONEq(t, `{"city":"Paris","days":2}`, string(<-arguments))

	_, err = mixed.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(`not-json`)})
	require.ErrorContains(t, err, "arguments are invalid JSON")

	_, err = mixed.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(`[]`)})
	require.ErrorContains(t, err, "arguments must be a JSON object")

	structured := findRemoteTool(t, tools, "structured")
	parts, err = structured.Exec(t.Context(), agent.ToolCall{})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.Equal(t, ai.Text(`{"answer":42}`), parts[0])

	failing := findRemoteTool(t, tools, "fails")
	_, err = failing.Exec(t.Context(), agent.ToolCall{})
	require.Error(t, err)

	var toolErr *ToolError
	require.ErrorAs(t, err, &toolErr)
	assert.Equal(t, "fails", toolErr.RemoteName)
	assert.Equal(t, "invalid location", toolErr.Message)
	assert.Equal(t, `agent/mcp: tool "fails" failed: invalid location`, toolErr.Error())
}

func TestToolExecWrapsProtocolErrors(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "protocol", Version: "v1"}, nil)
	server.AddTool(rawTool("ephemeral"), nil)
	client, _ := connectTestServer(t, server)

	tools, err := client.Tools(t.Context())
	require.NoError(t, err)
	tool := findRemoteTool(t, tools, "ephemeral")

	server.RemoveTools("ephemeral")

	_, err = tool.Exec(t.Context(), agent.ToolCall{})
	require.Error(t, err)
	require.ErrorContains(t, err, `call tool "ephemeral"`)

	var toolErr *ToolError
	assert.NotErrorAs(t, err, &toolErr)
}

func TestToolExecPropagatesCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "cancel", Version: "v1"}, nil)
	server.AddTool(rawTool("blocking"), func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)

		return nil, ctx.Err()
	})

	client, _ := connectTestServer(t, server)
	tools, err := client.Tools(t.Context())
	require.NoError(t, err)
	tool := findRemoteTool(t, tools, "blocking")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	result := make(chan error, 1)

	go func() {
		_, err := tool.Exec(ctx, agent.ToolCall{})
		result <- err
	}()

	<-started
	cancel()

	err = <-result
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	<-cancelled

	client.progressMu.RLock()
	progressCount := len(client.progress)
	client.progressMu.RUnlock()
	assert.Zero(t, progressCount)
}

type progressModel struct {
	mu        sync.Mutex
	responses []*ai.Response
	position  int
}

func (m *progressModel) Generate(_ context.Context, _ ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.position >= len(m.responses) {
		return nil, errors.New("progress model: script exhausted")
	}

	response := m.responses[m.position]
	m.position++

	return response, nil
}

func (*progressModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("progress model: stream is unsupported"))
	}
}

func (*progressModel) Provider() ai.Provider { return ai.Provider("progress-test") }
func (*progressModel) ModelID() string       { return "progress-test" }
func (*progressModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func TestToolProgressBecomesAgentUpdateAndChainsHandler(t *testing.T) {
	t.Parallel()

	tokens := make(chan string, 1)
	progressHandled := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "progress", Version: "v1"}, nil)
	server.AddTool(rawTool("long_task"), func(
		ctx context.Context,
		req *mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		token, ok := req.Params.GetProgressToken().(string)
		if !ok {
			return nil, errors.New("missing progress token")
		}

		tokens <- token

		err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      1,
			Total:         2,
			Message:       "halfway",
		})
		if err != nil {
			return nil, err
		}

		select {
		case <-progressHandled:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "complete"},
		}}, nil
	})

	chained := make(chan *mcp.ProgressNotificationParams, 1)
	client, _ := connectTestServer(t, server, WithClientOptions(&mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			chained <- req.Params

			close(progressHandled)
		},
	}))
	tools, err := client.Tools(t.Context())
	require.NoError(t, err)

	model := &progressModel{responses: []*ai.Response{
		{
			Message: ai.Assistant(ai.ToolCallPart{
				ID:   "call-progress",
				Name: "long_task",
				Args: ai.JSON(`{}`),
			}),
			FinishReason: ai.FinishToolCalls,
		},
		{Message: ai.AssistantText("done"), FinishReason: ai.FinishStop},
	}}

	var updates []agent.ToolUpdated

	var updatesMu sync.Mutex

	runtime, err := agent.New(model,
		agent.WithTools(tools...),
		agent.WithOnEvent(func(_ context.Context, event agent.Event) {
			if update, ok := event.Payload().(agent.ToolUpdated); ok {
				updatesMu.Lock()

				updates = append(updates, update)
				updatesMu.Unlock()
			}
		}),
	)
	require.NoError(t, err)

	result, err := runtime.Run(t.Context(), agent.NewSession(), ai.UserText("run it"))
	require.NoError(t, err)
	assert.Equal(t, "done", result.Text())
	assert.NotEmpty(t, <-tokens)

	progress := <-chained
	assert.Equal(t, "halfway", progress.Message)

	updatesMu.Lock()
	require.Len(t, updates, 1)
	assert.Equal(t, "long_task", updates[0].Call.Name)
	assert.Equal(t, []ai.Part{ai.Text("halfway")}, updates[0].Update)
	updatesMu.Unlock()

	client.progressMu.RLock()
	progressCount := len(client.progress)
	client.progressMu.RUnlock()
	assert.Zero(t, progressCount)
}
