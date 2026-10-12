package background

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The two batch shapes are the delivery contract: the model sees the single
// event shape for one event and the counted, per-monitor shape for more, both
// with the event's own wrapped text as the body source.
func TestMonitorBatchTextRendersSingleEventVerbatim(t *testing.T) {
	t.Parallel()

	event := Event{
		TaskID: "monitor-1", Description: "deploy log",
		Text:    "FAILED",
		Wrapped: wrapEvent("deploy log", "FAILED", "monitor-1"),
	}

	assert.Equal(t, `<monitor-event task_id="monitor-1">
[deploy log] FAILED
</monitor-event>`, MonitorBatchText([]Event{event}, KillTaskToolName))
}

func TestMonitorBatchTextFallsBackToTheEventLabel(t *testing.T) {
	t.Parallel()

	event := Event{TaskID: "monitor-1", Text: "raw body", Wrapped: "raw body"}

	assert.Equal(t, `<monitor-event task_id="monitor-1">
[event] raw body
</monitor-event>`, MonitorBatchText([]Event{event}, KillTaskToolName))
}

func TestMonitorBatchTextGroupsMultipleEventsByMonitor(t *testing.T) {
	t.Parallel()

	events := []Event{
		{TaskID: "monitor-1", Wrapped: wrapEvent("first watch", "one", "monitor-1")},
		{TaskID: "monitor-2", Wrapped: wrapEvent("second watch", "alpha", "monitor-2")},
		{TaskID: "monitor-1", Wrapped: wrapEvent("first watch", "two", "monitor-1")},
	}

	// N is the event count and M the monitor count; groups keep first-seen
	// order and each group numbers its own lines from one.
	assert.Equal(t, `3 monitor events from 2 monitors (use kill_background_task to identify each monitor):

<monitor description="first watch" task_id="monitor-1">
[1] one
[2] two
</monitor>

<monitor description="second watch" task_id="monitor-2">
[1] alpha
</monitor>`, MonitorBatchText(events, KillTaskToolName))
}

func TestMonitorBatchTextUsesTheSingularNounForOneMonitor(t *testing.T) {
	t.Parallel()

	events := []Event{
		{TaskID: "monitor-1", Wrapped: wrapEvent("watch", "one", "monitor-1")},
		{TaskID: "monitor-1", Wrapped: wrapEvent("watch", "two", "monitor-1")},
	}

	assert.Contains(t, MonitorBatchText(events, KillTaskToolName), "2 monitor events from 1 monitor (use ")

	// An empty batch renders nothing, so a drain with no surviving event
	// injects no message at all.
	assert.Empty(t, MonitorBatchText(nil, KillTaskToolName))
}

func TestMonitorBatchTextSanitizesTheDescriptionNotTheBody(t *testing.T) {
	t.Parallel()

	event := Event{
		TaskID: "monitor-1",
		Wrapped: `<monitor-event description="say &quot;hi&quot;
next" task_id="monitor-1">` + "\n" + `<b>body " verbatim</b>` + "\n</monitor-event>",
	}

	text := MonitorBatchText([]Event{event}, KillTaskToolName)
	assert.Contains(t, text, `[say &quot;hi&quot; next] `)
	assert.Contains(t, text, `<b>body " verbatim</b>`)
}

func TestMonitorCompletionTextPrefersTheDescription(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	completion := Completion{
		Snapshot: Snapshot{
			ID: "monitor-1", Command: "tail -f secret.log", Description: "deploy log",
			Start: start, End: start.Add(2500 * time.Millisecond), ExitCode: 0,
		},
		Tail: "last line",
	}

	text := MonitorCompletionText(completion)
	assert.Equal(t,
		"Monitor \"monitor-1\" ended: [monitor ended: exited (code 0)].\n"+
			"Description: deploy log | Duration: 2.5s\n"+
			"last line\n", text)
	assert.NotContains(t, text, "secret.log")
}

func TestMonitorCompletionTextNamesASignalAndTheUserKill(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	completion := Completion{
		Snapshot: Snapshot{
			ID: "monitor-2", Start: start, End: start.Add(time.Second),
			ExitCode: -1, Signal: "term",
		},
		Killed: true,
	}

	assert.Equal(t,
		"Monitor \"monitor-2\" ended: [monitor ended: killed by signal term].\n"+
			"Duration: 1.0s\n"+
			userKilledNotice, MonitorCompletionText(completion))

	// A kill the model already saw as a Tool result carries no notice: that
	// result is the one completion it gets.
	completion.KillResultDelivered = true
	assert.NotContains(t, MonitorCompletionText(completion), userKilledNotice)
}

func TestMonitorRunningWarningListsTheOtherTasks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 12, 10, 0, 30, 0, time.UTC)
	running := []Snapshot{
		{ID: "monitor-1", Description: "build log", Start: now.Add(-12 * time.Second)},
		{ID: "monitor-2", Start: now.Add(-3 * time.Second)},
	}

	assert.Equal(t,
		"Note: 2 other background tasks are still running:\n"+
			"- \"monitor-1\" (running for 12s): build log\n"+
			"- \"monitor-2\" (running for 3s)\n"+
			"Consider killing duplicate tasks with kill_background_task before launching new ones.",
		MonitorRunningWarning(running, now, KillTaskToolName))

	assert.Equal(t,
		"Note: 1 other background task is still running:\n"+
			"- \"monitor-1\" (running for 12s): build log\n"+
			"Consider killing duplicate tasks with kill_background_task before launching new ones.",
		MonitorRunningWarning(running[:1], now, KillTaskToolName))

	assert.Empty(t, MonitorRunningWarning(nil, now, KillTaskToolName))
}
