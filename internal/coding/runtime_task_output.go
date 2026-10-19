package coding

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

// childResultLedger is this Runtime's live record of which children's results a
// Tool read already carries. It is the child-side twin of the registry's own
// ResultTaken predicate, and it exists for the same reason: a result is carried
// by exactly one surface, so a completion a read already took must not wake the
// model as well. A blocking read registers before it blocks and the taken mark
// is set on the way out, so the delivery path sees the live state whichever
// order the two reach it.
type childResultLedger struct {
	mu      sync.Mutex
	waiters map[string]int
	taken   map[string]struct{}
}

func newChildResultLedger() *childResultLedger {
	return &childResultLedger{
		waiters: make(map[string]int),
		taken:   make(map[string]struct{}),
	}
}

// begin registers one blocking read before it blocks.
func (l *childResultLedger) begin(childSessionID string) {
	if l == nil || childSessionID == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.waiters == nil {
		l.waiters = make(map[string]int)
	}

	l.waiters[childSessionID]++
}

// end releases one blocking read's registration. The caller marks the result
// with take when the read carried a terminal record.
func (l *childResultLedger) end(childSessionID string) {
	if l == nil || childSessionID == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.waiters[childSessionID] <= 1 {
		delete(l.waiters, childSessionID)

		return
	}

	l.waiters[childSessionID]--
}

// take marks one child's terminal result as already carried by a read.
func (l *childResultLedger) take(childSessionID string) {
	if l == nil || childSessionID == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.taken == nil {
		l.taken = make(map[string]struct{})
	}

	l.taken[childSessionID] = struct{}{}
}

// takenBy reports whether a read is standing by for the child or already
// carries its terminal result.
func (l *childResultLedger) takenBy(childSessionID string) bool {
	if l == nil || childSessionID == "" {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.waiters[childSessionID] > 0 {
		return true
	}

	_, ok := l.taken[childSessionID]

	return ok
}

// childResultTaken asks the ledger whether one child's result is already the
// model's. The delivery path consults it live when it renders a completion.
func (r *Runtime) childResultTaken(childSessionID string) bool {
	if r == nil {
		return false
	}

	return r.childResults.takenBy(childSessionID)
}

// waitTaskOutputs resolves the asked ids over both producers. The registry's
// ids wait inside the registry's own event-driven wait; each child id waits
// through its own bounded child wait. wait_any returns at the first resolution
// of either kind and cancels the waits it left behind. The records are read
// after the wait, with the caller's context, so an elapsed or cancelled wait
// still reports what each id has.
func (r *Runtime) waitTaskOutputs(
	ctx context.Context,
	ids []string,
	mode string,
	budget time.Duration,
) ([]background.TaskOutput, background.WaitOutcome) {
	waitAny := mode == tools.TaskOutputModeWaitAny

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	registryIDs := make([]string, 0, len(ids))
	childIDs := make([]string, 0, len(ids))

	for _, id := range ids {
		if r.registryKnows(id) {
			registryIDs = append(registryIDs, id)

			continue
		}
		// An id that is not a child of this session is not a producer to wait
		// on: it reads as the not-found record, and it must not resolve a
		// wait_any the way a real completion does. The registry's own wait
		// skips an id it does not know the same way.
		if _, ok := r.childTaskOutput(ctx, id); ok {
			childIDs = append(childIDs, id)
		}
	}

	jobs := make([]waitJob, 0, len(registryIDs)+len(childIDs))
	if len(registryIDs) > 0 {
		jobs = append(jobs, func(ctx context.Context) background.WaitOutcome {
			_, outcome := r.backgroundTasks.Wait(ctx, registryIDs, waitAny, budget)

			return outcome
		})
	}

	for _, id := range childIDs {
		jobs = append(jobs, func(ctx context.Context) background.WaitOutcome {
			return r.childWait(ctx, id, budget)
		})
	}

	outcome := awaitWaitOutcomes(waitCtx, jobs, waitAny)

	return r.pollTaskOutputs(ctx, ids), outcome
}

// waitJob is one producer's wait: it blocks until its own ids resolve, its
// budget elapses, or its context ends, and reports which of the three happened.
type waitJob func(context.Context) background.WaitOutcome

// awaitWaitOutcomes runs the producer waits together. wait_any returns at the
// first wait that resolved and cancels the ones it left behind; otherwise every
// wait is joined and the strongest outcome wins, because a deadline or a
// cancellation must not be reported as a completion.
func awaitWaitOutcomes(
	ctx context.Context,
	jobs []waitJob,
	waitAny bool,
) background.WaitOutcome {
	if len(jobs) == 0 {
		return background.WaitOutcomeCompleted
	}

	if !waitAny {
		outcomes := make([]background.WaitOutcome, len(jobs))

		var group sync.WaitGroup

		for index, job := range jobs {
			group.Add(1)

			go func(index int, job waitJob) {
				defer group.Done()

				outcomes[index] = job(ctx)
			}(index, job)
		}

		group.Wait()

		return combineWaitOutcomes(outcomes)
	}

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outcomes := make(chan background.WaitOutcome, len(jobs))
	for _, job := range jobs {
		go func(job waitJob) { outcomes <- job(waitCtx) }(job)
	}

	canceled := false

	for range jobs {
		outcome := <-outcomes
		if outcome == background.WaitOutcomeCompleted {
			return outcome
		}

		if outcome == background.WaitOutcomeCanceled {
			canceled = true
		}
	}

	if canceled {
		return background.WaitOutcomeCanceled
	}

	return background.WaitOutcomeDeadline
}

// combineWaitOutcomes folds the producer waits a wait-all joined: a
// cancellation wins, then a deadline, and only then a completion.
func combineWaitOutcomes(outcomes []background.WaitOutcome) background.WaitOutcome {
	result := background.WaitOutcomeCompleted

	for _, outcome := range outcomes {
		if outcome == background.WaitOutcomeCanceled {
			return background.WaitOutcomeCanceled
		}

		if outcome == background.WaitOutcomeDeadline {
			result = background.WaitOutcomeDeadline
		}
	}

	return result
}

// childWait waits for one child id to reach a terminal state, bounded by the
// same clamped budget the registry's own wait takes. The record comes from a
// read after the wait rather than from the wait's result: Manager.Wait reports
// an error for a terminal child that did not succeed, so the outcome has to
// come from the lifecycle. A child that has already ended resolves at once,
// which is how a wait-any answers an id that needs no waiting.
func (r *Runtime) childWait(
	ctx context.Context,
	childSessionID string,
	budget time.Duration,
) background.WaitOutcome {
	if r == nil || r.subagents == nil {
		return background.WaitOutcomeCompleted
	}

	// Register before blocking, so a completion rendered while this read stands
	// by is suppressed. The registration is released on the way out, and the
	// result is marked taken only when the read carries a terminal record.
	r.childResults.begin(childSessionID)
	defer r.childResults.end(childSessionID)

	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	_, _ = r.subagents.Wait(waitCtx, childSessionID)

	record, ok := r.childTaskOutput(ctx, childSessionID)

	switch {
	case ctx.Err() != nil:
		return background.WaitOutcomeCanceled
	case !ok || !record.Live():
		return background.WaitOutcomeCompleted
	default:
		return background.WaitOutcomeDeadline
	}
}

// childTaskOutput resolves one of this session's children into the record the
// read Tools report. An id that is not a child of this session reads as absent,
// so the resolver falls through to the same not-found record an unknown
// registry id yields. A read that returns a terminal record takes the child's
// result: that read is now the surface that carries it, and the completion must
// not wake the model as well.
func (r *Runtime) childTaskOutput(
	ctx context.Context,
	childSessionID string,
) (background.TaskOutput, bool) {
	if r == nil || r.subagents == nil || childSessionID == "" {
		return background.TaskOutput{}, false
	}

	detail, err := r.subagents.Inspect(ctx, childSessionID)
	if err != nil {
		return background.TaskOutput{}, false
	}

	record := r.childOutputRecord(detail)
	if !record.Live() {
		r.childResults.take(childSessionID)
	}

	return record, true
}

// childOutputRecord projects one inspected child into the same record the
// registry produces, so the Tool layer, the multi shape, and the renderer are
// untouched by a third id source. The child's session id is the task id, its
// identity and task preview make the command line, and it names no output file
// because a child has no log.
func (r *Runtime) childOutputRecord(detail subagent.Detail) background.TaskOutput {
	summary := detail.Summary
	record := background.TaskOutput{
		Found:        true,
		Subject:      background.SubjectSubagent,
		TaskID:       summary.ChildSessionID,
		Command:      "[subagent:" + childIdentityName(summary) + "] " + summary.TaskPreview,
		Status:       childOutputStatus(summary.State),
		Started:      summary.CreatedAt,
		DurationSecs: summary.Duration.Seconds(),
	}

	if !record.Live() {
		record.Ended = summary.CreatedAt.Add(summary.Duration)
	}

	record.ExitCode = childOutputExitCode(summary.State)
	record.Output, record.Truncated, record.RawOutputBytes = background.BoundTaskOutputText(
		r.childOutputBody(detail),
	)

	if record.Truncated {
		// A child's result has no file behind it, so its hint reports the real
		// length rather than naming the read Tool and a path.
		record.TruncationHint = background.TotalTruncationHint(record.RawOutputBytes)
	}

	return record
}

// childOutputBody renders the body one child record carries: the progress block
// while the child runs, and the result rendering once it is terminal. Both live
// here, so a reader has one place to look for what a child reports.
func (r *Runtime) childOutputBody(detail subagent.Detail) string {
	if !terminalSubagentState(detail.Summary.State) {
		return r.childLiveOutputBody(detail)
	}

	return childTerminalOutputBody(detail)
}

// childLiveOutputBody renders a child that has not reached a terminal state. It
// is the reference's progress block: what the child is, what it was asked for,
// how long it has run, and what it has done so far. A child whose durable
// lifecycle exists but whose run has not started is initializing, which the
// reference reports without a progress line.
func (r *Runtime) childLiveOutputBody(detail subagent.Detail) string {
	summary := detail.Summary
	headline := "Subagent is still running."
	lines := []string{
		"Type: " + childIdentityName(summary),
		"Description: " + summary.TaskPreview,
		"Elapsed: " + formatChildElapsed(summary.Duration),
	}

	if summary.State == subagent.StateCreated {
		headline = "Subagent is initializing (creating worktree, resolving config)."

		return headline + "\n" + strings.Join(lines, "\n")
	}

	tokens := childTokensUsed(summary.Usage)
	capacity := r.childContextWindow(detail.Plan)

	lines = append(lines,
		fmt.Sprintf(
			"Progress: turn %d, %d tool calls, %dK/%dK tokens (%d%% context)",
			summary.Turns, childToolCalls(summary, detail.Activity), tokens/1000, capacity/1000,
			childContextPercent(tokens, capacity),
		),
		"Tools used: "+childToolsUsed(detail.Activity),
		fmt.Sprintf("Errors: %d", childErrorCount(detail.Activity)),
	)

	return headline + "\n" + strings.Join(lines, "\n")
}

// childTerminalOutputBody renders a terminal child through the same rendering
// the spawn result prints, so a read of a child and the spawn that produced it
// cannot drift. A child that did not succeed has no result value, so its code
// becomes the result line: the closed status vocabulary has no word for an
// interrupted child, and the code is what keeps that visible.
func childTerminalOutputBody(detail subagent.Detail) string {
	result := subagent.ResultFromDetail(detail)
	if result.Outcome != subagent.OutcomeSucceeded && result.Value == nil {
		result.Value = childTerminalCode(detail.Summary)
	}

	return subagent.FormatResultText(result)
}

// childTerminalCode is the line a child that did not succeed reports where a
// completed child reports its answer. It mirrors the reference, which reports a
// failed child's error and a cancelled child's reason there.
func childTerminalCode(summary subagent.Summary) string {
	if summary.Code != "" {
		return summary.Code
	}

	if summary.State == subagent.StateCanceled || summary.State == subagent.StateInterrupted {
		return "Subagent was cancelled"
	}

	return "Subagent failed"
}

// childOutputStatus maps pips' child lifecycle onto the reference's closed
// status vocabulary. An interrupted child reads `cancelled` too; its own code
// stays in the body. A failed child and anything unrecognized read `failed`.
func childOutputStatus(state subagent.State) string {
	switch state {
	case subagent.StateCreated:
		return background.StatusInitializing
	case subagent.StateRunning:
		return background.StatusRunning
	case subagent.StateSucceeded:
		return background.StatusCompleted
	case subagent.StateCanceled, subagent.StateInterrupted:
		return background.StatusCancelled
	default:
		return background.StatusFailed
	}
}

// childOutputExitCode follows the reference: a completed child exits zero, a
// failed one exits one, and a cancelled or interrupted child has no exit code.
func childOutputExitCode(state subagent.State) *int {
	switch state {
	case subagent.StateSucceeded:
		code := 0

		return &code
	case subagent.StateFailed:
		code := 1

		return &code
	default:
		return nil
	}
}

// childIdentityName is the child's type as a reader sees it: the canonical
// identity id, the legacy role alias for a record that carries none, and the
// generic name as the last resort.
func childIdentityName(summary subagent.Summary) string {
	if summary.Identity.ID != "" {
		return summary.Identity.ID
	}

	if summary.Role != "" {
		return string(summary.Role)
	}

	return "subagent"
}

// childTokensUsed is the child's cumulative token count, which is what the
// reference's progress line reports.
func childTokensUsed(usage ai.Usage) int {
	return usage.InputTokens + usage.OutputTokens
}

// childContextWindow resolves the child's own model window, so the progress
// line measures the context share against the model the child actually ran. It
// falls back to this Runtime's resolved window when the frozen plan names a
// model the catalogue no longer resolves.
func (r *Runtime) childContextWindow(plan subagent.ExecutionPlan) int {
	if r == nil {
		return 0
	}

	_, _, window := r.childSessionModel(plan.Model)

	return window
}

// childContextPercent is the child's share of its own context window as a whole
// percentage. A window that did not resolve reports zero rather than dividing
// by it.
func childContextPercent(tokens, capacity int) int {
	if capacity <= 0 || tokens <= 0 {
		return 0
	}

	return tokens * 100 / capacity
}

// childToolCalls is the tool-call count the progress line reports. The durable
// summary carries the authoritative count once the child ends; while it runs,
// the live activity list is the only count there is, and taking the larger keeps
// the progress line and the tools-used line from disagreeing.
func childToolCalls(summary subagent.Summary, activity subagent.Activity) int {
	return max(summary.ToolCalls, len(activity.Tools))
}

// childToolsUsed names the tools a live child has called, in first-seen order.
// The reference reports "none yet" for a child that has called none.
func childToolsUsed(activity subagent.Activity) string {
	names := make([]string, 0, len(activity.Tools))

	for _, tool := range activity.Tools {
		name := strings.TrimSpace(tool.Call.Name)
		if name == "" || slices.Contains(names, name) {
			continue
		}

		names = append(names, name)
	}

	if len(names) == 0 {
		return "none yet"
	}

	return strings.Join(names, ", ")
}

// childErrorCount counts the tool calls a live child has had report an error.
func childErrorCount(activity subagent.Activity) int {
	count := 0

	for _, tool := range activity.Tools {
		if tool.Result.IsError {
			count++
		}
	}

	return count
}

// formatChildElapsed renders a child's elapsed time at the reference's
// one-decimal precision.
func formatChildElapsed(duration time.Duration) string {
	return strconv.FormatFloat(duration.Seconds(), 'f', 1, 64) + "s"
}
