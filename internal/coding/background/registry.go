// Package background owns the process-local registry of background tasks that
// outlive the Tool call that started them. A watch's stdout runs through the
// line pipeline that turns it into bounded, model-visible events; a
// backgrounded command writes both streams to its log and reports once, when it
// completes.
//
// Registry state is runtime state, never persisted: a running task does not
// survive a restart, and nothing about it rides the subagent execution plan.
package background

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
)

// Errors reported by the background registry.
var (
	// ErrInvalid means a registry request cannot be represented safely.
	ErrInvalid = errors.New("coding background: invalid registry request")
	// ErrClosed means the owning Runtime has already been reaped.
	ErrClosed = errors.New("coding background: registry closed")
)

// Kind identifies what one registered background task runs.
type Kind string

// Supported background task kinds.
const (
	// KindMonitor watches a long-running script and reports its stdout lines.
	KindMonitor Kind = "monitor"
	// KindShell runs one command that outlived the Tool call that started it.
	// It writes both streams into its log and emits no events: a command has no
	// event contract, and its completion is the single model-visible report.
	KindShell Kind = "shell"
)

// KillTaskToolName is the model-visible name of the Tool that stops one
// background task. The monitor description, the start receipt, the running-task
// warning, and the catch-up notice all name it, so it has one definition.
const KillTaskToolName = "kill_background_task"

// TaskOutputToolName is the model-visible name of the Tool that reads one
// background task's output by id. The completion text points at it, so it has
// one definition beside the stop Tool's.
const TaskOutputToolName = "get_task_output"

// Snapshot is one immutable view of a registered background task. It is the
// single per-task record every surface reads: the kill Tool, the task-output
// Tool, the tasks pane, the Stop hook, and telemetry.
type Snapshot struct {
	ID          string
	Command     string
	Description string
	CWD         string
	Start       time.Time
	End         time.Time
	ExitCode    int
	Signal      string
	Completed   bool
	// Killed reports that a kill stopped the task, whatever the source.
	Killed bool
	// TimedOut reports that the task's own deadline ended it. A kill wins over
	// the deadline, so a killed task is never reported as timed out.
	TimedOut bool
	Kind     Kind
	Owner    string
}

// Event is one model-visible background event.
type Event struct {
	TaskID      string
	Description string
	// Text is the event body: the script's own output, a catch-up notice, or
	// the auto-kill message.
	Text string
	// Wrapped is the wire text carried into the conversation.
	Wrapped string
}

// Completion is one task's single terminal report.
type Completion struct {
	Snapshot Snapshot
	// Killed reports that a kill stopped the task rather than its own exit.
	Killed bool
	// Source names who stopped the task. It is meaningful only when Killed is
	// set, and it decides whether the terminal report still wakes the model.
	Source KillSource
	// KillResultDelivered reports that a Tool result already told the model the
	// kill outcome, so the terminal completion must not wake the model again.
	KillResultDelivered bool
}

// Observer receives one Registry's model-visible output.
type Observer interface {
	// BackgroundStarted reports one task registration, so a frontend can list
	// the task while it runs.
	BackgroundStarted(Snapshot)
	// BackgroundEvent delivers one streamed event.
	BackgroundEvent(Event)
	// BackgroundCompleted delivers one task's single terminal report.
	BackgroundCompleted(Completion)
}

// Start launches one registered task's process and streams its output into the
// sink. It returns when the process ends, when ctx is canceled, or when the
// sink fails.
type Start func(ctx context.Context, sink execution.Sink) (execution.Result, error)

// Request describes one background task to register and start.
type Request struct {
	Kind        Kind
	Command     string
	Description string
	CWD         string
	Owner       string
	// Timeout is the task's own deadline. It is ignored when NoDeadline is set.
	Timeout time.Duration
	// NoDeadline marks a task that runs until it exits, is killed, or the
	// Runtime closes.
	NoDeadline bool
	Start      Start
}

// KillSource identifies who stopped a task, and therefore whether its terminal
// completion may wake the model.
type KillSource uint8

const (
	// KillModel is the model's own stop Tool. It already produced a Tool
	// result, so its completion is delivered only when no result reached the
	// model.
	KillModel KillSource = iota
	// KillUser is the tasks-pane control or Runtime teardown. No Tool result
	// carries the outcome, so the completion still wakes the model.
	KillUser
	// KillPromptEnd is the cancel of the interaction that spawned the task. No
	// turn remains to report it and the user just interrupted, so its
	// completion produces no queue record at all.
	KillPromptEnd
)

// Outcome classifies one kill request.
type Outcome string

// Supported kill outcomes.
const (
	OutcomeKilled        Outcome = "killed"
	OutcomeAlreadyExited Outcome = "already_exited"
	OutcomeNotFound      Outcome = "not_found"
)

// Options configure one background registry.
type Options struct {
	// OutputDir is the directory each task's raw output file is written to: the
	// session's own terminal directory, never the workspace.
	OutputDir string
	// Observer receives events and completions. A nil Observer drops them.
	Observer Observer
	// KillToolName is the model-visible name of the Tool that stops a task,
	// substituted into the catch-up notice.
	KillToolName string
	// OutputBytes bounds one task's output file. It is the default for every
	// kind.
	OutputBytes int64
	// ShellOutputBytes bounds a shell task's output file. A command's log is
	// the command's whole output, so it takes the shell operation's own ceiling
	// rather than the narrower monitor cap. Zero uses OutputBytes.
	ShellOutputBytes int64
	// TailBytes bounds the output tail the pane's inspect read shows. A
	// completion inlines no output: it points at the task-output Tool.
	TailBytes int64
	// Now returns the current time; nil uses time.Now.
	Now func() time.Time
}

// Registry owns every background task of one Runtime.
type Registry struct {
	mu       sync.Mutex
	dir      string
	observer Observer
	killName string
	output   int64
	shellOut int64
	tail     int64
	now      func() time.Time
	// token makes this registry's task ids unique across processes. The counter
	// alone restarts at 1 with every process and can collide with an id already
	// named in the transcript.
	token  string
	next   uint64
	tasks  map[string]*task
	closed bool
	wg     sync.WaitGroup
}

// NewRegistry constructs one Runtime's background registry.
func NewRegistry(options Options) (*Registry, error) {
	if options.OutputDir == "" || options.OutputBytes <= 0 || options.TailBytes <= 0 ||
		options.ShellOutputBytes < 0 {
		return nil, ErrInvalid
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	shellOutput := options.ShellOutputBytes
	if shellOutput == 0 {
		shellOutput = options.OutputBytes
	}

	return &Registry{
		dir:      options.OutputDir,
		observer: options.Observer,
		killName: options.KillToolName,
		output:   options.OutputBytes,
		shellOut: shellOutput,
		tail:     options.TailBytes,
		now:      now,
		token:    newRegistryToken(),
		tasks:    make(map[string]*task),
	}, nil
}

// newRegistryToken returns the short random hex token that separates one
// registry's ids from another's, in this process or any other. A registry that
// cannot read the system randomness source falls back to its start time, which
// still separates two live processes.
func newRegistryToken() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}

	return hex.EncodeToString(raw[:])
}

// outputLimit is the output-file bound for one task kind: a shell task's log is
// the command's whole output and takes the shell ceiling, everything else takes
// the default.
func (r *Registry) outputLimit(kind Kind) int64 {
	if kind == KindShell {
		return r.shellOut
	}

	return r.output
}

// Launch registers one task and starts its process. It returns the task's
// snapshot as soon as a goroutine owns the run, so a Tool call returns a
// receipt without waiting for the process to end.
func (r *Registry) Launch(parent context.Context, request Request) (Snapshot, error) {
	if r == nil || request.Start == nil || request.Kind == "" || parent == nil {
		return Snapshot{}, ErrInvalid
	}

	if !request.NoDeadline && request.Timeout <= 0 {
		return Snapshot{}, ErrInvalid
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return Snapshot{}, ErrClosed
	}

	r.next++
	id := string(request.Kind) + "-" + strconv.FormatUint(r.next, 10) + "-" + r.token

	if !validTaskID(id) {
		r.mu.Unlock()

		return Snapshot{}, ErrInvalid
	}

	output, err := createOutputFile(r.dir, id+".log", r.outputLimit(request.Kind))
	if err != nil {
		r.mu.Unlock()

		return Snapshot{}, err
	}

	ctx, cancel := taskContext(parent, request)
	entry := &task{
		registry: r, id: id, kind: request.Kind,
		command: request.Command, description: request.Description,
		cwd: request.CWD, owner: request.Owner,
		start: r.now(), cancel: cancel, output: output, ended: make(chan struct{}),
	}
	r.tasks[id] = entry
	r.wg.Add(1)

	snapshot := entry.snapshotLocked()
	r.mu.Unlock()

	if r.observer != nil {
		r.observer.BackgroundStarted(snapshot)
	}

	go entry.run(ctx, request.Start)

	return snapshot, nil
}

// taskContext bounds one task by its own deadline, or leaves it unbounded for a
// no-deadline task.
func taskContext(parent context.Context, request Request) (context.Context, context.CancelFunc) {
	base, cancelBase := context.WithCancel(parent)
	if request.NoDeadline {
		return base, cancelBase
	}

	bounded, cancelBounded := context.WithTimeout(base, request.Timeout)

	return bounded, func() { cancelBounded(); cancelBase() }
}

// Kill stops one task. A running task is killed, a task that already ended is
// reported as already_exited, and an unknown id as not_found.
func (r *Registry) Kill(id string, source KillSource) Outcome {
	if r == nil {
		return OutcomeNotFound
	}

	r.mu.Lock()

	entry, ok := r.tasks[id]
	if !ok {
		r.mu.Unlock()

		return OutcomeNotFound
	}

	if entry.completed {
		r.mu.Unlock()

		return OutcomeAlreadyExited
	}

	if !entry.killed {
		entry.killed = true
		entry.source = source
		entry.delivered = source == KillModel
	}

	cancel := entry.cancel
	r.mu.Unlock()

	cancel()

	return OutcomeKilled
}

// CancelOwned stops every live task whose owner is the given interaction id and
// reports how many it stopped. An empty owner matches nothing, so a task
// registered without an owner is never cancelled by an interaction boundary.
func (r *Registry) CancelOwned(owner string, source KillSource) int {
	if r == nil || owner == "" {
		return 0
	}

	r.mu.Lock()

	cancels := make([]context.CancelFunc, 0, 1)

	for _, entry := range r.tasks {
		if entry.completed || entry.owner != owner {
			continue
		}

		if !entry.killed {
			entry.killed = true
			entry.source = source
			entry.delivered = source == KillModel
		}

		cancels = append(cancels, entry.cancel)
	}
	r.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	return len(cancels)
}

// Snapshot returns one task's current view. A task that is still running has a
// zero End and a zero ExitCode.
func (r *Registry) Snapshot(id string) (Snapshot, bool) {
	if r == nil {
		return Snapshot{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.tasks[id]
	if !ok {
		return Snapshot{}, false
	}

	return entry.snapshotLocked(), true
}

// List returns every registered task, oldest first.
func (r *Registry) List() []Snapshot {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	values := make([]Snapshot, 0, len(r.tasks))

	for _, entry := range r.tasks {
		values = append(values, entry.snapshotLocked())
	}

	sortSnapshots(values)

	return values
}

// Tail returns a bounded, sanitized tail of one task's own output. It is what
// the tasks pane's inspect action shows; the model's delivery stays the events.
func (r *Registry) Tail(id string, limit int64) (string, bool) {
	if r == nil || limit <= 0 {
		return "", false
	}

	r.mu.Lock()
	entry, found := r.tasks[id]
	path := ""

	if found && entry.output != nil {
		path = entry.output.path
	}
	r.mu.Unlock()

	if !found {
		return "", false
	}

	return tailBytes(path, limit), true
}

// OutputPath returns the task's own log path, `<dir>/<id>.log`. It is the path
// the task-output record reports as output_file and the only path the read Tool
// admits from outside the workspace.
func (r *Registry) OutputPath(id string) (string, bool) {
	if r == nil {
		return "", false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.tasks[id]
	if !ok || entry.output == nil {
		return "", false
	}

	return entry.output.path, true
}

// Result returns one finished task's own executor result and its run error,
// which is what a positive block renders a command that ended inside the wait
// from. A task that is still running, or an unknown id, reports false.
func (r *Registry) Result(id string) (execution.Result, bool, error) {
	if r == nil {
		return execution.Result{}, false, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.tasks[id]
	if !ok || !entry.completed {
		return execution.Result{}, false, nil
	}

	return entry.result, true, entry.runErr
}

// ResultTaken reports whether a task's result is already the model's: a read is
// standing by for it, or a read already returned its terminal record. The
// delivery path consults it live, so a result is carried by exactly one surface
// however the two order.
func (r *Registry) ResultTaken(id string) bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.tasks[id]

	return ok && (entry.waiters > 0 || entry.taken)
}

// TaskOutput returns one task's record. An unknown id yields a record with
// Found == false rather than an error, so a multi read can render the unknown
// entry beside the known ones. A terminal record is marked taken: the read is
// now the surface that carries it.
func (r *Registry) TaskOutput(id string) TaskOutput {
	if r == nil {
		return NotFoundTaskOutput(id)
	}

	r.mu.Lock()
	entry, ok := r.tasks[id]

	var (
		snapshot Snapshot
		killed   bool
		timedOut bool
		path     string
	)
	if ok {
		snapshot = entry.snapshotLocked()
		killed = entry.killed
		timedOut = entry.timedOut

		if entry.output != nil {
			path = entry.output.path
		}

		if entry.completed {
			entry.taken = true
		}
	}
	r.mu.Unlock()

	if !ok {
		return NotFoundTaskOutput(id)
	}

	return taskOutputFromSnapshot(snapshot, killed, timedOut, path, r.now())
}

// TaskOutputs returns one record per id, ordered as the ids were asked.
func (r *Registry) TaskOutputs(ids []string) []TaskOutput {
	outputs := make([]TaskOutput, 0, len(ids))

	for _, id := range ids {
		outputs = append(outputs, r.TaskOutput(id))
	}

	return outputs
}

// Wait resolves every id immediately, then blocks on the still-running ones
// until they all end, until any one ends when waitAny is set, or until the
// budget or ctx elapses. It is event-driven: each still-running task's own ended
// channel is a select case, so nothing polls.
//
// A waiter registers on a still-running task under the registry lock before it
// blocks, so the delivery path sees ResultTaken while the wait is inside its
// call. On return the registration is released and every terminal record is
// marked taken by TaskOutputs, so the delivery path sees ResultTaken after the
// wait as well. Exactly one surface therefore carries the outcome, whichever of
// the two the delivery reaches first.
func (r *Registry) Wait(
	ctx context.Context,
	ids []string,
	waitAny bool,
	budget time.Duration,
) ([]TaskOutput, WaitOutcome) {
	if r == nil {
		return r.TaskOutputs(ids), WaitOutcomeCompleted
	}

	r.mu.Lock()

	waiting := make([]*task, 0, len(ids))
	for _, id := range ids {
		entry, ok := r.tasks[id]
		if !ok || entry.completed {
			continue
		}

		entry.waiters++
		waiting = append(waiting, entry)
	}
	r.mu.Unlock()

	outcome := WaitOutcomeCompleted
	if len(waiting) > 0 {
		// An already-cancelled context never reached await, so the wait did not
		// happen: report the cancellation rather than a completion no task met.
		outcome = WaitOutcomeCanceled

		if ctx.Err() == nil {
			outcome = r.await(ctx, waiting, waitAny, budget)
		}
	}

	r.mu.Lock()
	for _, entry := range waiting {
		entry.waiters--

		if entry.completed {
			entry.taken = true
		}
	}
	r.mu.Unlock()

	return r.TaskOutputs(ids), outcome
}

// await selects over the registered tasks' own ended channels, the context, and
// the budget. It returns as soon as the wait condition is met.
func (r *Registry) await(
	ctx context.Context,
	waiting []*task,
	waitAny bool,
	budget time.Duration,
) WaitOutcome {
	if budget <= 0 {
		return WaitOutcomeDeadline
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()

	cases := make([]reflect.SelectCase, 0, len(waiting)+2)
	cases = append(
		cases,
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)},
	)

	for _, entry := range waiting {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(entry.ended)})
	}

	for {
		chosen, _, _ := reflect.Select(cases)

		switch chosen {
		case 0:
			return WaitOutcomeCanceled
		case 1:
			return WaitOutcomeDeadline
		}

		if waitAny {
			return WaitOutcomeCompleted
		}

		// Disable the fired channel so the next select cannot spin on it.
		cases[chosen] = reflect.SelectCase{Dir: reflect.SelectRecv}

		if r.allEnded(waiting) {
			return WaitOutcomeCompleted
		}
	}
}

// allEnded reports whether every registered task has completed.
func (r *Registry) allEnded(waiting []*task) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, entry := range waiting {
		if !entry.completed {
			return false
		}
	}

	return true
}

// Reap stops every live task and waits for their pipelines to end. Runtime
// teardown calls it, so no task outlives its session.
func (r *Registry) Reap(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	r.closed = true
	cancels := make([]context.CancelFunc, 0, len(r.tasks))

	for _, entry := range r.tasks {
		if entry.completed {
			continue
		}

		entry.killed = true
		entry.source = KillUser
		cancels = append(cancels, entry.cancel)
	}

	r.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		r.wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// task is one registered background task and its streaming pipeline.
type task struct {
	registry *Registry

	id          string
	kind        Kind
	command     string
	description string
	cwd         string
	owner       string

	start time.Time
	end   time.Time
	exit  int
	sig   string

	completed bool
	killed    bool
	timedOut  bool
	delivered bool
	// waiters counts the reads standing by for this task's terminal record, and
	// taken marks a terminal record a read already returned. The delivery path
	// consults both live, so exactly one surface carries a result. The count is
	// what keeps one read's release from clearing another read's registration.
	waiters int
	taken   bool
	source  KillSource

	// result is the executor's own outcome, kept so a positive block renders a
	// command that ended inside the wait through the shell's own renderer, and
	// runErr is the error beside it, which that renderer needs to tell a deadline
	// apart from a plain non-zero exit.
	result execution.Result
	runErr error

	output *outputFile
	cancel context.CancelFunc
	ended  chan struct{}
}

// run owns one task's process. A monitor's line pipeline starts first so no
// output is lost, then the process runs to completion; a shell task has no
// pipeline, because a command's log is its output and its completion is the
// single model-visible report.
func (t *task) run(ctx context.Context, start Start) {
	defer t.registry.wg.Done()

	streamed := make(chan struct{})
	if t.kind == KindShell {
		close(streamed)
	} else {
		go func() {
			defer close(streamed)

			t.stream()
		}()
	}

	// The executor classifies a run failure into result.Status, and the error is
	// kept beside it because a positive block renders a command that ended inside
	// the wait through the same renderer the synchronous path uses, and that
	// renderer tells a deadline apart from a plain non-zero exit by the error.
	result, runErr := start(ctx, t)

	_ = t.output.close()
	t.finish(result, runErr)

	<-streamed
	t.report()
}

// WriteOutput implements execution.Sink for one task's log. A monitor's sink
// keeps stdout alone: it reports the lines of the command it watches. A shell
// task's log is the command's whole output, so it keeps both streams.
func (t *task) WriteOutput(_ context.Context, chunk execution.OutputChunk) error {
	if chunk.Stream != execution.StreamStdout && t.kind != KindShell {
		return nil
	}

	return t.output.write(chunk.Data)
}

// stream reads the output file on every tick, assembles complete lines, and
// applies the rate limiter. It stops when the process has ended, after flushing
// the last partial line, or when the limiter has auto-killed the pipeline.
func (t *task) stream() {
	processor := &LineProcessor{}
	limiter := newRateLimiter(t.registry.killName, t.registry.now)

	var offset int64

	for {
		chunk, next, err := readFrom(t.output.path, offset, ReadLimit)
		if err == nil {
			offset = next
		}

		if len(chunk) > 0 {
			if lines := processor.Push(chunk); len(lines) > 0 {
				t.deliver(batchLines(lines), limiter)
			}
		}

		select {
		case <-t.ended:
			if line, ok := processor.Flush(); ok {
				t.deliver(line, limiter)
			}

			return
		default:
		}

		if limiter.killed {
			return
		}

		time.Sleep(PollInterval)
	}
}

// deliver routes one event body through the limiter. A suppressed event is
// dropped silently; a catch-up notice precedes the event that ends a burst; and
// the auto-kill message replaces the event that tripped it.
func (t *task) deliver(text string, limiter *rateLimiter) {
	verdict, message := limiter.process()

	switch verdict {
	case decisionSuppressed:
		return
	case decisionAutoKill:
		t.emit(message)
	default:
		if message != "" {
			t.emit(message)
		}

		t.emit(text)
	}
}

func (t *task) emit(text string) {
	observer := t.registry.observer
	if observer == nil {
		return
	}

	observer.BackgroundEvent(Event{
		TaskID: t.id, Description: t.description, Text: text,
		Wrapped: wrapEvent(t.description, text, t.id),
	})
}

// finish records the process outcome. It runs after the executor has drained
// every output reader and the pipeline has read the file, so the recorded state
// describes the whole run.
func (t *task) finish(result execution.Result, runErr error) {
	t.registry.mu.Lock()
	t.completed = true
	t.end = t.registry.now()
	t.exit = result.ExitCode
	t.sig = result.Signal
	t.timedOut = result.Status == execution.StatusTimedOut
	t.result = result
	t.runErr = runErr
	t.registry.mu.Unlock()

	close(t.ended)
}

// report emits the task's single terminal model-visible completion.
func (t *task) report() {
	observer := t.registry.observer
	if observer == nil {
		return
	}

	t.registry.mu.Lock()
	snapshot := t.snapshotLocked()
	killed := t.killed
	source := t.source
	delivered := t.delivered
	t.registry.mu.Unlock()

	observer.BackgroundCompleted(Completion{
		Snapshot: snapshot, Killed: killed, Source: source,
		KillResultDelivered: delivered,
	})
}

// snapshotLocked projects one task. The caller holds the registry lock.
func (t *task) snapshotLocked() Snapshot {
	return Snapshot{
		ID: t.id, Command: t.command, Description: t.description, CWD: t.cwd,
		Start: t.start, End: t.end, ExitCode: t.exit, Signal: t.sig,
		Completed: t.completed, Killed: t.killed, TimedOut: t.timedOut,
		Kind: t.kind, Owner: t.owner,
	}
}

// validTaskID reports whether a generated id is safe to use in a file name.
func validTaskID(id string) bool {
	if id == "" {
		return false
	}

	for _, character := range id {
		if character == '/' || character == '\\' || character == 0 {
			return false
		}
	}

	return true
}

// sortSnapshots orders tasks by start time, so the pane, the Stop hook, and the
// completion text all read the same stable sequence.
func sortSnapshots(values []Snapshot) {
	slices.SortStableFunc(values, func(left, right Snapshot) int {
		return left.Start.Compare(right.Start)
	})
}
