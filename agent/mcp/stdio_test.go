package agentmcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runStdioServerEnv = "PIPS_MCP_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(runStdioServerEnv) == "1" {
		runStdioTestServer()

		return
	}

	os.Exit(m.Run())
}

func runStdioTestServer() {
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-server", Version: "v1"}, nil)
	server.AddTool(rawTool("echo"), func(
		context.Context,
		*mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "stdio ready"},
		}}, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "stdio MCP server: %v\n", err)

		os.Exit(2)
	}
}

func TestConnectUsesCommandTransport(t *testing.T) {
	t.Parallel()

	switch runtime.GOOS {
	case "darwin", "linux", "windows":
	default:
		t.Skip("os/exec is unsupported on this platform")
	}

	executable, err := os.Executable()
	require.NoError(t, err)

	// #nosec G204 -- the command is the current test binary, not user input.
	command := exec.CommandContext(t.Context(), executable)

	command.Env = append(os.Environ(), runStdioServerEnv+"=1")

	client, err := Connect(
		t.Context(),
		&mcp.Implementation{Name: "stdio-client", Version: "v1"},
		&mcp.CommandTransport{Command: command},
	)
	require.NoError(t, err)

	tools, err := client.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	parts, err := tools[0].Exec(t.Context(), agent.ToolCall{})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("stdio ready")}, parts)

	require.NoError(t, client.Close())
}
