package coding

import (
	"path/filepath"
	"sync"

	"github.com/rsbin1178/pips/internal/coding/background"
)

const (
	// maxPendingBackgroundEvents bounds the background events that have not
	// reached the model yet. The oldest are dropped, matching the bound the
	// reference puts on its own pending queue.
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
// It keeps model-visible output that has not been delivered yet: the events,
// bounded with oldest-dropped, and one completion per task.
type backgroundObserver struct {
	mu          sync.Mutex
	runtime     *Runtime
	events      []background.Event
	completions []background.Completion
}

func newBackgroundObserver(runtime *Runtime) *backgroundObserver {
	return &backgroundObserver{runtime: runtime}
}

// BackgroundEvent records one streamed event. The event is the delivery, so
// nothing here is persisted; a restart drops what was not delivered yet.
func (o *backgroundObserver) BackgroundEvent(event background.Event) {
	if o == nil {
		return
	}

	o.mu.Lock()
	o.events = append(o.events, event)

	if excess := len(o.events) - maxPendingBackgroundEvents; excess > 0 {
		o.events = o.events[excess:]
	}
	o.mu.Unlock()

	if o.runtime != nil {
		o.runtime.signalNotifications()
	}
}

// BackgroundCompleted records one task's terminal report. Exactly one is
// produced per task, whatever stopped it.
func (o *backgroundObserver) BackgroundCompleted(completion background.Completion) {
	if o == nil {
		return
	}

	o.mu.Lock()
	o.completions = append(o.completions, completion)
	o.mu.Unlock()

	if o.runtime != nil {
		o.runtime.signalNotifications()
	}
}

// pendingBackground returns the undelivered background output and clears it.
// The Runtime's notification coordinator is the only caller.
func (o *backgroundObserver) pendingBackground() ([]background.Event, []background.Completion) {
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
