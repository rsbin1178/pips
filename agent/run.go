package agent

import (
	"context"
	"iter"

	"github.com/rsbin/pips/ai"
)

// Run appends msgs to the session and drives the agent loop to completion:
// call the model, execute requested tools, feed results back, repeat. It
// blocks until the run terminates cleanly (see [StopReason]) or fails; on
// failure the returned result carries what completed before the error.
//
// Run uses [ai.LanguageModel.Generate], so retry middleware on the model is
// fully effective. Use [Agent.Stream] for incremental output.
func (a *Agent) Run(ctx context.Context, sess *Session, msgs ...ai.Message) (*RunResult, error) {
	emit := func(ev Event) bool {
		if a.cfg.onEvent != nil {
			a.cfg.onEvent(ctx, ev)
		}

		return true
	}

	return a.loop(ctx, sess, msgs, emit, false)
}

// Stream is [Agent.Run] as an event sequence: it yields run, turn, and tool
// lifecycle events plus model deltas as they happen. Breaking out of the loop
// cancels the run; the session keeps everything appended up to that point,
// and any tool calls left unanswered surface through [Session.Pending].
//
// A clean termination ends with an [EventRunEnd] carrying the [StopReason]
// (after [StopPaused], resolve [Session.Pending] and run again); failures
// yield a non-nil error as the final element.
func (a *Agent) Stream(ctx context.Context, sess *Session, msgs ...ai.Message) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		stopped := false
		emit := func(ev Event) bool {
			if a.cfg.onEvent != nil {
				a.cfg.onEvent(ctx, ev)
			}

			if !yield(ev, nil) {
				stopped = true
				return false
			}

			return true
		}

		if _, err := a.loop(ctx, sess, msgs, emit, true); err != nil && !stopped {
			yield(Event{}, err)
		}
	}
}

// loop is the shared engine behind Run and Stream. It returns (nil, nil)
// when the stream consumer stops iterating — the run is abandoned and
// nothing further may be emitted.
func (a *Agent) loop(ctx context.Context, sess *Session, msgs []ai.Message, emit emitFunc, streaming bool) (*RunResult, error) {
	if err := sess.begin(); err != nil {
		return nil, err
	}
	defer sess.end()

	// The derived context lets an abandoned stream wind down in-flight work.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess.Append(msgs...)

	if !emit(Event{Type: EventRunStart}) {
		return nil, nil
	}

	r := &run{agent: a, sess: sess, emit: emit, cancel: cancel, streaming: streaming}

	for turn := 1; ; turn++ {
		result, next, err := r.turn(ctx, turn)
		if !next {
			return result, err
		}
	}
}

// run carries one run's mutable state between turns.
type run struct {
	agent     *Agent
	sess      *Session
	emit      emitFunc
	cancel    context.CancelFunc
	streaming bool

	usage    ai.Usage
	lastResp *ai.Response
}

// turn performs one model call plus its tool executions. next reports
// whether the loop should keep going; when false, result and err (both
// possibly nil, for an abandoned stream) are the loop's outcome.
func (r *run) turn(ctx context.Context, turn int) (result *RunResult, next bool, err error) {
	if !r.emit(Event{Type: EventTurnStart, Turn: turn}) {
		return nil, false, nil
	}

	resp, stopped, err := r.agent.callModel(ctx, turn, r.sess.Messages(), r.emit, r.streaming)
	if stopped {
		return nil, false, nil
	}

	if err != nil {
		return &RunResult{Turns: turn - 1, Usage: r.usage, Response: r.lastResp}, false, err
	}

	r.lastResp = resp
	r.usage.Add(resp.Usage)
	r.sess.addUsage(resp.Usage)
	r.sess.Append(resp.Message)

	if !r.emit(messageEvent(turn, resp.Message)) {
		return nil, false, nil
	}

	calls := resp.ToolCalls()
	if len(calls) == 0 {
		if !r.emit(Event{Type: EventTurnEnd, Turn: turn, Usage: r.usage}) {
			return nil, false, nil
		}

		result, err := r.finish(StopEndTurn, turn, nil)

		return result, false, err
	}

	return r.toolPhase(ctx, turn, calls)
}

// toolPhase executes a turn's tool calls and decides how the loop proceeds.
func (r *run) toolPhase(ctx context.Context, turn int, calls []ai.ToolCallPart) (result *RunResult, next bool, err error) {
	outcome := r.agent.execBatch(ctx, r.cancel, turn, calls, r.emit)

	if len(outcome.results) > 0 {
		toolMsg := toolMessage(outcome.results)
		r.sess.Append(toolMsg)

		if !outcome.stopped && !r.emit(messageEvent(turn, toolMsg)) {
			return nil, false, nil
		}
	}

	if outcome.stopped {
		return nil, false, nil
	}

	if !r.emit(Event{Type: EventTurnEnd, Turn: turn, Usage: r.usage}) {
		return nil, false, nil
	}

	if outcome.pending != nil {
		result, err := r.finish(StopPaused, turn, outcome.pending)
		return result, false, err
	}

	if err := ctx.Err(); err != nil {
		return &RunResult{Turns: turn, Usage: r.usage, Response: r.lastResp}, false, err
	}

	if reason, stop := r.agent.shouldStop(turn, r.usage, r.lastResp); stop {
		result, err := r.finish(reason, turn, nil)
		return result, false, err
	}

	return nil, true, nil
}

// finish emits run_end and assembles the result of a clean termination.
func (r *run) finish(stop StopReason, turns int, pending []ai.ToolCallPart) (*RunResult, error) {
	r.emit(Event{Type: EventRunEnd, Turn: turns, Stop: stop, Usage: r.usage})

	return &RunResult{
		Stop:     stop,
		Turns:    turns,
		Usage:    r.usage,
		Response: r.lastResp,
		Pending:  pending,
	}, nil
}

// callModel performs one model call. When streaming, deltas tee through emit
// while [ai.Collect] folds them into the completed response; stopped reports
// that the consumer quit mid-stream.
func (a *Agent) callModel(ctx context.Context, turn int, msgs []ai.Message, emit emitFunc, streaming bool) (resp *ai.Response, stopped bool, err error) {
	req := a.request(msgs)

	if !streaming {
		resp, err = a.model.Generate(ctx, req)
		return resp, false, err
	}

	resp, err = ai.Collect(func(yield func(ai.StreamEvent, error) bool) {
		for ev, streamErr := range a.model.Stream(ctx, req) {
			if streamErr != nil {
				yield(ai.StreamEvent{}, streamErr)
				return
			}

			if !emit(Event{Type: EventDelta, Turn: turn, Delta: ev}) {
				stopped = true
				return
			}

			if !yield(ev, nil) {
				return
			}
		}
	})

	if stopped {
		return nil, true, nil
	}

	return resp, false, err
}

// shouldStop checks the configured stop conditions after a completed turn.
func (a *Agent) shouldStop(turn int, usage ai.Usage, resp *ai.Response) (StopReason, bool) {
	switch {
	case a.cfg.maxTurns > 0 && turn >= a.cfg.maxTurns:
		return StopMaxTurns, true
	case a.cfg.maxTokens > 0 && usage.InputTokens+usage.OutputTokens >= a.cfg.maxTokens:
		return StopBudget, true
	case a.cfg.stopWhen != nil && a.cfg.stopWhen(RunInfo{Turns: turn, Usage: usage, Response: resp}):
		return StopWhen, true
	default:
		return "", false
	}
}

// toolMessage packs one turn's results into the tool message appended to the
// session.
func toolMessage(results []ai.ToolResultPart) ai.Message {
	parts := make([]ai.Part, 0, len(results))
	for _, r := range results {
		parts = append(parts, r)
	}

	return ai.Message{Role: ai.RoleTool, Parts: parts}
}
