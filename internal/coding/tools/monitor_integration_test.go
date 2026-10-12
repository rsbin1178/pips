package tools_test

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// monitorRecorder captures one registry's model-visible output.
type monitorRecorder struct {
	mu          sync.Mutex
	events      []background.Event
	completions []background.Completion
}

func (r *monitorRecorder) BackgroundStarted(background.Snapshot) {}

func (r *monitorRecorder) BackgroundEvent(event background.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *monitorRecorder) BackgroundCompleted(completion background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.completions = append(r.completions, completion)
}

func (r *monitorRecorder) reported() ([]background.Event, []background.Completion) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]background.Event(nil), r.events...), append([]background.Completion(nil), r.completions...)
}

// monitorFixture is one real approval-authored monitor path: a workspace, a
// durable session journal, the platform sandbox, and the registry the handler
// hands its process to.
type monitorFixture struct {
	session    *harness.Session
	controller *approval.Controller
	registry   *background.Registry
	recorder   *monitorRecorder
}

func newMonitorFixture(t *testing.T) monitorFixture {
	t.Helper()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	session, err := harness.NewSession(harness.NewMemoryStore("monitor-integration"))
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

	recorder := &monitorRecorder{}
	registry, err := background.NewRegistry(background.Options{
		OutputDir:    t.TempDir(),
		Observer:     recorder,
		KillToolName: background.KillTaskToolName,
		OutputBytes:  1 << 20,
		TailBytes:    4096,
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
		tools.NewMonitorHandler(registry),
	)
	require.NoError(t, err)

	return monitorFixture{
		session: session, controller: controller, registry: registry, recorder: recorder,
	}
}

// startMonitor authorizes and starts one monitor exactly as the model would.
func (f monitorFixture) startMonitor(t *testing.T, args string) (*ai.TextPart, background.Snapshot) {
	t.Helper()

	monitor, ok := f.controller.Tool(tools.MonitorName)
	require.True(t, ok)

	call := agent.ToolCall{ID: "call-1", Name: tools.MonitorName, Args: ai.JSON(args)}

	decision := f.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := monitor.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, _, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.NotNil(t, header.Monitor)

	snapshot, found := f.registry.Snapshot(header.Monitor.TaskID)
	require.True(t, found)

	return &text, snapshot
}

// waitForCompletion blocks until the recorder has seen count completions and
// returns them.
func (f monitorFixture) waitForCompletion(t *testing.T, count int) ([]background.Event, []background.Completion) {
	t.Helper()

	require.Eventually(t, func() bool {
		_, completions := f.recorder.reported()

		return len(completions) >= count
	}, 30*time.Second, 10*time.Millisecond)

	return f.recorder.reported()
}

// TestMonitorIntegrationStreamsAndEndsNaturally drives a real command that
// produces output over time and ends on its own.
func TestMonitorIntegrationStreamsAndEndsNaturally(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	fixture := newMonitorFixture(t)

	receipt, snapshot := fixture.startMonitor(t,
		`{"command":"for i in 1 2 3 4; do echo line-$i; sleep 0.3; done",`+
			`"description":"four lines"}`,
	)

	assert.Contains(t, receipt.Text, "Monitor started (task monitor-1, timeout 36000000ms).")
	assert.Equal(t, "monitor-1", snapshot.ID)
	assert.False(t, snapshot.Completed)

	events, completions := fixture.waitForCompletion(t, 1)

	require.Len(t, completions, 1)
	completion := completions[0]

	assert.True(t, completion.Snapshot.Completed)
	assert.Equal(t, 0, completion.Snapshot.ExitCode)
	assert.Equal(t, "four lines", completion.Snapshot.Description)
	assert.Contains(t, completion.Tail, "line-4")

	// Every line the command printed reaches the model, and no event is a
	// terminal one: the completion is the only terminal report.
	streamed := make([]string, 0, len(events))
	for _, event := range events {
		streamed = append(streamed, event.Text)
		assert.NotContains(t, event.Text, "[monitor ended:")
	}

	joined := strings.Join(streamed, "\n")

	for _, line := range []string{"line-1", "line-2", "line-3", "line-4"} {
		assert.Contains(t, joined, line)
	}

	text := background.MonitorCompletionText(completion)
	assert.Contains(t, text, "[monitor ended: exited (code 0)].")
	assert.Contains(t, text, "Description: four lines | Duration:")
	assert.NotContains(t, text, "for i in 1 2 3 4")
}

// TestMonitorIntegrationTimesOut drives a real command that outlives its
// deadline.
func TestMonitorIntegrationTimesOut(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	fixture := newMonitorFixture(t)

	fixture.startMonitor(t,
		`{"command":"while true; do echo tick; sleep 0.2; done",`+
			`"description":"endless","timeout_ms":600}`,
	)

	_, completions := fixture.waitForCompletion(t, 1)
	completion := completions[0]

	assert.True(t, completion.Snapshot.Completed)
	assert.False(t, completion.Killed)
	assert.Equal(t, -1, completion.Snapshot.ExitCode)
	assert.Contains(t, background.MonitorCompletionText(completion), "[monitor ended: ended].")

	require.Eventually(t, func() bool {
		stored, ok := fixture.registry.Snapshot(completion.Snapshot.ID)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)
}

// TestMonitorIntegrationKills stops a real command through the kill surface.
func TestMonitorIntegrationKills(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	fixture := newMonitorFixture(t)

	_, snapshot := fixture.startMonitor(t,
		`{"command":"while true; do echo tick; sleep 0.2; done","description":"endless","persistent":true}`,
	)

	require.Equal(t, background.OutcomeKilled, fixture.registry.Kill(snapshot.ID, background.KillModel))

	_, completions := fixture.waitForCompletion(t, 1)
	completion := completions[0]

	assert.True(t, completion.Killed)
	assert.True(t, completion.KillResultDelivered)
	assert.Contains(t, background.MonitorCompletionText(completion), "[monitor ended: ended].")
	assert.NotContains(t, background.MonitorCompletionText(completion), "do not restart")
}

// TestMonitorIntegrationRecordsTheRunInTheApprovalJournal proves a monitor runs
// through the same durable permit and receipt path as shell, under the same call
// id, so the change audit sees it.
func TestMonitorIntegrationRecordsTheRunInTheApprovalJournal(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	fixture := newMonitorFixture(t)

	fixture.startMonitor(t, `{"command":"echo hi","description":"journaled"}`)
	fixture.waitForCompletion(t, 1)

	var receipts []string

	for _, entry := range fixture.session.Entries() {
		if entry.Kind == harness.KindCustom && strings.Contains(string(entry.Data), "monitor") {
			receipts = append(receipts, string(entry.Data))
		}
	}

	require.NotEmpty(t, receipts, "the monitor call must be journaled")
	assert.Contains(t, strings.Join(receipts, "\n"), "call-1")
	assert.Contains(t, strings.Join(receipts, "\n"), "monitor")
}
