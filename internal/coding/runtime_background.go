package coding

import (
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

const (
	// maxPendingBackgroundEvents bounds the background events that have not
	// reached a delivery boundary yet. The oldest are dropped, matching the
	// bound the reference puts on its own mid-turn buffer.
	maxPendingBackgroundEvents = 50
	// backgroundTailBytes bounds the output tail inlined in one background
	// completion. It mirrors the shell's capture bound: the tail is a bounded
	// projection, not the whole log.
	backgroundTailBytes = 24 << 10
	// backgroundOutputBytes bounds one background task's raw output file. It
	// matches the shell's output ceiling rather than widening it.
	backgroundOutputBytes = 16 << 20
)

// backgroundOutputDir is where background tasks write their raw output. It is
// the session's own directory, so a monitor's log never lands in the workspace.
func backgroundOutputDir(sessionDir string) string {
	return filepath.Join(sessionDir, "terminal")
}

// backgroundObserver is the Runtime's view of its registered background tasks.
//
// It is the mid-turn buffer, not the queue: an event waits here until the
// notification coordinator reaches a delivery boundary, and a running task's
// buffered events are process-local state that a restart drops, exactly as the
// reference drops its own monitor buffer. What is durable is the rendered
// record the buffer produces — one message per delivery unit — which rides the
// shared parent pending queue until its conversation acknowledges it
// (design decision 6).
type backgroundObserver struct {
	mu          sync.Mutex
	runtime     *Runtime
	events      []background.Event
	completions []background.Completion
	// observed counts everything this Runtime has produced, so a test can
	// observe the seam without consuming it.
	observed int64
	sequence uint64
}

func newBackgroundObserver(runtime *Runtime) *backgroundObserver {
	return &backgroundObserver{runtime: runtime}
}

// BackgroundEvent buffers one streamed event. The buffer keeps the most recent
// maxPendingBackgroundEvents events.
func (o *backgroundObserver) BackgroundEvent(event background.Event) {
	if o == nil {
		return
	}

	o.mu.Lock()
	o.observed++
	o.events = append(o.events, event)

	if excess := len(o.events) - maxPendingBackgroundEvents; excess > 0 {
		o.events = o.events[excess:]
	}
	o.mu.Unlock()

	o.signal()
}

// BackgroundCompleted buffers one task's terminal report. Exactly one is
// produced per task, whatever stopped it. It also settles the tasks pane's row.
func (o *backgroundObserver) BackgroundCompleted(completion background.Completion) {
	if o == nil {
		return
	}

	o.mu.Lock()
	o.observed++
	o.completions = append(o.completions, completion)
	o.mu.Unlock()

	o.signal()
}

// observedBackground reports how many events and completions this Runtime has
// produced. It never consumes the buffer.
func (o *backgroundObserver) observedBackground() int64 {
	if o == nil {
		return 0
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	return o.observed
}

func (o *backgroundObserver) signal() {
	if o != nil && o.runtime != nil {
		o.runtime.signalNotifications()
	}
}

// takeBackground removes and returns everything buffered.
func (o *backgroundObserver) takeBackground() ([]background.Event, []background.Completion) {
	if o == nil {
		return nil, nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	events := o.events
	completions := o.completions
	o.events = nil
	o.completions = nil

	return events, completions
}

// deliverBackground turns everything buffered into the shared durable queue: one
// rendered record for the event batch and one for each completion. A completion
// supersedes the buffered events of its own task, because the terminal report
// already carries the outcome and the reference never delivers both.
func (o *backgroundObserver) deliverBackground() error {
	if o == nil || o.runtime == nil {
		return nil
	}

	events, completions := o.takeBackground()
	if len(events) == 0 && len(completions) == 0 {
		return nil
	}

	inbox := o.runtime.notifications
	if inbox == nil {
		return nil
	}

	for _, message := range o.backgroundMessages(events, completions) {
		if err := inbox.EnqueueMessage(message); err != nil {
			return err
		}
	}

	return nil
}

// backgroundMessages renders one queue record per delivery unit: the surviving
// events become one batch, and each completion becomes its own record. A
// completion whose kill outcome a Tool result already delivered produces no
// record, so the model is never woken twice for one kill.
func (o *backgroundObserver) backgroundMessages(
	events []background.Event,
	completions []background.Completion,
) []subagent.ParentMessage {
	completed := make(map[string]struct{}, len(completions))
	for _, completion := range completions {
		completed[completion.Snapshot.ID] = struct{}{}
	}

	surviving := make([]background.Event, 0, len(events))

	for _, event := range events {
		if _, superseded := completed[event.TaskID]; superseded {
			continue
		}

		surviving = append(surviving, event)
	}

	messages := make([]subagent.ParentMessage, 0, len(completions)+1)
	if len(surviving) > 0 {
		messages = append(messages, o.message(
			subagent.MessageKindBackgroundEvents,
			"",
			background.MonitorBatchText(surviving, background.KillTaskToolName),
		))
	}

	for _, completion := range completions {
		if completion.Killed && completion.KillResultDelivered {
			continue
		}

		messages = append(messages, o.message(
			subagent.MessageKindBackgroundCompletion,
			completion.Snapshot.ID,
			background.MonitorCompletionText(completion),
		))
	}

	return messages
}

// message stamps one rendered body with the queue identity its conversation
// acknowledges it by.
func (o *backgroundObserver) message(
	kind subagent.ParentMessageKind,
	taskID string,
	body string,
) subagent.ParentMessage {
	o.mu.Lock()
	o.sequence++
	sequence := o.sequence
	sessionID := ""

	if o.runtime.handle != nil {
		sessionID = o.runtime.handle.Metadata().ID
	}

	o.mu.Unlock()

	at := time.Now().UTC()

	return subagent.ParentMessage{
		ID:        subagent.ParentMessagePrefix + strconv.FormatInt(at.UnixNano(), 10) + "-" + strconv.FormatUint(sequence, 10),
		Ownership: subagent.Ownership{ParentSessionID: sessionID},
		Kind:      kind,
		TaskID:    taskID,
		Body:      body,
		At:        at,
	}
}
