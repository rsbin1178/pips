package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/rsbin/pips/ai"
)

// Run appends msgs to the session and drives the agent loop to completion:
// call the model, execute requested tools, feed results back, repeat. It
// blocks until the run terminates cleanly (see [StopReason]) or fails; on
// failure the returned result carries what completed before the error.
//
// Mid-run, [Session.Steer] injects messages before the next model call and
// [Session.FollowUp] queues work for after the model would otherwise finish.
//
// Run uses [ai.LanguageModel.Generate], so retry middleware on the model is
// fully effective. Use [Agent.Stream] for incremental output.
func (a *Agent) Run(ctx context.Context, sess *Session, msgs ...ai.Message) (*RunResult, error) {
	return a.loop(ctx, sess, msgs, func(Event) bool { return true }, false)
}

// Stream is [Agent.Run] as an event sequence: it yields run, turn, and tool
// lifecycle events plus model deltas as they happen. Breaking out of the loop
// cancels the run; the session keeps everything appended up to that point,
// and any tool calls left unanswered surface through [Session.Pending].
//
// A clean termination ends with an [EventRunCompleted] carrying the
// [RunCompleted] payload and [StopReason]
// (after [StopPaused], resolve [Session.Pending] and run again); failures
// yield a non-nil error as the final element.
func (a *Agent) Stream(ctx context.Context, sess *Session, msgs ...ai.Message) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		stopped := false
		emit := func(ev Event) bool {
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
func (a *Agent) loop(ctx context.Context, sess *Session, msgs []ai.Message, deliver func(Event) bool, streaming bool) (*RunResult, error) {
	if err := sess.begin(); err != nil {
		return nil, err
	}
	defer sess.end()

	// The derived context lets an abandoned stream wind down in-flight work.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	meta := newRunMetadata(ctx, a.cfg.name)
	ctx = withRunMetadata(ctx, meta)

	emit := func(payload EventPayload) bool {
		ev := newEvent(meta, time.Now(), payload)
		if a.cfg.onEvent != nil {
			a.cfg.onEvent(ctx, cloneEvent(ev))
		}

		return deliver(ev)
	}

	if !emit(RunStarted{}) {
		return nil, nil
	}

	r := &run{
		agent: a, sess: sess, emit: emit, cancel: cancel, streaming: streaming,
		model: a.model, tools: a.tools, meta: meta,
	}
	if err := checkInputGuardrails(ctx, a.cfg.inputGuards, InputGuardrailInfo{
		RunMetadata: meta,
		Session:     sess.Messages(),
		Input:       msgs,
	}); err != nil {
		return r.partial(0), err
	}

	sess.Append(msgs...)

	// Initial steering poll: pick up messages queued while no run was active.
	r.pending = sess.drainSteering(a.cfg.steeringMode)

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
	meta      RunMetadata

	// model serves the run's calls; WithPrepareTurn may swap it mid-run.
	model ai.LanguageModel
	// tools is the run-scoped immutable tool snapshot. Prepare-turn updates
	// replace it only between turns, preserving declaration/execution parity.
	tools *toolbox
	// pending holds drained queue messages awaiting injection at the next
	// turn start.
	pending []ai.Message
	// nextRequest is a one-shot request constraint consumed at the next turn.
	nextRequest *runModelRequest

	usage    ai.Usage
	lastResp *ai.Response
}

type runModelRequest struct {
	tools        *toolbox
	toolChoice   ai.ToolChoice
	systemSuffix string
}

// partial assembles the result accompanying a run error.
func (r *run) partial(turns int) *RunResult {
	return &RunResult{
		RunMetadata: r.meta,
		Turns:       turns,
		Usage:       r.usage,
		Response:    r.lastResp,
	}
}

// turn performs one model call plus its tool executions. next reports
// whether the loop should keep going; when false, result and err (both
// possibly nil, for an abandoned stream) are the loop's outcome.
func (r *run) turn(ctx context.Context, turn int) (result *RunResult, next bool, err error) {
	if !r.emit(TurnStarted{Turn: turn}) {
		return nil, false, nil
	}

	if !r.inject(turn) {
		return nil, false, nil
	}

	if err := ctx.Err(); err != nil {
		return r.partial(turn - 1), false, err
	}

	msgs, err := r.modelMessages(ctx)
	if err != nil {
		return r.partial(turn - 1), false, err
	}

	requestUpdate := r.nextRequest
	r.nextRequest = nil

	turnTools := r.tools
	if requestUpdate != nil && requestUpdate.tools != nil {
		turnTools = requestUpdate.tools
	}

	previousResponse := r.lastResp

	resp, stopped, err := r.agent.callModel(
		ctx, r.model, turnTools, requestUpdate, turn, msgs, r.emit, r.streaming,
	)
	if stopped {
		return nil, false, nil
	}

	if err != nil {
		return r.partial(turn - 1), false, err
	}

	r.lastResp = resp
	r.usage.Add(resp.Usage)
	r.sess.addUsage(resp.Usage)

	if len(resp.ToolCalls()) == 0 {
		update, candidateErr := r.evaluateCandidate(ctx, turn, resp)
		if candidateErr != nil {
			return r.partial(turn), false, candidateErr
		}

		if update != nil {
			r.lastResp = previousResponse

			return r.retryCandidate(ctx, turn, resp, update)
		}
	}

	r.sess.Append(resp.Message)

	if !r.emit(MessageCommitted{Turn: turn, Message: resp.Message}) {
		return nil, false, nil
	}

	return r.toolPhase(ctx, turn, resp, turnTools)
}

func (r *run) modelMessages(ctx context.Context) ([]ai.Message, error) {
	messages := r.sess.Messages()
	if r.agent.cfg.transform == nil {
		return messages, nil
	}

	return r.agent.cfg.transform(ctx, messages)
}

func (r *run) evaluateCandidate(
	ctx context.Context,
	turn int,
	resp *ai.Response,
) (*ModelRequestUpdate, error) {
	info := OutputGuardrailInfo{
		RunInfo: RunInfo{
			RunMetadata: r.meta,
			Turns:       turn,
			Usage:       r.usage,
			Response:    resp,
		},
		Message: resp.Message,
	}
	if err := checkOutputGuardrails(ctx, r.agent.cfg.outputGuards, info); err != nil {
		return nil, err
	}

	candidate := r.agent.cfg.candidate
	if candidate == nil {
		return nil, nil
	}

	decision := candidate(ctx, CandidateAnswerInfo{
		RunInfo: info.RunInfo,
		Session: r.sess.Messages(),
		Message: resp.Message,
	})
	if decision.Err != nil {
		return nil, decision.Err
	}

	return decision.Retry, nil
}

func (r *run) retryCandidate(
	ctx context.Context,
	turn int,
	resp *ai.Response,
	update *ModelRequestUpdate,
) (result *RunResult, next bool, err error) {
	if !r.emit(CandidateDiscarded{Turn: turn}) ||
		!r.emit(TurnCompleted{Turn: turn, Usage: r.usage}) {
		return nil, false, nil
	}

	if err := r.prepare(ctx, turn, resp); err != nil {
		return r.partial(turn), false, err
	}

	if err := r.setNextRequest(update); err != nil {
		return r.partial(turn), false, err
	}

	if err := ctx.Err(); err != nil {
		return r.partial(turn), false, err
	}

	if reason, stop := r.agent.shouldStop(RunInfo{
		RunMetadata: r.meta,
		Turns:       turn,
		Usage:       r.usage,
		Response:    resp,
	}); stop {
		result, err := r.finish(reason, turn, nil)

		return result, false, err
	}

	r.pending = r.sess.drainSteering(r.agent.cfg.steeringMode)

	return nil, true, nil
}

// inject appends and announces messages drained from the queues; false means
// the stream consumer stopped.
func (r *run) inject(turn int) bool {
	for _, msg := range r.pending {
		r.sess.Append(msg)

		if !r.emit(MessageCommitted{Turn: turn, Message: msg}) {
			return false
		}
	}

	r.pending = nil

	return true
}

// toolPhase executes a turn's tool calls (if any) and decides how the loop
// proceeds.
func (r *run) toolPhase(
	ctx context.Context,
	turn int,
	resp *ai.Response,
	tools *toolbox,
) (result *RunResult, next bool, err error) {
	calls := resp.ToolCalls()

	var outcome batchOutcome

	switch {
	case len(calls) == 0:
	case resp.FinishReason == ai.FinishLength:
		// The response was cut off by the output-token limit, so every call
		// may carry silently truncated arguments; fail the whole batch and
		// let the model re-issue the calls (none are safe to execute).
		outcome = r.truncatedBatch(turn, calls)
	default:
		outcome = r.agent.execBatch(ctx, tools, r.cancel, turn, calls, r.emit)
	}

	if len(outcome.results) > 0 {
		toolMsg := toolMessage(outcome.results)
		r.sess.Append(toolMsg)

		if !outcome.stopped && !r.emit(MessageCommitted{Turn: turn, Message: toolMsg}) {
			return nil, false, nil
		}
	}

	if outcome.stopped {
		return nil, false, nil
	}

	if !r.emit(TurnCompleted{Turn: turn, Usage: r.usage}) {
		return nil, false, nil
	}

	if outcome.pending != nil {
		result, err := r.finish(StopPaused, turn, outcome.pending)
		return result, false, err
	}

	natural, naturalStop := len(calls) == 0, StopEndTurn
	if outcome.terminated {
		natural, naturalStop = true, StopTerminated
	}

	return r.decide(ctx, turn, natural, naturalStop)
}

// truncatedBatch synthesizes error results for a truncated turn's calls
// without executing anything.
func (r *run) truncatedBatch(turn int, calls []ai.ToolCallPart) batchOutcome {
	const reason = "not executed: the response hit the output token limit, so " +
		"the arguments may be truncated; re-issue the tool call with complete arguments"

	results := make([]ai.ToolResultPart, len(calls))

	for idx, call := range calls {
		results[idx] = errorResult(call, reason)

		if !r.emit(ToolStarted{Turn: turn, Call: call}) ||
			!r.emit(ToolCompleted{Turn: turn, Call: call, Result: results[idx]}) {
			return batchOutcome{results: results[:idx+1], stopped: true}
		}
	}

	return batchOutcome{results: results}
}

// decide runs the post-turn chain: the prepare-turn hook, then termination
// against the queues. Natural stops (no tool calls, or a terminate hint) win
// when nothing is queued; otherwise the stop conditions guard continuation,
// and steering — then, on a natural stop, follow-ups — feed the next turn.
func (r *run) decide(ctx context.Context, turn int, natural bool, naturalStop StopReason) (result *RunResult, next bool, err error) {
	if err := r.prepare(ctx, turn, r.lastResp); err != nil {
		return r.partial(turn), false, err
	}

	if err := ctx.Err(); err != nil {
		return r.partial(turn), false, err
	}

	if natural && !r.sess.HasQueued() {
		result, err := r.finish(naturalStop, turn, nil)
		return result, false, err
	}

	if reason, stop := r.agent.shouldStop(RunInfo{
		RunMetadata: r.meta,
		Turns:       turn,
		Usage:       r.usage,
		Response:    r.lastResp,
	}); stop {
		result, err := r.finish(reason, turn, nil)
		return result, false, err
	}

	r.pending = r.sess.drainSteering(r.agent.cfg.steeringMode)

	if natural && len(r.pending) == 0 {
		r.pending = r.sess.drainFollowUps(r.agent.cfg.followUpMode)
		if len(r.pending) == 0 {
			// The queues emptied between HasQueued and the drains.
			result, err := r.finish(naturalStop, turn, nil)
			return result, false, err
		}
	}

	return nil, true, nil
}

func (r *run) prepare(ctx context.Context, turn int, response *ai.Response) error {
	if fn := r.agent.cfg.prepareTurn; fn != nil {
		return r.applyTurnUpdate(fn(ctx, RunInfo{
			RunMetadata: r.meta,
			Turns:       turn,
			Usage:       r.usage,
			Response:    response,
		}))
	}

	return nil
}

func (r *run) applyTurnUpdate(update TurnUpdate) error {
	if update.Err != nil {
		return update.Err
	}

	if update.Model != nil {
		r.model = update.Model
	}

	if update.ReplaceMessages != nil {
		r.sess.Replace(update.ReplaceMessages...)
	}

	if update.Tools != nil {
		tools, err := newToolbox(update.Tools)
		if err != nil {
			return err
		}

		r.tools = tools
	}

	if update.NextRequest != nil {
		return r.setNextRequest(update.NextRequest)
	}

	return nil
}

func (r *run) setNextRequest(update *ModelRequestUpdate) error {
	if update == nil {
		r.nextRequest = nil

		return nil
	}

	selected := r.tools

	request := &runModelRequest{
		toolChoice:   update.ToolChoice,
		systemSuffix: update.SystemSuffix,
	}
	if update.Tools != nil {
		tools, err := newToolbox(update.Tools)
		if err != nil {
			return fmt.Errorf("agent: next model request: %w", err)
		}

		selected = tools
		request.tools = tools
	}

	if err := validateToolChoice(update.ToolChoice, selected); err != nil {
		return fmt.Errorf("agent: next model request: %w", err)
	}

	r.nextRequest = request

	return nil
}

func validateToolChoice(choice ai.ToolChoice, tools *toolbox) error {
	switch choice.Mode {
	case "", ai.ToolChoiceAuto, ai.ToolChoiceNone:
		if choice.Name != "" {
			return errors.New("tool choice name requires exact-tool mode")
		}
	case ai.ToolChoiceRequired:
		if choice.Name != "" {
			return errors.New("required tool choice must not name a tool")
		}

		if tools == nil || len(tools.decls) == 0 {
			return errors.New("required tool choice has no available tools")
		}
	case ai.ToolChoiceTool:
		if choice.Name == "" {
			return errors.New("exact tool choice has no name")
		}

		if tools == nil || tools.byName[choice.Name] == nil {
			return fmt.Errorf("exact tool choice %q is unavailable", choice.Name)
		}
	default:
		return fmt.Errorf("unknown tool choice mode %q", choice.Mode)
	}

	return nil
}

// finish emits run_completed and assembles the result of a clean termination.
func (r *run) finish(stop StopReason, turns int, pending []ai.ToolCallPart) (*RunResult, error) {
	r.emit(RunCompleted{Turns: turns, Stop: stop, Usage: r.usage})

	return &RunResult{
		RunMetadata: r.meta,
		Stop:        stop,
		Turns:       turns,
		Usage:       r.usage,
		Response:    r.lastResp,
		Pending:     pending,
	}, nil
}

// callModel performs one model call. When streaming, deltas tee through emit
// while [ai.Collect] folds them into the completed response; stopped reports
// that the consumer quit mid-stream.
func (a *Agent) callModel(
	ctx context.Context,
	model ai.LanguageModel,
	tools *toolbox,
	update *runModelRequest,
	turn int,
	msgs []ai.Message,
	emit emitFunc,
	streaming bool,
) (resp *ai.Response, stopped bool, err error) {
	req := a.requestWithTools(msgs, tools, update)

	if !streaming {
		resp, err = model.Generate(ctx, req)
		return resp, false, err
	}

	resp, err = ai.Collect(func(yield func(ai.StreamEvent, error) bool) {
		for ev, streamErr := range model.Stream(ctx, req) {
			if streamErr != nil {
				yield(ai.StreamEvent{}, streamErr)
				return
			}

			if !validModelStreamEventType(ev.Type) {
				yield(ai.StreamEvent{}, invalidEvent("model stream event type %q is invalid", ev.Type))
				return
			}

			if !emit(ModelStreamEvent{Turn: turn, Event: ev}) {
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
// It guards continuation only: a natural stop with empty queues wins first.
func (a *Agent) shouldStop(info RunInfo) (StopReason, bool) {
	switch {
	case a.cfg.maxTurns > 0 && info.Turns >= a.cfg.maxTurns:
		return StopMaxTurns, true
	case a.cfg.maxTokens > 0 && info.Usage.InputTokens+info.Usage.OutputTokens >= a.cfg.maxTokens:
		return StopBudget, true
	case a.cfg.stopWhen != nil && a.cfg.stopWhen(info):
		return StopWhen, true
	default:
		return "", false
	}
}

// toolMessage packs one turn's results into the tool message appended to the
// session.
func toolMessage(results []ai.ToolResultPart) ai.ToolMessage {
	return ai.ToolResults(results...)
}
