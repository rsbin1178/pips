package background

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineProcessorAssemblesTrimsAndSkipsEmptyLines(t *testing.T) {
	t.Parallel()

	processor := &LineProcessor{}

	assert.Empty(t, processor.Push([]byte("  \n")))
	assert.Empty(t, processor.Push([]byte("one")))

	lines := processor.Push([]byte("two\n three \n\nfour"))
	assert.Equal(t, []string{"onetwo", "three"}, lines)

	partial, ok := processor.Flush()
	require.True(t, ok)
	assert.Equal(t, "four", partial)

	_, ok = processor.Flush()
	assert.False(t, ok)
}

func TestLineProcessorReplacesInvalidUTF8(t *testing.T) {
	t.Parallel()

	processor := &LineProcessor{}
	lines := processor.Push([]byte{'a', 0xff, 'b', '\n'})

	require.Len(t, lines, 1)
	assert.Equal(t, "a\uFFFDb", lines[0])
}

func TestLineTruncationIsByteBoundedAndRuneSafe(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("a", LineLimit)

	tests := []struct {
		name  string
		line  string
		want  string
		liner string
	}{
		{
			name: "exactly at the limit is kept whole",
			line: exact,
			want: exact,
		},
		{
			name: "one byte over is truncated with the literal suffix",
			line: exact + "a",
			want: exact + "...(truncated)",
		},
		{
			name: "a multi-byte line is cut on a rune boundary",
			line: strings.Repeat("日", 200),
			want: strings.Repeat("日", LineLimit/3) + "...(truncated)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			processor := &LineProcessor{}
			lines := processor.Push([]byte(test.line + "\n"))

			require.Len(t, lines, 1)
			assert.Equal(t, test.want, lines[0])
		})
	}
}

func TestLineBatchTruncation(t *testing.T) {
	t.Parallel()

	processor := &LineProcessor{}
	line := strings.Repeat("b", LineLimit)

	lines := processor.Push([]byte(strings.Repeat(line+"\n", 10)))
	require.Len(t, lines, 10)

	batch := batchLines(lines)
	require.Len(t, batch, BatchLimit+len(batchSuffix))
	require.True(t, strings.HasSuffix(batch, batchSuffix))

	body := strings.TrimSuffix(batch, batchSuffix)
	assert.Len(t, body, BatchLimit)
	assert.True(t, strings.HasPrefix(strings.Join(lines, "\n"), body))
}

func TestLineProcessorKeepsTheBufferTail(t *testing.T) {
	t.Parallel()

	processor := &LineProcessor{}

	// A script that never emits a newline must not grow the buffer past its cap:
	// the most recent bytes survive, so a line that arrives after a huge
	// unterminated run is still read.
	processor.Push([]byte(strings.Repeat("x", BufferLimit)))
	lines := processor.Push([]byte("y\ny2\n"))

	require.Len(t, lines, 2)
	assert.Equal(t, strings.Repeat("x", LineLimit)+lineSuffix, lines[0])
	assert.Equal(t, "y2", lines[1])

	_, ok := processor.Flush()
	assert.False(t, ok)
}

func TestWrapEventRoundTripsAndSanitizesTheDescription(t *testing.T) {
	t.Parallel()

	wrapped := wrapEvent("watch \"deploy\"\nstep 2", "line-1", "monitor-1")

	assert.Equal(
		t,
		"<monitor-event description=\"watch 'deploy' step 2\" task_id=\"monitor-1\">\nline-1\n</monitor-event>",
		wrapped,
	)

	description, inner, ok := splitWrappedEvent(wrapped)
	require.True(t, ok)
	assert.Equal(t, "watch 'deploy' step 2", description)
	assert.Equal(t, "line-1", inner)

	_, _, ok = splitWrappedEvent("not wrapped")
	assert.False(t, ok)
}

func TestSingleEventBatchShape(t *testing.T) {
	t.Parallel()

	event := backgroundEvent("monitor-1", "deploy", "line-1")

	assert.Equal(
		t,
		"<monitor-event task_id=\"monitor-1\">\n[deploy] line-1\n</monitor-event>",
		MonitorBatchText([]Event{event}, KillTaskToolName),
	)
	assert.Empty(t, MonitorBatchText(nil, KillTaskToolName))
}

func TestSingleEventBatchFallsBackToTheEventLabel(t *testing.T) {
	t.Parallel()

	// An event whose wrapped text cannot be parsed keeps its text verbatim and
	// takes the fallback label.
	event := Event{TaskID: "monitor-1", Text: "raw", Wrapped: "raw"}

	assert.Equal(
		t,
		"<monitor-event task_id=\"monitor-1\">\n[event] raw\n</monitor-event>",
		MonitorBatchText([]Event{event}, KillTaskToolName),
	)
}

func TestMultiEventBatchGroupsPerMonitorInFirstSeenOrder(t *testing.T) {
	t.Parallel()

	events := []Event{
		backgroundEvent("monitor-1", "deploy", "one"),
		backgroundEvent("monitor-2", "ci", "alpha"),
		backgroundEvent("monitor-1", "deploy", "two"),
	}

	assert.Equal(
		t,
		"3 monitor events from 2 monitors (use kill_background_task to identify each monitor):\n\n"+
			"<monitor description=\"deploy\" task_id=\"monitor-1\">\n[1] one\n[2] two\n</monitor>\n\n"+
			"<monitor description=\"ci\" task_id=\"monitor-2\">\n[1] alpha\n</monitor>",
		MonitorBatchText(events, KillTaskToolName),
	)
}

func TestSingleMonitorBatchUsesTheSingularNoun(t *testing.T) {
	t.Parallel()

	events := []Event{
		backgroundEvent("monitor-1", "deploy", "one"),
		backgroundEvent("monitor-1", "deploy", "two"),
	}

	text := MonitorBatchText(events, KillTaskToolName)
	assert.True(t, strings.HasPrefix(text, "2 monitor events from 1 monitor (use "), text)
}

func TestCompletionTextPrefersTheDescriptionAndPointsAtTheTool(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	completion := Completion{
		Snapshot: Snapshot{
			ID: "monitor-1", Command: "deploy --secret", Description: "deploy log",
			Start: start, End: start.Add(1500 * time.Millisecond),
			ExitCode: 0, Completed: true,
		},
	}

	text := MonitorCompletionText(completion)

	assert.Equal(
		t,
		"Monitor \"monitor-1\" ended: [monitor ended: exited (code 0)].\n"+
			"Description: deploy log | Duration: 1.5s\n"+
			"Use get_task_output(\"monitor-1\") for full output.\n",
		text,
	)
	assert.NotContains(t, text, "deploy --secret")
}

func TestCompletionTextStatesEndedAndTheDoNotRestartNotice(t *testing.T) {
	t.Parallel()

	completion := Completion{
		Snapshot: Snapshot{ID: "monitor-2", ExitCode: -1, Completed: true, Description: "log tail"},
		Killed:   true,
	}

	text := MonitorCompletionText(completion)
	assert.Contains(t, text, "[monitor ended: ended].")
	assert.Contains(t, text, "This task was killed by the user — do not restart it.")

	// A kill whose outcome the model already received adds no notice.
	completion.KillResultDelivered = true
	assert.NotContains(t, MonitorCompletionText(completion), "do not restart")
}

func TestCompletionTextNamesASignal(t *testing.T) {
	t.Parallel()

	completion := Completion{
		Snapshot: Snapshot{ID: "monitor-3", Signal: "term", ExitCode: -1, Completed: true},
	}

	assert.Contains(t, MonitorCompletionText(completion), "[monitor ended: killed by signal term].")
}

func TestRunningWarning(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	running := []Snapshot{
		{ID: "monitor-1", Description: "deploy", Start: start},
		{ID: "monitor-2", Start: start},
	}

	assert.Equal(
		t,
		"Note: 2 other background tasks are still running:\n"+
			"- \"monitor-1\" (running for 3s): deploy\n"+
			"- \"monitor-2\" (running for 3s)\n"+
			"Consider killing duplicate tasks with kill_background_task before launching new ones.",
		MonitorRunningWarning(running, start.Add(3*time.Second), ""),
	)

	assert.Contains(
		t,
		MonitorRunningWarning(running[:1], start, KillTaskToolName),
		"Note: 1 other background task is still running:",
	)
	assert.Empty(t, MonitorRunningWarning(nil, start, KillTaskToolName))
}

func backgroundEvent(taskID, description, text string) Event {
	return Event{
		TaskID: taskID, Description: description, Text: text,
		Wrapped: wrapEvent(description, text, taskID),
	}
}

func TestSplitWrappedEventToleratesQuotesInTheDescription(t *testing.T) {
	t.Parallel()

	// wrapEvent sanitizes quotes, so a raw tag with a quoted description is the
	// only way the inverse can see one; the last attribute marker still wins.
	wrapped := "<monitor-event description=\"a &quot;b&quot; tag\" task_id=\"monitor-1\">\nbody\n</monitor-event>"

	description, inner, ok := splitWrappedEvent(wrapped)
	require.True(t, ok)
	assert.Equal(t, "a &quot;b&quot; tag", description)
	assert.Equal(t, "body", inner)
}
