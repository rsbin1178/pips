package agentmcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

type toolSession interface {
	Tools(context.Context, *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error]
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// Client owns an initialized official MCP client session and adapts its tools
// for the agent runtime. Its methods are safe for concurrent use.
type Client struct {
	session *mcp.ClientSession
	tools   toolSession

	maxTools   int
	nameMapper ToolNameMapper
	namePrefix string

	toolListChanged chan struct{}
	progressID      atomic.Uint64
	progressMu      sync.RWMutex
	progress        map[string]func(ai.Part)
}

// Connect initializes an MCP client over transport. Any official SDK
// transport is supported, including CommandTransport for stdio and
// StreamableClientTransport for HTTP. The returned Client owns the session and
// must be closed.
func Connect(
	ctx context.Context,
	implementation *mcp.Implementation,
	transport mcp.Transport,
	options ...Option,
) (*Client, error) {
	if implementation == nil {
		return nil, errors.New("agent/mcp: nil client implementation")
	}

	if transport == nil {
		return nil, errors.New("agent/mcp: nil transport")
	}

	cfg, err := buildConfig(options)
	if err != nil {
		return nil, err
	}

	client := &Client{
		maxTools:        cfg.maxTools,
		nameMapper:      cfg.nameMapper,
		namePrefix:      cfg.namePrefix,
		toolListChanged: make(chan struct{}, 1),
		progress:        make(map[string]func(ai.Part)),
	}

	sdkOptions := cfg.clientOptions
	progressHandler := sdkOptions.ProgressNotificationHandler
	sdkOptions.ProgressNotificationHandler = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
		client.handleProgress(req)

		if progressHandler != nil {
			progressHandler(ctx, req)
		}
	}

	listHandler := sdkOptions.ToolListChangedHandler
	sdkOptions.ToolListChangedHandler = func(ctx context.Context, req *mcp.ToolListChangedRequest) {
		client.handleToolListChanged()

		if listHandler != nil {
			listHandler(ctx, req)
		}
	}

	sdkClient := mcp.NewClient(implementation, &sdkOptions)

	session, err := sdkClient.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("agent/mcp: connect: %w", err)
	}

	client.session = session
	client.tools = session

	return client, nil
}

// Session returns the initialized official SDK session. The caller may use it
// for prompts, resources, completions, logging, and other MCP capabilities.
// Close the owning Client rather than closing this session separately.
func (c *Client) Session() *mcp.ClientSession {
	return c.session
}

// Close closes the official SDK session. It is idempotent and concurrency
// safe.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}

	if err := c.session.Close(); err != nil {
		return fmt.Errorf("agent/mcp: close: %w", err)
	}

	return nil
}

// ToolListChanged returns a coalescing signal for
// notifications/tools/list_changed. On receipt, call Tools to obtain a fresh
// snapshot for the next Agent. The channel is not closed by Close.
func (c *Client) ToolListChanged() <-chan struct{} {
	return c.toolListChanged
}

// Tools lists all remote tools across MCP pagination and returns an immutable
// agent tool snapshot. The operation rejects the entire snapshot on malformed
// definitions, mapped-name collisions, or the configured tool limit.
func (c *Client) Tools(ctx context.Context) ([]agent.Tool, error) {
	if c == nil || c.tools == nil {
		return nil, errors.New("agent/mcp: client is not connected")
	}

	tools := make([]agent.Tool, 0)
	remoteByName := make(map[string]string)

	for remote, err := range c.tools.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("agent/mcp: list tools: %w", err)
		}

		if remote == nil {
			return nil, errors.New("agent/mcp: server returned a nil tool")
		}

		if len(tools) >= c.maxTools {
			return nil, fmt.Errorf("agent/mcp: server returned more than %d tools", c.maxTools)
		}

		tool, err := c.adaptTool(remote)
		if err != nil {
			return nil, err
		}

		name := tool.Decl().Name
		if previous, exists := remoteByName[name]; exists {
			return nil, fmt.Errorf(
				"agent/mcp: remote tools %q and %q map to duplicate name %q",
				previous,
				remote.Name,
				name,
			)
		}

		remoteByName[name] = remote.Name

		tools = append(tools, tool)
	}

	return tools, nil
}

func (c *Client) adaptTool(remote *mcp.Tool) (*Tool, error) {
	if remote.Name == "" {
		return nil, errors.New("agent/mcp: server returned a tool with an empty name")
	}

	name, err := c.nameMapper(remote.Name)
	if err != nil {
		return nil, fmt.Errorf("agent/mcp: map tool name %q: %w", remote.Name, err)
	}

	if err := validatePortableName(name, 0); err != nil {
		return nil, fmt.Errorf("agent/mcp: mapped tool name %q: %w", remote.Name, err)
	}

	if c.namePrefix != "" {
		name = c.namePrefix + "_" + name
	}

	name = shortenToolName(name)

	schema, err := convertInputSchema(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("agent/mcp: tool %q input schema: %w", remote.Name, err)
	}

	return &Tool{
		client:     c,
		remoteName: remote.Name,
		decl: ai.Tool{
			Name:        name,
			Description: remote.Description,
			InputSchema: schema,
		},
	}, nil
}

func convertInputSchema(input any) (*ai.Schema, error) {
	if input == nil {
		return nil, errors.New("missing schema")
	}

	data, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("must be a JSON object")
	}

	schema, err := ai.ParseSchema(trimmed)
	if err != nil {
		return nil, err
	}

	return schema, nil
}

func defaultToolName(remoteName string) (string, error) {
	var name strings.Builder

	for _, char := range remoteName {
		if portableToolNameChar(char) {
			name.WriteRune(char)
		} else {
			name.WriteByte('_')
		}
	}

	return name.String(), nil
}

func validatePortableName(name string, maxLen int) error {
	if name == "" {
		return errors.New("name is empty")
	}

	if maxLen > 0 && len(name) > maxLen {
		return fmt.Errorf("name exceeds %d bytes", maxLen)
	}

	for _, char := range name {
		if !portableToolNameChar(char) {
			return fmt.Errorf("name contains unsupported character %q", char)
		}
	}

	return nil
}

func portableToolNameChar(char rune) bool {
	return char >= 'a' && char <= 'z' ||
		char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' ||
		char == '_' || char == '-'
}

func shortenToolName(name string) string {
	if len(name) <= maxToolNameLen {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:4])

	return name[:maxToolNameLen-len(suffix)] + suffix
}

func (c *Client) handleToolListChanged() {
	select {
	case c.toolListChanged <- struct{}{}:
	default:
	}
}

func (c *Client) registerProgress(ctx context.Context) (string, func()) {
	token := "pips-" + strconv.FormatUint(c.progressID.Add(1), 10)
	report := func(part ai.Part) {
		agent.ReportProgress(ctx, part)
	}

	c.progressMu.Lock()
	c.progress[token] = report
	c.progressMu.Unlock()

	return token, func() {
		c.unregisterProgress(token)
	}
}

func (c *Client) unregisterProgress(token string) {
	c.progressMu.Lock()
	delete(c.progress, token)
	c.progressMu.Unlock()
}

func (c *Client) handleProgress(req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}

	token, ok := req.Params.ProgressToken.(string)
	if !ok {
		return
	}

	c.progressMu.RLock()
	defer c.progressMu.RUnlock()

	report := c.progress[token]

	if report != nil {
		report(ai.Text(progressText(req.Params)))
	}
}

func progressText(progress *mcp.ProgressNotificationParams) string {
	if progress.Message != "" {
		return progress.Message
	}

	if progress.Total != 0 {
		return fmt.Sprintf("MCP progress: %g/%g", progress.Progress, progress.Total)
	}

	return fmt.Sprintf("MCP progress: %g", progress.Progress)
}
