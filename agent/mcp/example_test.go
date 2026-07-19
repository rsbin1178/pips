package agentmcp_test

import (
	"context"
	"fmt"
	"log"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent/mcp"
)

func ExampleConnect_stdio() {
	ctx := context.Background()
	transport := &mcp.CommandTransport{Command: exec.CommandContext(ctx, "my-mcp-server")}

	client, err := agentmcp.Connect(
		ctx,
		&mcp.Implementation{Name: "pips-host", Version: "v0.1.0"},
		transport,
		agentmcp.WithToolNamePrefix("workspace"),
	)
	if err != nil {
		log.Print(err)

		return
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Print(err)
		}
	}()

	tools, err := client.Tools(ctx)
	if err != nil {
		log.Print(err)

		return
	}

	fmt.Println(len(tools)) // Pass tools to agent.WithTools.
}
