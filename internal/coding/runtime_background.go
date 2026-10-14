package coding

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
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
	// backgroundOutputBytes bounds one background task's raw output file. It is
	// the default for every kind, and it is the reference's monitor cap of
	// 10 MiB rather than the shell's wider output ceiling.
	backgroundOutputBytes = 10 << 20
	// shellBackgroundOutputBytes bounds a shell task's raw output file. A
	// backgrounded command's log is the command's whole output, so it keeps the
	// shell operation's own ceiling rather than the narrower monitor cap.
	shellBackgroundOutputBytes = tools.ShellMaximumOutput
	// backgroundLogReadBytes bounds the admitted read of a background task's
	// log. It is the widest bound any kind writes, so a shell task's larger log
	// is readable while a monitor's own file still never exceeds its cap.
	backgroundLogReadBytes = shellBackgroundOutputBytes
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

// BackgroundStarted publishes one registration, so the tasks pane lists a
// running watch before it has produced anything.
func (o *backgroundObserver) BackgroundStarted(snapshot background.Snapshot) {
	if o == nil || o.runtime == nil {
		return
	}

	o.runtime.publishBackgroundTaskLifecycle(backgroundTaskLifecycle(snapshot))
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

	if o.runtime != nil {
		o.runtime.publishBackgroundTaskLifecycle(backgroundTaskLifecycle(completion.Snapshot))
	}

	o.signal()
}

// publishBackgroundTaskLifecycle commits the tasks pane's row for one
// background task. The pane is a projection, so a missing frontend or telemetry
// observer never holds up the registry.
func (r *Runtime) publishBackgroundTaskLifecycle(value BackgroundTaskLifecycle) {
	if r == nil || r.publisher == nil {
		return
	}

	eventType := EventBackgroundStarted
	if value.Completed {
		eventType = EventBackgroundCompleted
	}

	emitter := newEventEmitter(context.WithoutCancel(context.Background()), r, nil, false)
	_ = emitter.emit("", "", eventType, value)
}

// KillBackgroundTask stops one registered background task. It is the one
// control method the model's stop Tool, the tasks pane, and the runtime control
// plane share, so a watch is stopped through exactly one path. The source
// decides whether the terminal completion still wakes the model: a model kill
// already returned its own Tool result and must not wake twice.
func (r *Runtime) KillBackgroundTask(
	ctx context.Context,
	taskID string,
	source background.KillSource,
) (background.Outcome, error) {
	if r == nil || r.backgroundTasks == nil {
		return background.OutcomeNotFound, ErrRuntimeClosed
	}

	if taskID == "" {
		return background.OutcomeNotFound, ErrRuntimeInvalid
	}

	if err := ctx.Err(); err != nil {
		return background.OutcomeNotFound, err
	}

	return r.backgroundTasks.Kill(taskID, source), nil
}

// BackgroundTaskInspection is one bounded view of a registered background task:
// its snapshot and a bounded, sanitized tail of the task's own output. No
// rendering surface holds more than this, so an inspect can never grow with a
// watch's log.
type BackgroundTaskInspection struct {
	Task   background.Snapshot
	Output string
}

// Lifecycle projects the inspection's snapshot into the record the tasks pane
// and the event stream read, so every surface describes one task the same way.
func (i BackgroundTaskInspection) Lifecycle() BackgroundTaskLifecycle {
	return backgroundTaskLifecycle(i.Task)
}

// InspectBackgroundTask returns one task's bounded inspection view. It is the
// read half of the same control surface the stop Tool uses.
func (r *Runtime) InspectBackgroundTask(
	ctx context.Context,
	taskID string,
) (BackgroundTaskInspection, error) {
	if r == nil || r.backgroundTasks == nil {
		return BackgroundTaskInspection{}, ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return BackgroundTaskInspection{}, err
	}

	snapshot, found := r.backgroundTasks.Snapshot(taskID)
	if !found {
		return BackgroundTaskInspection{}, fmt.Errorf(
			"%w: background task %q is unknown", ErrRuntimeInvalid, taskID,
		)
	}

	output, _ := r.backgroundTasks.Tail(taskID, backgroundTailBytes)

	return BackgroundTaskInspection{Task: snapshot, Output: output}, nil
}

// KnownBackgroundTaskIDs lists the ids the Runtime's background registry still
// knows, oldest first. It is what a not-found stop names so the model can
// recover an id it lost. It reads the same registry the kill itself addresses.
//
// A delegated child's session id is deliberately not listed even though the
// read Tools resolve it: the sentence is shared with the stop Tool, which
// cannot stop a child, so naming one would offer an id the caller cannot use.
func (r *Runtime) KnownBackgroundTaskIDs(ctx context.Context) []string {
	if r == nil || r.backgroundTasks == nil || ctx.Err() != nil {
		return nil
	}

	snapshots := r.backgroundTasks.List()
	ids := make([]string, 0, len(snapshots))

	for _, snapshot := range snapshots {
		if snapshot.ID != "" {
			ids = append(ids, snapshot.ID)
		}
	}

	return ids
}

// readBackgroundTasks is the one read operation the task-output and
// blocking-wait Tools route through, so the runtime owns id resolution. Every
// id resolves against the background registry first and then against this
// session's own children, so a watch and a delegated child are readable through
// one surface. A poll takes a snapshot; any other mode waits inside the
// producers, which are event-driven on their own terminal signals.
func (r *Runtime) readBackgroundTasks(
	ctx context.Context,
	ids []string,
	mode string,
	budget time.Duration,
) ([]background.TaskOutput, background.WaitOutcome) {
	if r == nil {
		return nil, background.WaitOutcomeCompleted
	}

	if mode == tools.TaskOutputModePoll || budget <= 0 {
		return r.pollTaskOutputs(ctx, ids), background.WaitOutcomeCompleted
	}

	return r.waitTaskOutputs(ctx, ids, mode, budget)
}

// pollTaskOutputs resolves one record per asked id, in the asked order.
func (r *Runtime) pollTaskOutputs(ctx context.Context, ids []string) []background.TaskOutput {
	records := make([]background.TaskOutput, 0, len(ids))

	for _, id := range ids {
		records = append(records, r.resolveTaskOutput(ctx, id))
	}

	return records
}

// resolveTaskOutput resolves one id against the registry first and then against
// this session's own children. An id of neither shape yields the same not-found
// record, so a caller that guessed the id's shape wrong still reads one answer.
func (r *Runtime) resolveTaskOutput(ctx context.Context, id string) background.TaskOutput {
	if record := r.registryTaskOutput(id); record.Found {
		return record
	}

	if record, ok := r.childTaskOutput(ctx, id); ok {
		return record
	}

	return background.NotFoundTaskOutput(id)
}

// registryTaskOutput reads one id from the background registry. It is the watch
// half of the resolver, so it stays beside the registry's other read surfaces.
func (r *Runtime) registryTaskOutput(taskID string) background.TaskOutput {
	if r == nil || r.backgroundTasks == nil {
		return background.NotFoundTaskOutput(taskID)
	}

	return r.backgroundTasks.TaskOutput(taskID)
}

// registryKnows reports whether the background registry owns an id. It is the
// first half of the resolution order, and it is how a wait decides which
// producer an id belongs to. A registry id carries a kind prefix and a child id
// is a session id, so the two id spaces cannot collide.
func (r *Runtime) registryKnows(taskID string) bool {
	if r == nil || r.backgroundTasks == nil {
		return false
	}

	_, ok := r.backgroundTasks.Snapshot(taskID)

	return ok
}

// admitBackgroundLogPath reports whether the requested value is exactly the log
// path of a task the registry still knows, and returns that path. The match is
// exact, so no other path, another directory, a traversed spelling, or an
// unknown id's log is admitted, and the read Tool learns nothing about the
// session directory.
func (r *Runtime) admitBackgroundLogPath(requested string) (string, bool) {
	if r == nil || r.backgroundTasks == nil || requested == "" {
		return "", false
	}

	for _, id := range r.KnownBackgroundTaskIDs(context.Background()) {
		path, ok := r.backgroundTasks.OutputPath(id)
		if ok && path == requested {
			return path, true
		}
	}

	return "", false
}

// backgroundTaskOutcome classifies one task's projection into the closed,
// low-cardinality vocabulary the pane and telemetry share. It is content-free
// by construction: no command, description, path, output, or id takes part.
const (
	backgroundOutcomeStarted = "started"
	backgroundOutcomeExited  = "exited"
	backgroundOutcomeKilled  = "killed"
	backgroundOutcomeFailed  = "failed"
)

func backgroundTaskOutcome(value BackgroundTaskLifecycle) string {
	switch {
	case !value.Completed:
		return backgroundOutcomeStarted
	case value.Signal != "":
		return backgroundOutcomeKilled
	case value.ExitCode != 0:
		return backgroundOutcomeFailed
	default:
		return backgroundOutcomeExited
	}
}

// backgroundTaskLifecycle projects one snapshot. The description and the
// command are bounded by the monitor Tool's own schema; a task's output is
// never part of it.
func backgroundTaskLifecycle(snapshot background.Snapshot) BackgroundTaskLifecycle {
	return BackgroundTaskLifecycle{
		ID:          snapshot.ID,
		Kind:        string(snapshot.Kind),
		Description: snapshot.Description,
		Command:     snapshot.Command,
		StartedAt:   snapshot.Start,
		EndedAt:     snapshot.End,
		Completed:   snapshot.Completed,
		ExitCode:    snapshot.ExitCode,
		Signal:      snapshot.Signal,
	}
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
// completion produces no record when a Tool result already delivered its kill
// outcome, when the kill came from the cancelled interaction, or when the
// registry reports the result already taken by a read. The taken check is live
// rather than a field captured when the completion was reported, because a
// waiter registers before it blocks and marks the result when it returns, so
// the live state covers both orderings and exactly one surface carries a result.
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
		if completion.KillResultDelivered {
			continue
		}

		if completion.Killed && completion.Source == background.KillPromptEnd {
			continue
		}

		if o.resultTaken(completion.Snapshot.ID) {
			continue
		}

		messages = append(messages, o.message(
			subagent.MessageKindBackgroundCompletion,
			completion.Snapshot.ID,
			backgroundCompletionText(completion),
		))
	}

	return messages
}

// backgroundCompletionText renders one task's terminal report in the words its
// kind uses: a command reports an exit or a signal, a watch reports its
// monitored condition.
func backgroundCompletionText(completion background.Completion) string {
	if completion.Snapshot.Kind == background.KindShell {
		return background.ShellCompletionText(completion)
	}

	return background.MonitorCompletionText(completion)
}

// resultTaken asks the registry whether a task's result is already the model's.
func (o *backgroundObserver) resultTaken(taskID string) bool {
	if o == nil || o.runtime == nil || o.runtime.backgroundTasks == nil {
		return false
	}

	return o.runtime.backgroundTasks.ResultTaken(taskID)
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
