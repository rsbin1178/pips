package background_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startWritingThenBlocking writes one line and then runs until its context
// ends, standing in for a watch whose output is readable while it still runs.
func startWritingThenBlocking(line string) background.Start {
	return func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		_ = sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStdout, Data: []byte(line + "\n"),
		})

		<-ctx.Done()

		return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
	}
}

// TestTaskOutputReadsARunningTask pins the running record: no end, no exit code,
// a live dump, and the file the model can read for the rest.
func TestTaskOutputReadsARunningTask(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, startWritingThenBlocking("first-line"), nil)

	require.Eventually(t, func() bool {
		records := fixture.registry.TaskOutputs([]string{snapshot.ID})

		return len(records) == 1 && strings.Contains(records[0].Output, "first-line")
	}, 30*time.Second, 10*time.Millisecond)

	record := fixture.registry.TaskOutputs([]string{snapshot.ID})[0]
	require.True(t, record.Found)
	assert.Equal(t, background.StatusRunning, record.Status)
	assert.True(t, record.Ended.IsZero())
	assert.Nil(t, record.ExitCode)
	assert.Equal(t, filepath.Join(fixture.dir, snapshot.ID+".log"), record.OutputFile)
	assert.False(t, record.Truncated)

	// The plain poll carries the reference's wait words, so the model knows the
	// task is still working.
	hinted := background.ApplyWaitHint(record, background.WaitHintPoll, 0, 0)
	assert.Contains(t, hinted.Output, "Use timeout_ms to wait for completion.")
	assert.Contains(t, hinted.Output, "It is still working.")

	// The body renders the record and never the raw command.
	body := background.RenderTaskOutputBody(hinted)
	assert.Contains(t, body, "=== Task "+snapshot.ID+" ===")
	assert.Contains(t, body, "Status: running")
	assert.Contains(t, body, "Output File: "+record.OutputFile)
	assert.Contains(t, body, "=== Output ===")
}

// TestTaskOutputReadsAFinishedTask pins the terminal record: the exit code, both
// times, the duration, and the whole log while it fits the bound.
func TestTaskOutputReadsAFinishedTask(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, "line-1", "line-2"), nil)
	waitForCompletion(t, observer, 1)

	record := fixture.registry.TaskOutputs([]string{snapshot.ID})[0]
	require.True(t, record.Found)
	assert.Equal(t, background.StatusCompleted, record.Status)
	require.NotNil(t, record.ExitCode)
	assert.Equal(t, 0, *record.ExitCode)
	assert.False(t, record.Started.IsZero())
	assert.False(t, record.Ended.IsZero())
	assert.GreaterOrEqual(t, record.DurationSecs, 0.0)
	assert.Equal(t, "line-1\nline-2\n", record.Output)
	assert.False(t, record.Truncated)
	assert.Empty(t, record.TruncationHint)
	assert.Equal(t, int64(len("line-1\nline-2\n")), record.RawOutputBytes)
}

// TestTaskOutputTruncatesPastTheBound pins the bounded dump: the first 2 000
// bytes at a rune boundary, the real total, and both hint lines naming the read
// Tool.
func TestTaskOutputTruncatesPastTheBound(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 100_000, 1024)

	snapshot := launchMonitor(t, fixture.registry, writeStart(t, strings.Repeat("x", 60_000)), nil)
	waitForCompletion(t, observer, 1)

	record := fixture.registry.TaskOutputs([]string{snapshot.ID})[0]
	require.True(t, record.Found)
	assert.True(t, record.Truncated)
	assert.Equal(t, int64(60_001), record.RawOutputBytes)
	assert.True(t, strings.HasPrefix(record.Output, strings.Repeat("x", 2_000)+"\n\n"))
	assert.Contains(t, record.Output,
		"[Output truncated - 60001 bytes total. Use read on "+record.OutputFile+" for full content]")
	assert.Equal(t, "[truncated - use read on output_file for full content]", record.TruncationHint)

	// The preview is cut at a rune boundary, so a multi-byte rune straddling the
	// bound is dropped rather than split.
	second := &recorder{}
	fixture = newRegistryFixture(t, second, 100_000, 1024)
	snapshot = launchMonitor(
		t,
		fixture.registry,
		writeStart(t, strings.Repeat("a", 1_999)+"é"+strings.Repeat("b", 50_000)),
		nil,
	)
	waitForCompletion(t, second, 1)

	require.Eventually(t, func() bool {
		stored, ok := fixture.registry.Snapshot(snapshot.ID)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)

	record = fixture.registry.TaskOutputs([]string{snapshot.ID})[0]
	require.True(t, record.Truncated)
	assert.True(t, strings.HasPrefix(record.Output, strings.Repeat("a", 1_999)+"\n\n"))
	assert.NotContains(t, record.Output[:1_999], "é")
}

// TestTaskOutputStatusWords pins the order: a kill wins over the deadline, the
// deadline beats a clean exit, and a clean exit is completed.
func TestTaskOutputStatusWords(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	killed := launchMonitor(t, fixture.registry, blockStart(), nil)
	require.Equal(t, background.OutcomeKilled, fixture.registry.Kill(killed.ID, background.KillUser))
	waitForCompletion(t, observer, 1)
	assert.Equal(t, background.StatusCancelled, fixture.registry.TaskOutputs([]string{killed.ID})[0].Status)

	timedOut := launchMonitor(t, fixture.registry, blockStart(), func(request *background.Request) {
		request.Timeout = 50 * time.Millisecond
	})
	waitForCompletion(t, observer, 2)
	assert.Equal(t, background.StatusTimedOut, fixture.registry.TaskOutputs([]string{timedOut.ID})[0].Status)

	completed := launchMonitor(t, fixture.registry, writeStart(t, "line"), nil)
	waitForCompletion(t, observer, 3)
	assert.Equal(t, background.StatusCompleted, fixture.registry.TaskOutputs([]string{completed.ID})[0].Status)
}

// TestTaskOutputReportsAnUnknownID keeps an unknown id out of the error channel:
// a multi read renders it as its own not_found record instead.
func TestTaskOutputReportsAnUnknownID(t *testing.T) {
	t.Parallel()

	fixture := newRegistryFixture(t, &recorder{}, 1<<20, 1024)

	records := fixture.registry.TaskOutputs([]string{"monitor-missing"})
	require.Len(t, records, 1)
	assert.False(t, records[0].Found)
	assert.Equal(t, "monitor-missing", records[0].TaskID)
	assert.Equal(t, background.StatusNotFound, records[0].Status)
	assert.Equal(t, "Task monitor-missing not found.", records[0].Output)
}

// TestTaskOutputBodyRendersTheNoOutputStates pins the two empty-output lines the
// reference's body uses.
func TestTaskOutputBodyRendersTheNoOutputStates(t *testing.T) {
	t.Parallel()

	running := background.TaskOutput{Found: true, TaskID: "monitor-1", Status: background.StatusRunning}
	assert.Contains(t, background.RenderTaskOutputBody(running), "(no output yet)")

	ended := background.TaskOutput{Found: true, TaskID: "monitor-1", Status: background.StatusCompleted}
	assert.Contains(t, background.RenderTaskOutputBody(ended), "(no output)")
}

// TestRegistryWaitReturnsWhenTheTaskEnds proves the wait is event-driven: it
// returns as soon as the registered task's own ended channel closes.
func TestRegistryWaitReturnsWhenTheTaskEnds(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	release := make(chan struct{})

	start := func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		select {
		case <-release:
			return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
		case <-ctx.Done():
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		}
	}

	snapshot := launchMonitor(t, fixture.registry, start, nil)

	type result struct {
		records []background.TaskOutput
		outcome background.WaitOutcome
	}

	waited := make(chan result, 1)

	go func() {
		records, outcome := fixture.registry.Wait(t.Context(), []string{snapshot.ID}, false, time.Minute)
		waited <- result{records: records, outcome: outcome}
	}()

	close(release)

	started := time.Now()

	select {
	case value := <-waited:
		assert.Equal(t, background.WaitOutcomeCompleted, value.outcome)
		require.Len(t, value.records, 1)
		assert.Equal(t, background.StatusCompleted, value.records[0].Status)
		// The wait wakes on the task's own ended channel, so it returns long
		// before its minute-long budget and not on a poll interval.
		assert.Less(t, time.Since(started), 5*time.Second,
			"the wait must wake on the task's own end rather than on a poll")
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not return when the task ended")
	}
}

// TestRegistryWaitReturnsAtTheBudget keeps a still-running task's record when
// the budget elapses, and a wait-any returns at the first completion.
func TestRegistryWaitReturnsAtTheBudgetAndAtTheFirstCompletion(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	running := launchMonitor(t, fixture.registry, blockStart(), nil)

	records, outcome := fixture.registry.Wait(t.Context(), []string{running.ID}, false, 20*time.Millisecond)
	assert.Equal(t, background.WaitOutcomeDeadline, outcome)
	require.Len(t, records, 1)
	assert.Equal(t, background.StatusRunning, records[0].Status)

	finished := launchMonitor(t, fixture.registry, writeStart(t, "line"), nil)

	records, outcome = fixture.registry.Wait(
		t.Context(), []string{running.ID, finished.ID}, true, time.Minute,
	)
	assert.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 2)
	assert.Equal(t, background.StatusCompleted, records[1].Status)

	require.Equal(t, background.OutcomeKilled, fixture.registry.Kill(running.ID, background.KillUser))
	waitForCompletion(t, observer, 2)
}

// TestRegistryWaitOrdersRecordsAsAsked keeps a multi read stable: the records
// come back in the order the ids were asked, whatever the registry's own order.
func TestRegistryWaitOrdersRecordsAsAsked(t *testing.T) {
	t.Parallel()

	observer := &recorder{}
	fixture := newRegistryFixture(t, observer, 1<<20, 1024)

	first := launchMonitor(t, fixture.registry, writeStart(t, "one"), nil)
	second := launchMonitor(t, fixture.registry, writeStart(t, "two"), nil)
	waitForCompletion(t, observer, 2)

	records := fixture.registry.TaskOutputs([]string{second.ID, "monitor-missing", first.ID})
	require.Len(t, records, 3)
	assert.Equal(t, second.ID, records[0].TaskID)
	assert.Equal(t, "monitor-missing", records[1].TaskID)
	assert.False(t, records[1].Found)
	assert.Equal(t, first.ID, records[2].TaskID)
}

// TestBoundTaskOutputTextKeepsABodyUnderTheBound pins the un-truncated case: the
// whole body, the body's real length, and no footer.
func TestBoundTaskOutputTextKeepsABodyUnderTheBound(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("a", 100)

	output, truncated, raw := background.BoundTaskOutputText(body)
	assert.Equal(t, body, output)
	assert.False(t, truncated)
	assert.Equal(t, int64(len(body)), raw)
}

// TestBoundTaskOutputTextTruncatesWithThePathFreeFooter pins the bound a
// delegated child's result takes: the same 40 000 bytes a task log is bounded
// at, the body's real length, and a footer that names no file, because a child
// has none.
func TestBoundTaskOutputTextTruncatesWithThePathFreeFooter(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("b", 40_001)

	output, truncated, raw := background.BoundTaskOutputText(body)
	require.True(t, truncated)
	assert.Equal(t, int64(len(body)), raw)
	assert.Contains(t, output, "[Output truncated - 40001 bytes total]")
	assert.NotContains(t, output, "Use read on",
		"a body with no file behind it must not point at one")
	assert.Less(t, len(output), len(body))
}

// TestWaitHintAddressesASubagentInItsOwnWords pins the subject split: the same
// renderer serves a task and a child, and only a child is told not to be
// stopped, because a background task has no follow-up channel.
func TestWaitHintAddressesASubagentInItsOwnWords(t *testing.T) {
	t.Parallel()

	child := background.TaskOutput{
		Found: true, Subject: background.SubjectSubagent,
		TaskID: "s-child", Status: background.StatusRunning, Output: "Subagent is still running.",
	}
	hinted := background.ApplyWaitHint(child, background.WaitHintPoll, 0, 0)
	assert.Contains(t, hinted.Output, "do not kill this subagent and do not tell it to stop")
	assert.NotContains(t, hinted.Output, "do not kill this task")

	task := child
	task.Subject = background.SubjectTask
	hinted = background.ApplyWaitHint(task, background.WaitHintPoll, 0, 0)
	assert.Contains(t, hinted.Output, "do not kill this task")
	assert.NotContains(t, hinted.Output, "do not tell it to stop")

	// A terminal record is the whole answer, whatever its subject.
	terminal := child
	terminal.Status = background.StatusCompleted
	assert.NotContains(t, background.ApplyWaitHint(terminal, background.WaitHintPoll, 0, 0).Output,
		"timeout_ms")
}

// TestTaskOutputLiveReportsAWorkingSubject pins which records a wait hint and a
// "(no output yet)" body belong to.
func TestTaskOutputLiveReportsAWorkingSubject(t *testing.T) {
	t.Parallel()

	live := map[string]bool{
		background.StatusInitializing: true,
		background.StatusRunning:      true,
		background.StatusCompleted:    false,
		background.StatusFailed:       false,
		background.StatusCancelled:    false,
		background.StatusTimedOut:     false,
		background.StatusNotFound:     false,
	}
	for status, want := range live {
		assert.Equal(t, want, background.TaskOutput{Status: status}.Live(), status)
	}

	// An initializing child is still working, so its body says so rather than
	// reporting a finished subject with no output.
	body := background.RenderTaskOutputBody(background.TaskOutput{
		Status: background.StatusInitializing, Subject: background.SubjectSubagent,
	})
	assert.Contains(t, body, "(no output yet)")
}
