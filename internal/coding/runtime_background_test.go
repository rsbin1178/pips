package coding

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalCatalogOffersTheMonitorInTheShellClass(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	local, err := tools.NewCatalog(
		tree,
		tools.DefaultLimits(),
		tools.WithControlledShell(testCatalogTool("shell")),
		tools.WithControlledMonitor(testCatalogTool(tools.MonitorName)),
	)
	require.NoError(t, err)

	privileged, err := local.Snapshot(t.Context(), catalog.AllowAll("workspace", catalog.RiskPrivileged))
	require.NoError(t, err)
	assert.Contains(t, agentToolNames(privileged), "shell")
	assert.Contains(t, agentToolNames(privileged), tools.MonitorName)

	// A monitor starts a process, so it is privileged like shell: a read-only
	// policy removes both.
	readOnly, err := local.Snapshot(t.Context(), catalog.AllowAll("workspace", catalog.RiskRead))
	require.NoError(t, err)
	assert.NotContains(t, agentToolNames(readOnly), tools.MonitorName)
	assert.NotContains(t, agentToolNames(readOnly), "shell")
}

// TestMonitorIsNotDelegatedToChildrenYet records the deliberate phase split:
// pips offers the monitor to the conversation, and a child catalog is built from
// the delegable allowlist, which does not carry it yet. A later phase that adds
// child watches updates this expectation on purpose.
func TestMonitorIsNotDelegatedToChildrenYet(t *testing.T) {
	t.Parallel()

	monitor := catalog.Descriptor{
		Name:   tools.MonitorName,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
		Risk:   catalog.RiskPrivileged,
	}

	// The delegable allowlist is the authority filter, so a child plan never
	// carries the monitor whatever its capability mode is. The mode table also
	// keeps it out of an execute child, so adding the allowlist entry later is
	// the only change a child watch needs.
	assert.False(t, delegableDescriptor(monitor))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityExecute, monitor))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadOnly, monitor))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadWrite, monitor))
}

func TestRuntimeReapsItsBackgroundTasksOnClose(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	monitor, ok := runtime.controller.Tool(tools.MonitorName)
	require.True(t, ok, "the runtime must register the monitor wrapper")

	call := agent.ToolCall{
		ID:   "call-monitor",
		Name: tools.MonitorName,
		Args: ai.JSON(`{"command":"echo line-1; sleep 30","description":"reap me","persistent":true}`),
	}

	decision := runtime.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := monitor.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.Monitor)
	assert.True(t, header.Monitor.Persistent)
	assert.Contains(t, body, "persistent -- runs until "+background.KillTaskToolName)

	taskID := header.Monitor.TaskID

	// The pipeline reads what the command wrote and hands it to the Runtime's
	// own observer, which is the seam the delivery path reads.
	require.Eventually(t, func() bool {
		events, _ := runtime.backgroundOutput.pendingBackground()

		return len(events) > 0
	}, 30*time.Second, 10*time.Millisecond)

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, runtime.Close(closeCtx))

	// Closing reaps the task: it is terminal and no new background work starts.
	stored, found := runtime.backgroundTasks.Snapshot(taskID)
	require.True(t, found)
	assert.True(t, stored.Completed)

	_, err = runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "echo late", NoDeadline: true,
		Start: func(context.Context, execution.Sink) (execution.Result, error) {
			return execution.Result{Status: execution.StatusExited}, nil
		},
	})
	require.ErrorIs(t, err, background.ErrClosed)
}
