package tools_test

import (
	"context"
	"os"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellIntegration(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	session, err := harness.NewSession(harness.NewMemoryStore("shell-integration"))
	require.NoError(t, err)

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	})
	require.NoError(t, err)

	tempRoot := t.TempDir()
	//nolint:gosec // The executor contract requires a traversable owner-only directory.
	require.NoError(t, os.Chmod(tempRoot, 0o700))
	executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
		TempRoot:    tempRoot,
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	controller, err := approval.New(
		ws,
		session,
		unusedResolver{},
		unusedPendingRunner{},
		policy,
		executor,
		tools.NewShellHandler(),
	)
	require.NoError(t, err)

	shell, ok := controller.Tool("shell")
	require.True(t, ok)

	call := agent.ToolCall{
		ID:   "call-1",
		Name: "shell",
		Args: ai.JSON(`{"command":"printf 'hello\\n'; printf 'warn\\n' >&2"}`),
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action)

	parts, err := shell.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	require.NotNil(t, header.Execution)
	assert.Equal(t, "exited", header.Execution.Status)
	assert.Equal(t, "stdout:\nhello\nstderr:\nwarn\n", body)
}

type unusedResolver struct{}

func (unusedResolver) ResolveToolCalls(...agent.ToolResolution) error { return assert.AnError }

type unusedPendingRunner struct{}

func (unusedPendingRunner) RunPending(
	context.Context,
	agent.ToolCall,
	execution.Sink,
) ([]ai.Part, error) {
	return nil, assert.AnError
}
