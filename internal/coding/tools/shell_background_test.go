package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellBackgroundCeilingMS is the shell's own 30-minute ceiling in
// milliseconds, which a backgrounding call resolves its deadline to.
const shellBackgroundCeilingMS = 1_800_000

// shellFixture is one real approval-authored shell path: a workspace, a durable
// session journal, the platform sandbox, the registry a backgrounded command
// lands in, and the controller that authorizes both the shell and the monitor.
type shellFixture struct {
	controller *approval.Controller
	registry   *background.Registry
	recorder   *monitorRecorder
}

func newShellFixture(t *testing.T, withRegistry bool) shellFixture {
	t.Helper()

	return newShellFixtureWithSandbox(t, withRegistry, config.SandboxWorkspaceWrite)
}

func newShellFixtureWithSandbox(t *testing.T, withRegistry bool, sandbox config.SandboxMode) shellFixture {
	t.Helper()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	session, err := harness.NewSession(harness.NewMemoryStore("shell-background"))
	require.NoError(t, err)

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  sandbox,
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

	recorder := &monitorRecorder{}

	var registry *background.Registry
	if withRegistry {
		registry, err = background.NewRegistry(background.Options{
			OutputDir:    t.TempDir(),
			Observer:     recorder,
			KillToolName: background.KillTaskToolName,
			OutputBytes:  1 << 20,
			TailBytes:    4096,
		})
		require.NoError(t, err)

		// Reap every live task before the fixture's own temp directories are
		// removed, so a killed command's log file is closed first.
		t.Cleanup(func() { _ = registry.Reap(context.Background()) })
	}

	controller, err := approval.New(
		ws,
		session,
		unusedResolver{},
		unusedPendingRunner{},
		policy,
		executor,
		tools.NewShellHandlerForSandboxWithOwner(
			registry, sandbox, config.SandboxNetworkOnRequest, nil,
		),
		tools.NewMonitorHandler(registry),
	)
	require.NoError(t, err)

	return shellFixture{controller: controller, registry: registry, recorder: recorder}
}

// execTool authorizes and executes one controlled call exactly as the model
// would, returning the rendered Tool text.
func (f shellFixture) execTool(t *testing.T, id, name, args string) (string, error) {
	t.Helper()

	tool, ok := f.controller.Tool(name)
	require.True(t, ok)

	call := agent.ToolCall{ID: id, Name: name, Args: ai.JSON(args)}

	decision := f.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := tool.Exec(t.Context(), call)
	if err != nil {
		return "", err
	}

	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text, nil
}

// TestShellBackgroundReturnsAReceiptWhileTheCommandRuns drives the whole path:
// the controller authorizes the call, the shell handler starts the command on
// the registry, and the Tool result is the receipt, not the output.
func TestShellBackgroundReturnsAReceiptWhileTheCommandRuns(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	text, err := fixture.execTool(t, "call-background", "shell", `{"command":"sleep 30","block_until_ms":0}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.Background)
	assert.True(t, strings.HasPrefix(header.Background.TaskID, "shell-"))
	assert.Equal(t, string(background.KindShell), header.Background.Kind)
	assert.Equal(t, int64(shellBackgroundCeilingMS), header.Background.TimeoutMS)
	assert.Contains(t, body, "Command started in the background as task "+header.Background.TaskID)
	assert.Contains(t, body, "You will be notified when it completes")
	assert.Contains(t, body, "get_task_output")
	assert.Contains(t, body, "wait_tasks")
	assert.Contains(t, body, "kill_background_task")

	// The command outlives the call and is still running.
	snapshot, found := fixture.registry.Snapshot(header.Background.TaskID)
	require.True(t, found)
	assert.False(t, snapshot.Completed)
	assert.Equal(t, background.KindShell, snapshot.Kind)

	assert.Equal(t, background.OutcomeKilled,
		fixture.registry.Kill(header.Background.TaskID, background.KillModel))
}

// TestShellPositiveBlockReturnsTheOrdinaryResultWhenTheCommandEnds pins the
// wait's happy path: a command that ends inside the block renders the ordinary
// shell result from the task's own executor result, and nothing is left
// running.
func TestShellPositiveBlockReturnsTheOrdinaryResultWhenTheCommandEnds(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	text, err := fixture.execTool(t, "call-block", "shell",
		`{"command":"printf 'hello\\n'; printf 'warn\\n' >&2","block_until_ms":30000}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	require.NotNil(t, header.Execution)
	assert.Equal(t, "exited", header.Execution.Status)
	assert.Equal(t, "stdout:\nhello\nstderr:\nwarn\n", body)
	assert.Nil(t, header.Background, "a command that ended inside the wait renders the ordinary result")

	tasks := fixture.registry.List()
	require.Len(t, tasks, 1)
	assert.True(t, tasks[0].Completed, "the command ended inside the wait")
}

// TestShellPositiveBlockReturnsAReceiptWhenTheCommandOutlivesTheWait pins the
// other half of a positive block: a command still running at the block moves to
// the background — it is not killed — and the call returns the receipt.
func TestShellPositiveBlockReturnsAReceiptWhenTheCommandOutlivesTheWait(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	text, err := fixture.execTool(t, "call-outlive", "shell",
		`{"command":"sleep 30","block_until_ms":300}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.Background)
	assert.True(t, strings.HasPrefix(header.Background.TaskID, "shell-"))
	assert.Equal(t, string(background.KindShell), header.Background.Kind)
	assert.Equal(t, int64(shellBackgroundCeilingMS), header.Background.TimeoutMS)
	assert.Contains(t, body, "Command started in the background as task "+header.Background.TaskID)

	snapshot, found := fixture.registry.Snapshot(header.Background.TaskID)
	require.True(t, found)
	assert.False(t, snapshot.Completed, "the block must not kill the command")

	assert.Equal(t, background.OutcomeKilled,
		fixture.registry.Kill(header.Background.TaskID, background.KillModel))
}

// TestShellPositiveBlockReportsADeadlineLikeTheForegroundPath pins the render
// parity: a command its own timeout_ms kills inside a positive block is rendered
// from the same result-and-error pair the synchronous path hands the renderer,
// so a deadline reads the same in both modes.
func TestShellPositiveBlockReportsADeadlineLikeTheForegroundPath(t *testing.T) {
	t.Parallel()

	// Each call runs in its own fixture: the controller journals a started call
	// whose result the harness would commit, and this fixture drives the Tool
	// directly, so a second controlled call would pause on the first one's open
	// receipt.
	foreground, err := newShellFixture(t, true).execTool(t, "call-deadline-foreground", "shell",
		`{"command":"sleep 30","timeout_ms":200}`)
	require.Error(t, err)
	assert.Empty(t, foreground)

	foregroundHeader, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	require.False(t, foregroundHeader.OK)
	assert.Equal(t, "deadline_exceeded", foregroundHeader.Code)
	require.NotNil(t, foregroundHeader.Execution)
	assert.Equal(t, "timed_out", foregroundHeader.Execution.Status)

	blocked, err := newShellFixture(t, true).execTool(t, "call-deadline-block", "shell",
		`{"command":"sleep 30","timeout_ms":200,"block_until_ms":30000}`)
	require.Error(t, err)
	assert.Empty(t, blocked)

	blockedHeader, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, blockedHeader.OK)
	assert.Equal(t, foregroundHeader.Code, blockedHeader.Code,
		"the deadline reads the same whether the call blocked or waited")
	require.NotNil(t, blockedHeader.Execution)
	assert.Equal(t, "timed_out", blockedHeader.Execution.Status)
	assert.Nil(t, blockedHeader.Background, "the command ended inside the wait")
}

// TestShellForegroundCallCreatesNoRegistryEntry pins the per-call choice's other
// half: a call with no block runs the synchronous path, so it registers
// nothing.
func TestShellForegroundCallCreatesNoRegistryEntry(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	text, err := fixture.execTool(t, "call-foreground", "shell", `{"command":"printf ok"}`)
	require.NoError(t, err)

	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	require.NotNil(t, header.Execution)
	assert.Equal(t, "stdout:\nok", body)
	assert.Nil(t, header.Background, "a foreground call returns the ordinary result")
	assert.Empty(t, fixture.registry.List(), "a foreground call registers no task")
}

// TestShellDeniedCallNeverLaunches pins the permit's place: a call with no
// before-tool permit is refused before the controller prepares or launches
// anything, so the registry stays empty.
func TestShellDeniedCallNeverLaunches(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	shell, ok := fixture.controller.Tool("shell")
	require.True(t, ok)

	_, err := shell.Exec(t.Context(), agent.ToolCall{
		ID: "call-denied", Name: "shell", Args: ai.JSON(`{"command":"sleep 30","block_until_ms":0}`),
	})
	require.Error(t, err)
	assert.Empty(t, fixture.registry.List(), "a denied call must not launch a command")
}

// TestShellBackgroundWithoutARegistryIsRefused pins the nil-tolerant boundary: a
// handler with no registry refuses a backgrounding call with a named error
// instead of panicking.
func TestShellBackgroundWithoutARegistryIsRefused(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, false)

	_, err := fixture.execTool(t, "call-noregistry", "shell", `{"command":"true","block_until_ms":0}`)
	require.Error(t, err)

	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
	assert.Equal(t, "background_unavailable", header.Reason)
	require.NotNil(t, header.Problem)
	assert.Equal(t, "block_until_ms", header.Problem.Field)
}

// TestMonitorHandlerAlwaysDetaches proves the per-call choice is the shell
// handler's alone: a monitor implements no decider, so the controller always
// routes it to Detach.
func TestMonitorHandlerAlwaysDetaches(t *testing.T) {
	t.Parallel()

	_, ok := any(tools.NewMonitorHandler(nil)).(approval.DetachDecider)
	assert.False(t, ok, "a monitor has no per-call choice")

	_, ok = any(tools.NewShellHandler()).(approval.DetachDecider)
	assert.True(t, ok, "the shell handler answers per call")
}

// TestMonitorCallStillDetachesThroughTheController proves the same rule through
// the real controller: a monitor call returns its receipt and leaves a running
// task, exactly as before the shell kind existed.
func TestMonitorCallStillDetachesThroughTheController(t *testing.T) {
	t.Parallel()

	fixture := newShellFixture(t, true)

	text, err := fixture.execTool(t, "call-monitor", tools.MonitorName,
		`{"command":"sleep 30","description":"watch"}`)
	require.NoError(t, err)

	header, _, err := tools.ParseResult(text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.Background)
	assert.True(t, strings.HasPrefix(header.Background.TaskID, "monitor-"))

	snapshot, found := fixture.registry.Snapshot(header.Background.TaskID)
	require.True(t, found)
	assert.False(t, snapshot.Completed)
	assert.Equal(t, background.KindMonitor, snapshot.Kind)

	assert.Equal(t, background.OutcomeKilled,
		fixture.registry.Kill(header.Background.TaskID, background.KillModel))
}

// TestBackgroundTaskToolDescriptionsNameBothProducers pins the model's map from
// an id back to its producer: both id-addressed Tools name the shell and the
// monitor.
func TestBackgroundTaskToolDescriptionsNameBothProducers(t *testing.T) {
	t.Parallel()

	output := tools.NewTaskOutputTool(nil, nil).Decl()
	assert.Contains(t, output.Description, "shell")
	assert.Contains(t, output.Description, "monitor")

	kill := tools.NewKillBackgroundTool(nil, nil).Decl()
	assert.Contains(t, kill.Description, "shell")
	assert.Contains(t, kill.Description, "monitor")
}

// TestShellBackgroundDeniedByTheSandboxNeverLaunches pins the detached path's
// sandbox invariant: a backgrounding call that asks for a write the sandbox
// denies is refused at the gate, before anything is prepared or launched, while
// a plain backgrounding call in the same sandbox is allowed — so the refusal is
// the requested write and not the backgrounding.
func TestShellBackgroundDeniedByTheSandboxNeverLaunches(t *testing.T) {
	t.Parallel()

	fixture := newShellFixtureWithSandbox(t, true, config.SandboxReadOnly)
	outside := t.TempDir()

	denied := agent.ToolCall{
		ID: "call-sandbox-denied", Name: "shell",
		Args: ai.JSON(`{"command":"touch x","block_until_ms":0,"justification":"write the report",` +
			`"permissions":{"write_paths":["` + outside + `"]}}`),
	}

	decision := fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: denied})
	require.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "sandbox_read_only")

	shell, ok := fixture.controller.Tool("shell")
	require.True(t, ok)

	_, err := shell.Exec(t.Context(), denied)
	require.Error(t, err)
	assert.Empty(t, fixture.registry.List(), "a sandbox-denied call must not launch")

	// A plain backgrounding call in the same read-only sandbox is allowed.
	text, err := fixture.execTool(t, "call-sandbox-allowed", "shell",
		`{"command":"sleep 30","block_until_ms":0}`)
	require.NoError(t, err)

	header, _, err := tools.ParseResult(text)
	require.NoError(t, err)
	require.NotNil(t, header.Background)

	assert.Equal(t, background.OutcomeKilled,
		fixture.registry.Kill(header.Background.TaskID, background.KillModel))
}

// TestShellDetachesAnswersForegroundForAMalformedCall pins the decode boundary
// the controller relies on: a call the handler cannot decode answers
// "foreground", so it reaches the same decode error the synchronous path
// reports rather than being launched in the background.
func TestShellDetachesAnswersForegroundForAMalformedCall(t *testing.T) {
	t.Parallel()

	handler := tools.NewShellHandlerForSandboxWithOwner(
		nil, config.SandboxWorkspaceWrite, config.SandboxNetworkOnRequest, nil,
	)

	for _, args := range []string{
		`{"command":"true","block_until_ms":"soon"}`,
		`{"command":"true","block_until_ms":-1}`,
		`{"command":"true","block_until_ms":null}`,
	} {
		assert.False(t, handler.Detaches(agent.ToolCall{Name: "shell", Args: ai.JSON(args)}), args)
	}

	assert.True(t, handler.Detaches(agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"true","block_until_ms":0}`)}))
	assert.False(t, handler.Detaches(agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"true"}`)}))
}
