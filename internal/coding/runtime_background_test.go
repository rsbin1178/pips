package coding

import (
	"context"
	"strings"
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

// TestMonitorAndStopAreDelegatedToExecutingChildren records the phase decision:
// a watch starts and stops processes exactly as shell does, so a child keeps it
// precisely when its capability mode keeps shell. A child's watch registers on
// the session's own registry, so its events reach the one conversation the
// Runtime owns rather than opening a second delivery queue.
func TestMonitorAndStopAreDelegatedToExecutingChildren(t *testing.T) {
	t.Parallel()

	local := catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}
	monitor := catalog.Descriptor{Name: tools.MonitorName, Source: local, Risk: catalog.RiskPrivileged}
	stop := catalog.Descriptor{
		Name: tools.KillBackgroundName, Source: local, Risk: catalog.RiskPrivileged,
	}

	assert.True(t, delegableDescriptor(monitor))
	assert.True(t, delegableDescriptor(stop))

	assert.True(t, capabilityModeAllows(agentprofile.CapabilityExecute, monitor))
	assert.True(t, capabilityModeAllows(agentprofile.CapabilityExecute, stop))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadOnly, monitor))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadOnly, stop))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadWrite, monitor))
	assert.False(t, capabilityModeAllows(agentprofile.CapabilityReadWrite, stop))
}

// toolResultText is the text one request carries for a Tool call, which lives
// in a ToolResultPart rather than in a plain text part.
func toolResultText(request ai.Request, callID string) string {
	for _, message := range request.Messages {
		for _, part := range messageParts(message) {
			result, ok := part.(ai.ToolResultPart)
			if !ok || result.ToolCallID != callID {
				continue
			}

			var text strings.Builder

			for _, content := range result.Content {
				if value, ok := content.(ai.TextPart); ok {
					text.WriteString(value.Text)
				}
			}

			return text.String()
		}
	}

	return ""
}

func messageParts(message ai.Message) []ai.Part {
	parts, err := ai.MessageParts(message)
	if err != nil {
		return nil
	}

	return parts
}

// TestBackgroundTaskLifecycleRoundTripsThroughTheEventCodec pins the wire shape
// the tasks pane reads: a monitor row survives encode and decode unchanged, and
// a completed row without an end time is refused.
func TestBackgroundTaskLifecycleRoundTripsThroughTheEventCodec(t *testing.T) {
	t.Parallel()

	event := Event{
		Schema: EventSchema, Sequence: 1, Time: time.Unix(1, 0).UTC(), SessionID: "session-1",
		Type: EventBackgroundStarted,
		Payload: BackgroundTaskLifecycle{
			ID: "monitor-1", Kind: string(background.KindMonitor),
			Description: "deploy log", Command: "tail -f deploy.log",
			StartedAt: time.Unix(2, 0).UTC(),
		},
	}

	data, err := MarshalEvent(event)
	require.NoError(t, err)

	decoded, err := UnmarshalEvent(data)
	require.NoError(t, err)
	assert.Equal(t, event.Payload, decoded.Payload)
	assert.Equal(t, EventBackgroundStarted, decoded.Type)

	completed := event
	completed.Type = EventBackgroundCompleted
	completed.Payload = BackgroundTaskLifecycle{
		ID: "monitor-1", Kind: string(background.KindMonitor), Completed: true,
		StartedAt: time.Unix(2, 0).UTC(),
	}
	_, err = MarshalEvent(completed)
	assert.Error(t, err, "a completed row without an end time is not a state the pane can read")
}

// TestRuntimeStopsAWatchThroughTheModelStopTool drives the whole stop path with
// real processes: the model starts a persistent watch, then stops it by task id
// through the one control method. The Tool result is that kill's single
// completion, so the terminal report must not wake the model a second time.
func TestRuntimeStopsAWatchThroughTheModelStopTool(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"call-monitor",
			tools.MonitorName,
			`{"command":"sleep 30","description":"watch me","persistent":true}`,
		),
		runtimeToolResponse("call-stop", tools.KillBackgroundName, `{"task_id":"monitor-1"}`),
		runtimeTextResponse("stopped the watch"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("watch then stop")))
	require.Contains(t, eventTypes(events), EventInteractionCompleted)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Contains(t, toolResultText(requests[1], "call-monitor"), `"task_id":"monitor-1"`)
	assert.Contains(t, toolResultText(requests[2], "call-stop"), `"outcome":"killed"`)

	// The watched command prints nothing, so the one report the observer holds
	// is the kill's terminal completion.
	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	stored, found := runtime.backgroundTasks.Snapshot("monitor-1")
	require.True(t, found)
	require.Eventually(t, func() bool {
		current, ok := runtime.backgroundTasks.Snapshot("monitor-1")

		return ok && current.Completed
	}, 30*time.Second, 10*time.Millisecond)
	assert.True(t, stored.Start.Before(time.Now()))

	// No queue record is produced for that task: the Tool result was its wake.
	require.NoError(t, runtime.backgroundOutput.deliverBackground())
	pending, err := runtime.notifications.PendingMessages()
	require.NoError(t, err)

	for _, message := range pending {
		assert.NotEqual(t, "monitor-1", message.TaskID, "a delivered kill must not wake the model twice")
	}

	// The pane row settles from the same projection the events carry.
	require.Eventually(t, func() bool {
		for _, task := range runtime.Snapshot().BackgroundTasks {
			if task.ID == "monitor-1" {
				return task.Completed
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)
}

// TestRuntimeInspectsAWatchThroughTheControlSurface runs a real watch and reads
// the bounded view the tasks pane's inspect action shows.
func TestRuntimeInspectsAWatchThroughTheControlSurface(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	monitor, ok := runtime.controller.Tool(tools.MonitorName)
	require.True(t, ok, "the runtime must register the monitor wrapper")

	call := agent.ToolCall{
		ID:   "call-monitor",
		Name: tools.MonitorName,
		Args: ai.JSON(`{"command":"printf 'inspected-line\n'; sleep 30","description":"watch me","persistent":true}`),
	}
	decision := runtime.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := monitor.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	startHeader, _, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.NotNil(t, startHeader.Monitor)
	taskID := startHeader.Monitor.TaskID

	require.Eventually(t, func() bool {
		inspection, inspectErr := runtime.InspectBackgroundTask(t.Context(), taskID)

		return inspectErr == nil && strings.Contains(inspection.Output, "inspected-line")
	}, 30*time.Second, 10*time.Millisecond)

	inspection, err := runtime.InspectBackgroundTask(t.Context(), taskID)
	require.NoError(t, err)
	assert.Equal(t, "watch me", inspection.Task.Description)
	assert.False(t, inspection.Task.Completed)
	assert.Equal(t, taskID, inspection.Lifecycle().ID)
	assert.Equal(t, string(background.KindMonitor), inspection.Lifecycle().Kind)

	_, err = runtime.InspectBackgroundTask(t.Context(), "monitor-404")
	require.ErrorIs(t, err, ErrRuntimeInvalid)
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
	// own observer, which is the seam the delivery path renders from.
	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() > 0
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
