package coding

import (
	"strconv"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrappedMonitorEvent is the wire text one monitor event carries. The
// background package writes it; a test that injects an event directly has to
// spell it the same way.
func wrappedMonitorEvent(description, text, taskID string) string {
	return "<monitor-event description=\"" + description + "\" task_id=\"" + taskID + "\">\n" +
		text + "\n</monitor-event>"
}

func testParentMessage(runtime *Runtime, id, body string) subagent.ParentMessage {
	return subagent.ParentMessage{
		ID:        id,
		Ownership: subagent.Ownership{ParentSessionID: runtime.handle.Metadata().ID},
		Kind:      subagent.MessageKindBackgroundEvents,
		TaskID:    "monitor-1",
		Body:      body,
		At:        time.Now().UTC(),
	}
}

// TestRuntimeDeliversARealMonitorWatchToAnIdleSession drives the whole path with
// a real process: a real /bin/sh watch writes a line, the pipeline turns it into
// an event, the coordinator renders it onto the shared queue, and an idle
// Runtime runs one batched turn that carries it. The record is acknowledged when
// the transcript commits the message.
func TestRuntimeDeliversARealMonitorWatchToAnIdleSession(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse("noted"),
		runtimeTextResponse("noted"),
		runtimeTextResponse("noted"),
	)
	runtime := openTestRuntime(t, model)

	monitor, ok := runtime.controller.Tool(tools.MonitorName)
	require.True(t, ok, "the runtime must register the monitor wrapper")

	call := agent.ToolCall{
		ID:   "call-monitor",
		Name: tools.MonitorName,
		Args: ai.JSON(`{"command":"printf 'watched-line\n'","description":"watch once","persistent":true}`),
	}

	decision := runtime.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := monitor.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	require.Eventually(t, func() bool {
		for _, request := range model.Requests() {
			if requestContainsText(request, "watched-line") {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.PendingMessages()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)

	// The batch reaches the conversation as runtime-generated input, so a
	// frontend renders it as activity rather than as the user's words.
	state := runtime.Snapshot()
	require.NotEmpty(t, state.Transcript)
	assert.Contains(t, state.SyntheticMessages, 0)
}

// TestRuntimeSteersARealMonitorEventIntoAnActiveTurn starts a real watch in one
// turn and keeps the run busy with a real sleep, so the event is produced while
// the turn is still running. The batch is steered as a <system-reminder> user
// message and lands at the next turn boundary.
func TestRuntimeSteersARealMonitorEventIntoAnActiveTurn(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"call-monitor",
			tools.MonitorName,
			`{"command":"printf 'mid-line\n'; sleep 30","description":"mid watch","persistent":true}`,
		),
		runtimeToolResponse("call-wait", "shell", `{"command":"sleep 4"}`),
		runtimeTextResponse("saw the line"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("start the watch")))
	require.Contains(t, eventTypes(events), EventInteractionCompleted)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.True(t,
		requestContainsText(requests[2], "mid-line"),
		"the streamed event must reach the next turn of the same run",
	)
	assert.True(t, requestContainsText(requests[2], systemReminderOpen))

	// The steered message was committed, so its queue record is delivered and
	// cannot be handed to a second turn.
	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.PendingMessages()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)
}

func TestRuntimeAcknowledgesADeliveredBackgroundMessageOnce(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("noted"))
	runtime := openTestRuntime(t, model)
	message := testParentMessage(
		runtime,
		subagent.ParentMessagePrefix+"ack-1",
		"1 monitor events from 1 monitor (use kill_background_task to identify each monitor):\n\n"+
			"<monitor description=\"watch\" task_id=\"monitor-1\">\n[1] only\n</monitor>",
	)
	require.NoError(t, runtime.notifications.EnqueueMessage(message))
	runtime.signalNotifications()

	require.Eventually(t, func() bool {
		pending, err := runtime.notifications.PendingMessages()

		return err == nil && len(pending) == 0 && len(model.Requests()) == 1 &&
			len(runtime.Snapshot().Transcript) == 2
	}, 30*time.Second, 10*time.Millisecond)

	require.Len(t, model.Requests(), 1)

	transcript := runtime.Snapshot().Transcript
	require.Len(t, transcript, 2)

	text, ok := singleUserText(transcript[0])
	require.True(t, ok)
	assert.Equal(t, reminderMessageText(message.Body), text)

	// The record is delivered, so the pending set an acknowledgement leaves
	// behind is empty: a later signal can never hand the same batch to a second
	// turn.
	known, err := runtime.notifications.Contains(message.ID)
	require.NoError(t, err)
	assert.True(t, known)

	pending, err := runtime.notifications.PendingMessages()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestBackgroundObserverRendersQueueRecords pins the registration step itself:
// a completion supersedes the buffered events of its own task, a model kill that
// already returned a Tool result produces no wake, and every record carries the
// conversation it belongs to.
func TestBackgroundObserverRendersQueueRecords(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	observer := runtime.backgroundOutput

	// Park the coordinator so the buffer this test reads is the one it wrote.
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	observer.BackgroundEvent(background.Event{
		TaskID: "monitor-1", Description: "deploy log", Text: "FAILED",
		Wrapped: wrappedMonitorEvent("deploy log", "FAILED", "monitor-1"),
	})
	observer.BackgroundEvent(background.Event{
		TaskID: "monitor-2", Description: "other watch", Text: "up",
		Wrapped: wrappedMonitorEvent("other watch", "up", "monitor-2"),
	})
	observer.BackgroundCompleted(background.Completion{
		Snapshot: background.Snapshot{
			ID: "monitor-1", Description: "deploy log", ExitCode: 0,
			Completed: true, Kind: background.KindMonitor,
			Start: time.Now().Add(-2 * time.Second), End: time.Now(),
		},
		Tail: "final line",
	})

	events, completions := observer.takeBackground()
	messages := observer.backgroundMessages(events, completions)
	require.Len(t, messages, 2)

	// The events of a task that completed are dropped: the completion is that
	// task's single wake. A batch can span monitors, so it carries no single
	// task id; the rendered body names each one.
	assert.Equal(t, subagent.MessageKindBackgroundEvents, messages[0].Kind)
	assert.Empty(t, messages[0].TaskID)
	assert.Equal(t, "<monitor-event task_id=\"monitor-2\">\n[other watch] up\n</monitor-event>", messages[0].Body)

	assert.Equal(t, subagent.MessageKindBackgroundCompletion, messages[1].Kind)
	assert.Equal(t, "monitor-1", messages[1].TaskID)
	assert.Contains(t, messages[1].Body, `Monitor "monitor-1" ended: [monitor ended: exited (code 0)].`)
	assert.Contains(t, messages[1].Body, "final line")

	for _, message := range messages {
		assert.Equal(t, runtime.handle.Metadata().ID, message.Ownership.ParentSessionID)
		assert.True(t, subagent.IsParentMessageID(message.ID))
		assert.False(t, message.At.IsZero())
	}

	// A kill whose outcome already reached the model as a Tool result produces
	// no second waking message.
	suppressed := background.Completion{
		Snapshot: background.Snapshot{ID: "monitor-3", Completed: true, Kind: background.KindMonitor},
		Killed:   true, KillResultDelivered: true,
	}
	assert.Empty(t, observer.backgroundMessages(nil, []background.Completion{suppressed}))

	// The same kill from the tasks pane has no Tool result behind it, so the
	// notice still reaches the model.
	paneKill := suppressed
	paneKill.KillResultDelivered = false
	delivered := observer.backgroundMessages(nil, []background.Completion{paneKill})
	require.Len(t, delivered, 1)
	assert.Contains(t, delivered[0].Body, "killed by the user — do not restart it.")
}

// TestBackgroundSelectionConsumesNoAttempt pins the retry budget's owner: a wake
// that lands while the conversation is busy selects the record but spends
// nothing, so a run of busy wakes can never retire a batch that was never
// handed to a turn. Only a claim spends an attempt.
func TestBackgroundSelectionConsumesNoAttempt(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	message := testParentMessage(runtime, subagent.ParentMessagePrefix+"attempt-1", "batch")
	require.NoError(t, runtime.notifications.EnqueueMessage(message))

	for range maxBackgroundDeliveryAttempts + 1 {
		require.Len(t, runtime.nextBackgroundBatch([]subagent.ParentMessage{message}), 1)
	}

	runtime.notificationMu.Lock()
	assert.Empty(t, runtime.backgroundAttempts)
	runtime.notificationMu.Unlock()

	for attempt := range maxBackgroundDeliveryAttempts {
		text := "text-" + strconv.Itoa(attempt)
		require.True(t, runtime.claimBackground(text, []subagent.ParentMessage{message}))
		runtime.releaseBackground(text)
	}

	assert.Empty(t, runtime.nextBackgroundBatch([]subagent.ParentMessage{message}),
		"a record whose budget is spent stops being retried")
}

func TestRuntimeDrainsBufferedEventsIntoTheSharedQueue(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	runtime.backgroundOutput.BackgroundEvent(background.Event{
		TaskID: "monitor-1", Description: "deploy log", Text: "one",
		Wrapped: wrappedMonitorEvent("deploy log", "one", "monitor-1"),
	})
	runtime.backgroundOutput.BackgroundEvent(background.Event{
		TaskID: "monitor-1", Description: "deploy log", Text: "two",
		Wrapped: wrappedMonitorEvent("deploy log", "two", "monitor-1"),
	})
	require.NoError(t, runtime.backgroundOutput.deliverBackground())

	pending, err := runtime.notifications.PendingMessages()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, subagent.MessageKindBackgroundEvents, pending[0].Kind)
	assert.Equal(t, `2 monitor events from 1 monitor (use kill_background_task to identify each monitor):

<monitor description="deploy log" task_id="monitor-1">
[1] one
[2] two
</monitor>`, pending[0].Body)

	// Rendering twice is idempotent: the buffer is already empty and the record
	// keeps its identity.
	require.NoError(t, runtime.backgroundOutput.deliverBackground())
	pending, err = runtime.notifications.PendingMessages()
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// Reopening the inbox over the same session finds the record, so a restart
	// between the render and the commit still delivers it.
	reopened, err := subagent.NewNotificationInbox(
		runtime.session,
		runtime.handle.Metadata().ID,
		subagent.DefaultLimits().MaxResultBytes,
	)
	require.NoError(t, err)
	messages, err := reopened.PendingMessages()
	require.NoError(t, err)
	assert.Equal(t, pending, messages)
}
