package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rsbin/pips/ai"
)

// batchOutcome is what one turn's tool execution produced.
type batchOutcome struct {
	// results answer the leading calls in call order — denials, failures,
	// and cancellations included, so every answered call has exactly one
	// result.
	results []ai.ToolResultPart
	// pending holds the calls from a [Pause] truncation point on; nil when
	// the gate let the whole batch proceed.
	pending []ai.ToolCallPart
	// terminated reports that every result in the batch carried the
	// [ErrTerminate] hint.
	terminated bool
	// stopped reports that the stream consumer stopped iterating mid-batch;
	// no further events may be emitted.
	stopped bool
}

// execOutcome is one call's internal result: the wire part plus runtime
// bookkeeping that never reaches the session.
type execOutcome struct {
	part ai.ToolResultPart
	// terminated is the tool's ErrTerminate hint.
	terminated bool
	// executed reports that Exec was invoked, making the result eligible for
	// the [WithAfterTool] hook (immediate outcomes — unknown tool, denial,
	// cancellation — are not).
	executed bool
}

// execBatch gates and executes one turn's tool calls. cancel aborts the run
// context when the consumer stops mid-batch so in-flight tools wind down.
func (a *Agent) execBatch(ctx context.Context, tools *toolbox, cancel context.CancelFunc, turn int, calls []ai.ToolCallPart, emit emitFunc) batchOutcome {
	runnable, denials, pending := a.gateCalls(ctx, turn, calls)

	b := &batchExec{
		agent:    a,
		tools:    tools,
		cancel:   cancel,
		turn:     turn,
		emitFn:   emit,
		calls:    runnable,
		denials:  denials,
		outcomes: make([]execOutcome, len(runnable)),
		sem:      make(chan struct{}, a.cfg.parallelTools),
		done:     make(chan int, len(runnable)),
		progress: make(chan EventPayload, progressBuffer),
	}
	b.run(ctx)

	return batchOutcome{
		results:    b.results(),
		pending:    pending,
		terminated: b.terminated(),
		stopped:    b.stopped,
	}
}

// progressBuffer bounds queued ReportProgress events per batch; overflow is
// dropped (progress is advisory).
const progressBuffer = 32

// gateCalls consults the gate for each call in order. It returns the prefix
// of calls that may proceed, a parallel slice of denial results (nil entries
// mean allowed), and the pending suffix when the gate paused. Without a gate
// every call proceeds.
func (a *Agent) gateCalls(ctx context.Context, turn int, calls []ai.ToolCallPart) ([]ai.ToolCallPart, []*ai.ToolResultPart, []ai.ToolCallPart) {
	denials := make([]*ai.ToolResultPart, len(calls))
	if a.cfg.beforeTool == nil {
		return calls, denials, nil
	}
	runnable := slices.Clone(calls)

	for idx, call := range runnable {
		info := ToolCallInfo{
			ToolCall:   ToolCall{ID: call.ID, Name: call.Name, Args: call.Args},
			Turn:       turn,
			BatchIndex: idx,
			BatchSize:  len(calls),
		}

		decision := a.cfg.beforeTool(ctx, info)
		if decision.UpdatedInput != nil {
			runnable[idx].Args = slices.Clone(decision.UpdatedInput)
			call = runnable[idx]
		}
		switch decision.Action {
		case ToolDecisionPause:
			return runnable[:idx], denials[:idx], slices.Clone(runnable[idx:])
		case ToolDecisionDeny:
			reason := decision.Reason
			if reason == "" {
				reason = "tool call denied"
			}

			result := errorResult(call, reason)
			denials[idx] = &result
		case ToolDecisionAllow:
		}
	}

	return runnable, denials, nil
}

// batchExec walks one turn's gated calls in order: denied calls settle
// instantly, [Parallel]-marked tools dispatch to bounded workers, and serial
// tools act as barriers that run inline. Results land at their call's index,
// so order is preserved regardless of completion order, and all events —
// including tool progress — are emitted from the walking goroutine (yield is
// not concurrency-safe).
type batchExec struct {
	agent    *Agent
	tools    *toolbox
	cancel   context.CancelFunc
	turn     int
	emitFn   emitFunc
	calls    []ai.ToolCallPart
	denials  []*ai.ToolResultPart
	outcomes []execOutcome

	// sem bounds concurrent workers; done carries finished worker indexes
	// back to the walker. done is buffered to len(calls) so workers never
	// block sending, even when the walker has bailed out. progress carries
	// ReportProgress events from tool goroutines (non-blocking sends).
	sem      chan struct{}
	done     chan int
	progress chan EventPayload
	inflight int
	stopped  bool
}

func (b *batchExec) run(ctx context.Context) {
	for idx := range b.calls {
		b.step(ctx, idx)
	}

	b.await(ctx, 0)
	b.drainProgress()
}

func (b *batchExec) step(ctx context.Context, idx int) {
	call := b.calls[idx]

	// A gone consumer or cancelled context settles the rest of the batch
	// without executing, keeping the call/result pairing intact.
	if b.stopped || ctx.Err() != nil {
		if denial := b.denials[idx]; denial != nil {
			b.settle(idx, *denial)
		} else {
			b.settle(idx, errorResult(call, "tool execution cancelled"))
		}

		return
	}

	if denial := b.denials[idx]; denial != nil {
		b.settle(idx, *denial)
		return
	}

	if b.tools.concurrent(call.Name) {
		b.dispatch(ctx, idx)
		return
	}

	// Serial tools barrier on all in-flight work, then run inline.
	b.await(ctx, 0)

	if b.stopped || ctx.Err() != nil {
		b.settle(idx, errorResult(call, "tool execution cancelled"))
		return
	}

	b.emit(ToolStarted{Turn: b.turn, Call: call})
	b.outcomes[idx] = b.agent.runTool(b.reporterCtx(ctx, call), b.tools, call)
	b.drainProgress()
	b.finalize(ctx, idx)
	b.emit(ToolCompleted{Turn: b.turn, Call: call, Result: b.outcomes[idx].part})
}

// settle records a result that did not come from an execution (denial or
// cancellation), emitting the tool's start/end pair.
func (b *batchExec) settle(idx int, result ai.ToolResultPart) {
	b.emit(ToolStarted{Turn: b.turn, Call: b.calls[idx]})
	b.outcomes[idx] = execOutcome{part: result}
	b.emit(ToolCompleted{Turn: b.turn, Call: b.calls[idx], Result: result})
}

// dispatch hands a call to a worker, waiting for a semaphore slot while
// forwarding completions and progress so events stay timely.
func (b *batchExec) dispatch(ctx context.Context, idx int) {
	call := b.calls[idx]
	toolCtx := b.reporterCtx(ctx, call)

	for {
		select {
		case ev := <-b.progress:
			b.emit(ev)
		case doneIdx := <-b.done:
			b.finish(ctx, doneIdx)
		case b.sem <- struct{}{}:
			b.emit(ToolStarted{Turn: b.turn, Call: call})
			b.inflight++

			go func() {
				defer func() { <-b.sem; b.done <- idx }()

				b.outcomes[idx] = b.agent.runTool(toolCtx, b.tools, call)
			}()

			return
		}
	}
}

// await drains completions and progress until at most target workers remain
// in flight.
func (b *batchExec) await(ctx context.Context, target int) {
	for b.inflight > target {
		select {
		case ev := <-b.progress:
			b.emit(ev)
		case idx := <-b.done:
			b.finish(ctx, idx)
		}
	}
}

// finish books one completed worker: apply the after-tool hook, then emit its
// ToolCompleted payload.
func (b *batchExec) finish(ctx context.Context, idx int) {
	b.inflight--
	b.finalize(ctx, idx)
	b.emit(ToolCompleted{
		Turn:   b.turn,
		Call:   b.calls[idx],
		Result: b.outcomes[idx].part,
	})
}

// finalize applies the [WithAfterTool] hook to an executed outcome. It runs
// on the walker goroutine, so hooks are serialized like gates. A hook panic
// converts the result into an error result (mirroring tool panics).
func (b *batchExec) finalize(ctx context.Context, idx int) {
	hook := b.agent.cfg.afterTool
	if hook == nil || !b.outcomes[idx].executed {
		return
	}

	call := b.calls[idx]

	defer func() {
		if r := recover(); r != nil {
			b.outcomes[idx] = execOutcome{
				part:     errorResult(call, fmt.Sprintf("after-tool hook panicked: %v", r)),
				executed: true,
			}
		}
	}()

	override := hook(ctx, ToolResultInfo{
		ToolCall: ToolCall{ID: call.ID, Name: call.Name, Args: call.Args},
		Turn:     b.turn,
		Result:   b.outcomes[idx].part,
	})
	if override == nil {
		return
	}

	outcome := &b.outcomes[idx]
	if override.Content != nil {
		outcome.part.Content = override.Content
	}

	if override.IsError != nil {
		outcome.part.IsError = *override.IsError
	}

	if override.Terminate != nil {
		outcome.terminated = *override.Terminate
	}
}

// drainProgress emits any queued progress events without blocking.
func (b *batchExec) drainProgress() {
	for {
		select {
		case ev := <-b.progress:
			b.emit(ev)
		default:
			return
		}
	}
}

// reporterCtx derives the tool context carrying a ReportProgress reporter
// that forwards updates to the walker (dropped when the buffer is full).
func (b *batchExec) reporterCtx(ctx context.Context, call ai.ToolCallPart) context.Context {
	return withProgress(ctx, func(parts []ai.Part) {
		select {
		case b.progress <- cloneEventPayload(ToolUpdated{Turn: b.turn, Call: call, Update: parts}):
		default:
		}
	})
}

// emit forwards an event unless the consumer already stopped; when the
// consumer stops here, the run context is cancelled so in-flight tools wind
// down.
func (b *batchExec) emit(payload EventPayload) {
	if b.stopped {
		return
	}

	if !b.emitFn(payload) {
		b.stopped = true
		b.cancel()
	}
}

// results extracts the wire parts in call order.
func (b *batchExec) results() []ai.ToolResultPart {
	parts := make([]ai.ToolResultPart, len(b.outcomes))
	for i, o := range b.outcomes {
		parts[i] = o.part
	}

	return parts
}

// terminated reports whether every result in the batch carried the
// termination hint.
func (b *batchExec) terminated() bool {
	if len(b.outcomes) == 0 {
		return false
	}

	for _, o := range b.outcomes {
		if !o.terminated {
			return false
		}
	}

	return true
}

// runTool executes one call, converting every failure mode — unknown tool,
// cancellation, timeout, execution error, panic — into an error tool result
// the model can react to. A tool returning [ErrTerminate] produces a
// successful result carrying the termination hint.
func (a *Agent) runTool(ctx context.Context, tools *toolbox, call ai.ToolCallPart) execOutcome {
	tool, ok := tools.byName[call.Name]
	if !ok {
		return execOutcome{part: errorResult(call, "unknown tool: "+call.Name)}
	}

	if ctx.Err() != nil {
		return execOutcome{part: errorResult(call, "tool execution cancelled")}
	}

	if a.cfg.toolTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, a.cfg.toolTimeout)
		defer cancel()
	}

	parts, err := safeExec(ctx, tool, ToolCall{ID: call.ID, Name: call.Name, Args: call.Args})

	switch {
	case errors.Is(err, ErrTerminate):
		return execOutcome{
			part:       ai.ToolResultPart{ToolCallID: call.ID, Name: call.Name, Content: parts},
			terminated: true,
			executed:   true,
		}
	case err != nil:
		return execOutcome{part: errorResult(call, err.Error()), executed: true}
	default:
		return execOutcome{
			part:     ai.ToolResultPart{ToolCallID: call.ID, Name: call.Name, Content: parts},
			executed: true,
		}
	}
}

// safeExec runs Exec with panic recovery, so a buggy tool degrades to an
// error result instead of crashing the calling process.
func safeExec(ctx context.Context, tool Tool, call ToolCall) (parts []ai.Part, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()

	return tool.Exec(ctx, call)
}

func errorResult(call ai.ToolCallPart, text string) ai.ToolResultPart {
	return ai.ToolResultPart{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    TextResult(text),
		IsError:    true,
	}
}
