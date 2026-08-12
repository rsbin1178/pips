package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// ToolError reports an MCP tool-level failure (`isError: true`). Protocol and
// transport failures remain wrapped SDK errors instead.
type ToolError struct {
	RemoteName string
	Message    string
}

// Error implements error.
func (e *ToolError) Error() string {
	return fmt.Sprintf("agent/mcp: tool %q failed: %s", e.RemoteName, e.Message)
}

// Tool adapts one remote MCP tool to [agent.Tool]. It is immutable and safe
// for concurrent use when the remote server supports concurrent calls. The
// bridge does not mark it parallel automatically because MCP annotations are
// untrusted hints.
type Tool struct {
	client     *Client
	remoteName string
	decl       ai.Tool
}

var _ agent.Tool = (*Tool)(nil)

// RemoteName returns the exact server-side MCP tool name.
func (t *Tool) RemoteName() string {
	return t.remoteName
}

// Decl returns the portable declaration advertised to the language model.
func (t *Tool) Decl() ai.Tool {
	return t.decl
}

// Exec forwards a tool call to the MCP server using the Agent's context. This
// propagates cancellation and tool deadlines through the official SDK.
func (t *Tool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	arguments := bytes.TrimSpace(bytes.Clone(call.Args))
	switch {
	case len(arguments) == 0:
		arguments = json.RawMessage(`{}`)
	case !json.Valid(arguments):
		return nil, fmt.Errorf("agent/mcp: tool %q arguments are invalid JSON", t.remoteName)
	case arguments[0] != '{':
		return nil, fmt.Errorf("agent/mcp: tool %q arguments must be a JSON object", t.remoteName)
	}

	params := &mcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: json.RawMessage(arguments),
	}

	token, unregister := t.client.registerProgress(ctx)
	defer unregister()

	params.SetProgressToken(token)

	result, err := t.client.tools.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("agent/mcp: call tool %q: %w", t.remoteName, err)
	}

	if result == nil {
		return nil, fmt.Errorf("agent/mcp: tool %q returned a nil result", t.remoteName)
	}

	parts, err := convertResult(result)
	if err != nil {
		return nil, fmt.Errorf("agent/mcp: tool %q result: %w", t.remoteName, err)
	}

	if result.IsError {
		message := readableError(parts)
		if message == "" {
			message = "remote tool reported an error"
		}

		return nil, &ToolError{RemoteName: t.remoteName, Message: message}
	}

	return parts, nil
}

func readableError(parts []ai.Part) string {
	var messages []string

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok && text.Text != "" {
			messages = append(messages, text.Text)
		}
	}

	return strings.Join(messages, "\n")
}
