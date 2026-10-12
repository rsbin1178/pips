// Package background owns the process-local registry of background tasks that
// outlive the Tool call that started them, and the stdout pipeline that turns
// one such task's output into bounded, model-visible events.
//
// Registry state is runtime state, never persisted: a running task does not
// survive a restart, and nothing about it rides the subagent execution plan.
package background

import (
	"context"
	"errors"
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
)

// KillTaskToolName is the model-visible name of the Tool that stops one
// background task. The monitor description, the start receipt, the running-task
// warning, and the catch-up notice all name it, so it has one definition.
const KillTaskToolName = "kill_background_task"

// Snapshot is one immutable view of a registered background task. It is the
// single per-task record every surface reads: the kill Tool, the tasks pane,
// the Stop hook, and telemetry.
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
	Kind        Kind
	Owner       string
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
	// Tail is a bounded, sanitized projection of the task's own output.
	Tail string
	// Killed reports that a kill stopped the task rather than its own exit.
	Killed bool
	// KillResultDelivered reports that a Tool result already told the model the
	// kill outcome, so the terminal completion must not wake the model again.
	KillResultDelivered bool
}

// Observer receives one Registry's model-visible output.
type Observer interface {
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

// KillSource identifies who stopped a task. A model kill already produced a
// Tool result, so its terminal completion must not wake the model again.
type KillSource uint8

const (
	// KillModel is the model's own stop Tool.
	KillModel KillSource = iota
	// KillUser is the tasks-pane control or Runtime teardown.
	KillUser
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
	// OutputBytes bounds one task's output file.
	OutputBytes int64
	// TailBytes bounds the output tail inlined in a completion.
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
	tail     int64
	now      func() time.Time
	next     uint64
	tasks    map[string]*task
	closed   bool
	wg       sync.WaitGroup
}

// NewRegistry constructs one Runtime's background registry.
func NewRegistry(options Options) (*Registry, error) {
	if options.OutputDir == "" || options.OutputBytes <= 0 || options.TailBytes <= 0 {
		return nil, ErrInvalid
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Registry{
		dir:      options.OutputDir,
		observer: options.Observer,
		killName: options.KillToolName,
		output:   options.OutputBytes,
		tail:     options.TailBytes,
		now:      now,
		tasks:    make(map[string]*task),
	}, nil
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
	id := string(request.Kind) + "-" + strconv.FormatUint(r.next, 10)

	if !validTaskID(id) {
		r.mu.Unlock()

		return Snapshot{}, ErrInvalid
	}

	output, err := createOutputFile(r.dir, id+".log", r.output)
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
		entry.delivered = source == KillModel
	}

	cancel := entry.cancel
	r.mu.Unlock()

	cancel()

	return OutcomeKilled
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
	delivered bool

	output *outputFile
	cancel context.CancelFunc
	ended  chan struct{}
}

// run owns one task's process and pipeline. The pipeline starts first so no
// output is lost, then the process runs to completion.
func (t *task) run(ctx context.Context, start Start) {
	defer t.registry.wg.Done()

	streamed := make(chan struct{})

	go func() {
		defer close(streamed)

		t.stream()
	}()

	// A run failure is already classified into result.Status by the executor,
	// so the recorded outcome is the result rather than a second error channel.
	result, _ := start(ctx, t)

	_ = t.output.close()
	t.finish(result)

	<-streamed
	t.report()
}

// WriteOutput implements execution.Sink for one task's stdout. Stderr is not an
// event source: a monitor reports the lines of the command it watches.
func (t *task) WriteOutput(_ context.Context, chunk execution.OutputChunk) error {
	if chunk.Stream != execution.StreamStdout {
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
func (t *task) finish(result execution.Result) {
	t.registry.mu.Lock()
	t.completed = true
	t.end = t.registry.now()
	t.exit = result.ExitCode
	t.sig = result.Signal
	t.registry.mu.Unlock()

	close(t.ended)
}

// report emits the task's single terminal model-visible completion.
func (t *task) report() {
	observer := t.registry.observer
	if observer == nil {
		return
	}

	tail := tailBytes(t.output.path, t.registry.tail)

	t.registry.mu.Lock()
	snapshot := t.snapshotLocked()
	killed := t.killed
	delivered := t.delivered
	t.registry.mu.Unlock()

	observer.BackgroundCompleted(Completion{
		Snapshot: snapshot, Tail: tail, Killed: killed, KillResultDelivered: delivered,
	})
}

// snapshotLocked projects one task. The caller holds the registry lock.
func (t *task) snapshotLocked() Snapshot {
	return Snapshot{
		ID: t.id, Command: t.command, Description: t.description, CWD: t.cwd,
		Start: t.start, End: t.end, ExitCode: t.exit, Signal: t.sig,
		Completed: t.completed, Kind: t.kind, Owner: t.owner,
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
