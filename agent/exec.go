package agent

import (
	"context"
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
	// stopped reports that the stream consumer stopped iterating mid-batch;
	// no further events may be emitted.
	stopped bool
}

// execBatch gates and executes one turn's tool calls. cancel aborts the run
// context when the consumer stops mid-batch so in-flight tools wind down.
func (a *Agent) execBatch(ctx context.Context, cancel context.CancelFunc, turn int, calls []ai.ToolCallPart, emit emitFunc) batchOutcome {
	runnable, denials, pending := a.gateCalls(ctx, turn, calls)

	b := &batchExec{
		agent:   a,
		cancel:  cancel,
		turn:    turn,
		emitFn:  emit,
		calls:   runnable,
		denials: denials,
		results: make([]ai.ToolResultPart, len(runnable)),
		sem:     make(chan struct{}, a.cfg.parallelTools),
		done:    make(chan int, len(runnable)),
	}
	b.run(ctx)

	return batchOutcome{results: b.results, pending: pending, stopped: b.stopped}
}

// gateCalls consults the gate for each call in order. It returns the prefix
// of calls that may proceed, a parallel slice of denial results (nil entries
// mean allowed), and the pending suffix when the gate paused. Without a gate
// every call proceeds.
func (a *Agent) gateCalls(ctx context.Context, turn int, calls []ai.ToolCallPart) ([]ai.ToolCallPart, []*ai.ToolResultPart, []ai.ToolCallPart) {
	denials := make([]*ai.ToolResultPart, len(calls))
	if a.cfg.beforeTool == nil {
		return calls, denials, nil
	}

	for idx, call := range calls {
		info := ToolCallInfo{
			ToolCall: ToolCall{ID: call.ID, Name: call.Name, Args: call.Args},
			Turn:     turn,
		}

		decision := a.cfg.beforeTool(ctx, info)
		switch decision.Action {
		case Pause:
			return calls[:idx], denials[:idx], slices.Clone(calls[idx:])
		case Deny:
			reason := decision.Reason
			if reason == "" {
				reason = "tool call denied"
			}

			result := errorResult(call, reason)
			denials[idx] = &result
		case Allow:
		}
	}

	return calls, denials, nil
}

// batchExec walks one turn's gated calls in order: denied calls settle
// instantly, [Parallel]-marked tools dispatch to bounded workers, and serial
// tools act as barriers that run inline. Results land at their call's index,
// so order is preserved regardless of completion order, and all events are
// emitted from the walking goroutine (yield is not concurrency-safe).
type batchExec struct {
	agent   *Agent
	cancel  context.CancelFunc
	turn    int
	emitFn  emitFunc
	calls   []ai.ToolCallPart
	denials []*ai.ToolResultPart
	results []ai.ToolResultPart

	// sem bounds concurrent workers; done carries finished worker indexes
	// back to the walker. done is buffered to len(calls) so workers never
	// block sending, even when the walker has bailed out.
	sem      chan struct{}
	done     chan int
	inflight int
	stopped  bool
}

func (b *batchExec) run(ctx context.Context) {
	for idx := range b.calls {
		b.step(ctx, idx)
	}

	b.await(0)
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

	if b.agent.tools.concurrent(call.Name) {
		b.dispatch(ctx, idx)
		return
	}

	// Serial tools barrier on all in-flight work, then run inline.
	b.await(0)

	if b.stopped || ctx.Err() != nil {
		b.settle(idx, errorResult(call, "tool execution cancelled"))
		return
	}

	b.emit(toolStartEvent(b.turn, call))
	b.results[idx] = b.agent.runTool(ctx, call)
	b.emit(toolEndEvent(b.turn, call, b.results[idx]))
}

// settle records a result that did not come from an execution (denial or
// cancellation), emitting the tool's start/end pair.
func (b *batchExec) settle(idx int, result ai.ToolResultPart) {
	b.emit(toolStartEvent(b.turn, b.calls[idx]))
	b.results[idx] = result
	b.emit(toolEndEvent(b.turn, b.calls[idx], result))
}

// dispatch hands a call to a worker, waiting for a semaphore slot while
// forwarding completions so tool_end events stay timely.
func (b *batchExec) dispatch(ctx context.Context, idx int) {
	call := b.calls[idx]

	for {
		select {
		case doneIdx := <-b.done:
			b.finish(doneIdx)
		case b.sem <- struct{}{}:
			b.emit(toolStartEvent(b.turn, call))
			b.inflight++

			go func() {
				defer func() { <-b.sem; b.done <- idx }()

				b.results[idx] = b.agent.runTool(ctx, call)
			}()

			return
		}
	}
}

// await drains completions until at most target workers remain in flight.
func (b *batchExec) await(target int) {
	for b.inflight > target {
		b.finish(<-b.done)
	}
}

// finish books one completed worker and emits its tool_end.
func (b *batchExec) finish(idx int) {
	b.inflight--
	b.emit(toolEndEvent(b.turn, b.calls[idx], b.results[idx]))
}

// emit forwards an event unless the consumer already stopped; when the
// consumer stops here, the run context is cancelled so in-flight tools wind
// down.
func (b *batchExec) emit(ev Event) {
	if b.stopped {
		return
	}

	if !b.emitFn(ev) {
		b.stopped = true
		b.cancel()
	}
}

// runTool executes one call, converting every failure mode — unknown tool,
// cancellation, timeout, execution error, panic — into an error tool result
// the model can react to.
func (a *Agent) runTool(ctx context.Context, call ai.ToolCallPart) ai.ToolResultPart {
	tool, ok := a.tools.byName[call.Name]
	if !ok {
		return errorResult(call, "unknown tool: "+call.Name)
	}

	if ctx.Err() != nil {
		return errorResult(call, "tool execution cancelled")
	}

	if a.cfg.toolTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, a.cfg.toolTimeout)
		defer cancel()
	}

	parts, err := safeExec(ctx, tool, ToolCall{ID: call.ID, Name: call.Name, Args: call.Args})
	if err != nil {
		return errorResult(call, err.Error())
	}

	return ai.ToolResultPart{ToolCallID: call.ID, Name: call.Name, Content: parts}
}

// safeExec runs Exec with panic recovery, so a buggy tool degrades to an
// error result instead of crashing the host.
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
