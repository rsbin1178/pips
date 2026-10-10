package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
)

// lengthSalvageAttempts bounds how many times one turn continues a response
// the output-token limit cut off. Two continues match Grok Build's default.
const lengthSalvageAttempts = 2

// lengthSalvageInstruction asks the model to resume a response the
// output-token limit cut off. It is provider-neutral: adapters map the leading
// system block to their native instruction surface.
const lengthSalvageInstruction = "Your previous response exceeded the output token limit and was cut off. " +
	"Continue from exactly where it stopped. Do not repeat what you already wrote."

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
	// salvageContinues counts the length-salvage continues this run has
	// consumed, bounding how many times one truncated answer is resumed.
	salvageContinues int
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

	resp, stopped, err := r.callTurn(ctx, turn, turnTools, requestUpdate, msgs)
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

	resp.Message = normalizeModelToolArguments(resp.Message)
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

	// A response the output-token limit cut off is real output, so the loop
	// keeps it and asks for the remainder instead of ending the turn. Once the
	// budget is spent the same response reports StopTruncated, never a
	// successful end_turn.
	salvage := r.salvageable()
	if !salvage && naturalStop == StopEndTurn && r.lengthTruncated() {
		naturalStop = StopTruncated
	}

	if natural && !r.sess.HasQueued() && !salvage {
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

	if salvage {
		r.salvageContinues++
		r.addNextSystemSuffix(lengthSalvageInstruction)
	}

	r.pending = r.sess.drainSteering(r.agent.cfg.steeringMode)

	if natural && len(r.pending) == 0 {
		r.pending = r.sess.drainFollowUps(r.agent.cfg.followUpMode)
		if len(r.pending) == 0 && !salvage {
			// The queues emptied between HasQueued and the drains.
			result, err := r.finish(naturalStop, turn, nil)
			return result, false, err
		}
	}

	return nil, true, nil
}

// lengthTruncated reports that the last response is a no-tool answer the
// output-token limit cut off with text worth continuing.
func (r *run) lengthTruncated() bool {
	resp := r.lastResp

	return resp != nil &&
		resp.FinishReason == ai.FinishLength &&
		len(resp.ToolCalls()) == 0 &&
		strings.TrimSpace(resp.Text()) != ""
}

// salvageable reports whether the last response should be continued and the
// per-run salvage budget still allows it.
func (r *run) salvageable() bool {
	return r.lengthTruncated() && r.salvageContinues < lengthSalvageAttempts
}

// addNextSystemSuffix appends a one-request system instruction to the next
// request, preserving any constraint a prepare-turn hook already set.
func (r *run) addNextSystemSuffix(suffix string) {
	if r.nextRequest == nil {
		r.nextRequest = &runModelRequest{}
	}

	if r.nextRequest.systemSuffix == "" {
		r.nextRequest.systemSuffix = suffix

		return
	}

	r.nextRequest.systemSuffix += "\n" + suffix
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

		var tool Tool
		if tools != nil {
			tool = tools.byName[choice.Name]
		}

		if tool == nil {
			return fmt.Errorf("exact tool choice %q is unavailable", choice.Name)
		}

		// A Disabled tool stays executable by name but is never declared to
		// the model, so forcing it would name a tool the provider cannot see.
		if !tool.Decl().IsEnabled() {
			return fmt.Errorf("exact tool choice %q is not declared to the model", choice.Name)
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

// callTurn performs one turn's model call, re-issuing it when a stream that had
// already produced output failed with a retryable error. In the default
// discard mode every re-issue first retracts the provisional candidate so a
// frontend drops the partial output. In continuation mode the partial text is
// kept and sent back as a trailing assistant prefix, so the re-issue asks only
// for the missing remainder. Nothing from the failed attempt reached the
// session and no tool ran, so a re-issue can neither duplicate content nor
// repeat an effect.
//
// A failed attempt that produced only droppable output — reasoning, with no
// answer text and no tool call — is equivalent to a request that produced
// nothing, so it is charged to the replay allowance: what the model middleware
// would have given it. The two allowances are counted apart, so neither failure
// shape spends the other's; a frontend that sets no replay allowance keeps one
// tier, and both shapes share the re-issue budget.
func (r *run) callTurn(
	ctx context.Context,
	turn int,
	turnTools *toolbox,
	requestUpdate *runModelRequest,
	msgs []ai.Message,
) (resp *ai.Response, stopped bool, err error) {
	recovery := r.agent.cfg.streamRecovery
	window := r.agent.cfg.streamRecoveryWindow
	replayAttempts := r.agent.cfg.streamReplayAttempts

	// prefix accumulates the text retained across failed attempts; a non-empty
	// prefix switches every later attempt to the continuation request.
	// fellBack records that the prefix was abandoned because the provider would
	// not serve the continuation shape or the continuation added nothing: the
	// turn then answers from the start rather than dying with a fragment.
	var (
		prefix     string
		havePrefix bool
		fellBack   bool
		// episodeStart marks when the first re-issue was scheduled, which is what
		// the recovery window is measured from: a slow first attempt that
		// succeeds must not consume it.
		episodeStart time.Time
		// spent and replayed count the re-issues already scheduled per failure
		// shape: one that retracted answer content, and one that dropped
		// reasoning only. Without a replay allowance the two shapes share spent,
		// which keeps a single tier.
		spent    int
		replayed int
	)

	for attempt := 0; ; attempt++ {
		attemptMsgs, attemptUpdate := msgs, requestUpdate

		var filter *continuationFilter

		switch {
		case havePrefix:
			// Build a fresh slice: msgs shares its backing array with the
			// session snapshot, so appending to it would corrupt the session.
			attemptMsgs = slices.Concat(msgs, []ai.Message{ai.Assistant(ai.TextPart{Text: prefix})})
			attemptUpdate = continuationRequestUpdate(requestUpdate, streamContinuationInstruction)
			filter = newContinuationFilter(prefix)
		case fellBack:
			attemptUpdate = continuationRequestUpdate(requestUpdate, streamRestartInstruction)
		}

		var produced, retractable bool

		resp, produced, retractable, stopped, err = r.agent.callModel(
			ctx, r.model, turnTools, attemptUpdate, turn, attemptMsgs, r.emit, r.streaming, filter,
		)
		if stopped {
			return nil, true, nil
		}

		// abandonPrefix marks the two escape arms: the continuation cannot be
		// honoured, so the turn drops the fragment and answers from the start.
		abandonPrefix := false

		windowSpent := !episodeStart.IsZero() && window > 0 && time.Since(episodeStart) >= window

		// budget is the allowance this failure draws on and used is how much of
		// it the turn has already spent. Answer text and tool calls draw the
		// re-issue budget, because every re-issue retracts content a consumer
		// read. Reasoning alone draws the replay allowance when recovery is
		// enabled and the frontend set one, and shares the re-issue budget
		// otherwise: a budget configures recovery, it does not enable it.
		budget, used := recovery.attempts, spent
		replayTier := recovery.attempts > 0 && replayAttempts > 0 && !retractable

		if replayTier {
			budget, used = replayAttempts, replayed
		}

		switch {
		case err == nil && havePrefix && !fellBack && continuationEmpty(resp):
			// The provider accepted the continuation and added nothing.
			// Committing the prefix alone would present a fragment as the whole
			// reply, so answer from the start instead.
			abandonPrefix = true
		case err == nil:
			if havePrefix {
				resp = mergeContinuation(prefix, resp)
			}

			return resp, false, nil
		case havePrefix && !fellBack && !ai.IsRetryable(err) && ctx.Err() == nil:
			// The provider refused the continuation shape. Dropping the prefix
			// costs the work already done, but the turn's own request still
			// works and a dead turn costs more.
			abandonPrefix = true
		case !produced || used >= budget || !ai.IsRetryable(err) || ctx.Err() != nil ||
			windowSpent:
			// Give up, reporting whatever the failing attempt produced.
			// Continuation attempts extend one answer, so their fragments
			// accumulate. Regenerate attempts are independent alternatives, so
			// only the fragment the consumer was actually looking at — the last
			// one — is reported. prefix is read by the report below; havePrefix
			// is not, because this arm returns — the turn is over either way.
			if retained, ok := retainPrefix(resp); ok {
				if recovery.mode == recoveryContinue && !fellBack {
					prefix += retained
				} else {
					prefix = retained
				}
			}

			if !r.reportAbandoned(turn, prefix, produced, err) {
				return nil, true, nil
			}

			return resp, false, err
		default:
			if episodeStart.IsZero() {
				episodeStart = time.Now()
			}

			if retained, ok := retainPrefix(resp); ok {
				if recovery.mode == recoveryContinue && !fellBack {
					// A continuation extends one answer, so its fragment
					// accumulates. A continuation that produced no usable text
					// keeps the existing prefix and still consumes an attempt.
					prefix += retained
					havePrefix = true
				} else {
					// A regenerating turn replaces the fragment: its attempts
					// are alternatives, and the last one is what the consumer
					// was looking at when the turn gave up.
					prefix = retained
				}
			}

			if replayTier {
				replayed++
			} else {
				spent++
			}

			delay := recovery.backoff(attempt)

			switch {
			case havePrefix:
				if !r.emit(retryNotice(turn, used, budget, delay, err)) {
					return nil, true, nil
				}
			case !r.emit(CandidateDiscarded{Turn: turn}),
				!r.emit(retryNotice(turn, used, budget, delay, err)):
				return nil, true, nil
			}

			if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
				return nil, false, sleepErr
			}

			continue
		}

		if abandonPrefix {
			fellBack = true
			havePrefix = false
			prefix = ""

			// The consumer rendered the fragment, so it has to be told the
			// draft is no longer being honoured before the restart streams.
			if !r.emit(CandidateDiscarded{Turn: turn}) {
				return nil, true, nil
			}

			if sleepErr := sleepContext(ctx, recovery.backoff(attempt)); sleepErr != nil {
				return nil, false, sleepErr
			}
		}
	}
}

// continuationRequestUpdate returns the request constraint for a continuation
// attempt: the turn's original constraint plus the continuation instruction as
// a one-request system suffix. Tools and tool choice are preserved so the
// continuation cannot alter the tool snapshot.
func continuationRequestUpdate(base *runModelRequest, instruction string) *runModelRequest {
	update := &runModelRequest{}
	if base != nil {
		*update = *base
	}

	if update.systemSuffix == "" {
		update.systemSuffix = instruction

		return update
	}

	update.systemSuffix += "\n" + instruction

	return update
}

// reportAbandoned reports what a turn that is giving up leaves behind. Retained
// answer text becomes an explicitly incomplete reply, so a fragment the consumer
// saw never vanishes without a trace. When nothing was retained but the attempt
// did stream output, that output was reasoning: it is not an answer fragment to
// report, but the consumer rendered it, and a turn that gives up must not leave a
// live draft behind. It returns false when the consumer stopped.
func (r *run) reportAbandoned(turn int, prefix string, produced bool, err error) bool {
	switch {
	case prefix != "":
		return r.emitIncomplete(turn, prefix, err)
	case produced:
		return r.emit(CandidateDiscarded{Turn: turn})
	default:
		return true
	}
}

// emitIncomplete reports a retained partial answer that will not be committed.
// It returns false when the stream consumer stopped.
func (r *run) emitIncomplete(turn int, prefix string, err error) bool {
	return r.emit(CandidateIncomplete{
		Turn:   turn,
		Text:   boundIncompleteText(prefix),
		Bytes:  len(prefix),
		Reason: incompleteReason(err),
	})
}

// incompleteReason names the failure class shown next to an incomplete reply.
// It reuses the retry notice's classification, so a consumer reads the same
// phrase whether the failure was retried or abandoned.
func incompleteReason(err error) string {
	return ai.NewRetryNotice(1, 1, 0, err).Reason
}

// retryNotice announces the re-issue that is about to start. The loop reports
// the wait on the same stream channel the retry middleware uses, so a frontend
// renders one kind of notice whichever layer replays the request. spent is how
// many re-issues the failure shape has already used, so this one's ordinal is
// spent+1 against its budget.
func retryNotice(turn, spent, budget int, delay time.Duration, err error) ModelStreamEvent {
	return ModelStreamEvent{
		Turn: turn,
		Event: ai.StreamEvent{
			Type:  ai.StreamRetry,
			Retry: ai.NewRetryNotice(spent+1, budget, delay, err),
		},
	}
}

// sleepContext waits for delay or the context, whichever comes first.
func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// callModel performs one model call. When streaming, deltas tee through emit
// while [ai.Collect] folds them into the completed response; produced reports
// that the call streamed output before it ended, retractable reports that the
// output included content a consumer would have to retract — answer text or a
// tool call — and stopped reports that the consumer quit mid-stream. filter,
// when non-nil, trims the restatement a continuation may open with before
// either the draft or the accumulator sees it.
func (a *Agent) callModel(
	ctx context.Context,
	model ai.LanguageModel,
	tools *toolbox,
	update *runModelRequest,
	turn int,
	msgs []ai.Message,
	emit emitFunc,
	streaming bool,
	filter *continuationFilter,
) (resp *ai.Response, produced, retractable, stopped bool, err error) {
	req := a.requestWithTools(msgs, tools, update)

	if !streaming {
		resp, err = model.Generate(ctx, req)
		return resp, false, false, false, err
	}

	resp, err = ai.Collect(func(yield func(ai.StreamEvent, error) bool) {
		for ev, streamErr := range model.Stream(ctx, req) {
			if streamErr != nil {
				// A break also ends the stream: release buffered text before
				// the error so a continuation's output is retained, not lost.
				for _, forwarded := range filter.flush() {
					if !emit(ModelStreamEvent{Turn: turn, Event: forwarded}) {
						stopped = true
						return
					}

					if !yield(forwarded, nil) {
						return
					}
				}

				yield(ai.StreamEvent{}, streamErr)
				return
			}

			if !validModelStreamEventType(ev.Type) {
				yield(ai.StreamEvent{}, invalidEvent("model stream event type %q is invalid", ev.Type))
				return
			}

			// A retry notice reports a wait, so it is not output. The raw
			// event decides this: a buffered delta the filter has not released
			// yet still means the call produced output.
			if ev.Type != ai.StreamMessageStart && ev.Type != ai.StreamRetry {
				produced = true

				// Reasoning is the one output a re-issue can drop and redo
				// without costing the consumer an answer, so it does not make
				// the attempt retractable. Any other kind does, including one
				// this layer does not model yet.
				if ev.Type != ai.StreamReasoningDelta {
					retractable = true
				}
			}

			for _, forwarded := range filter.forward(ev) {
				if !emit(ModelStreamEvent{Turn: turn, Event: forwarded}) {
					stopped = true
					return
				}

				if !yield(forwarded, nil) {
					return
				}
			}
		}
	})

	if stopped {
		return nil, produced, retractable, true, nil
	}

	return resp, produced, retractable, false, err
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

// streamContinuationInstruction asks the model to resume a stream that broke
// mid-answer. It is provider-neutral, delivered as a one-request system
// suffix on the continuation request.
const streamContinuationInstruction = "The previous response was interrupted mid-stream. " +
	"Continue it: output only the missing remainder, starting exactly where the text above stops. " +
	"Do not repeat, rephrase, summarise, or restart anything already written."

// streamRestartInstruction accompanies the request that answers from the start
// after a continuation was abandoned. The model is told why the turn is being
// re-issued, so it neither assumes the fragment it wrote still stands nor reads
// the re-issue as a new user request.
const streamRestartInstruction = "Your previous response was interrupted mid-stream and nothing of it was kept. " +
	"Write the complete answer again from the start — no apology, no recap of the interruption."

// continuationEmpty reports that a continuation was accepted but contributed
// no answer text. Committing the retained prefix alone in that case would
// present a fragment as the whole reply.
func continuationEmpty(resp *ai.Response) bool {
	if resp == nil || len(resp.ToolCalls()) > 0 {
		return false
	}

	_, hasText := retainPrefix(resp)

	return !hasText
}

// Continuation overlap trim. A provider may restate the tail of the prefix
// when asked to continue; the filter drops that repeat so the live draft and
// the committed message agree.
const (
	// continuationOverlapBytes caps how much of the prefix tail is compared
	// against the continuation's opening.
	continuationOverlapBytes = 256
	// continuationMinOverlapBytes is the shortest match treated as a real
	// restatement; anything shorter is coincidence.
	continuationMinOverlapBytes = 16
	// maxIncompleteTextBytes bounds the text a CandidateIncomplete event
	// retains, so a broken stream cannot push an unbounded draft into a
	// consumer's durable trace.
	maxIncompleteTextBytes = 64 << 10
	// maxIncompleteReasonBytes bounds the failure phrase beside that text.
	maxIncompleteReasonBytes = 64
)

// retainPrefix returns the text a failed attempt produced, which a
// continuation re-issues as a trailing assistant message. It refuses a
// response carrying tool calls — those cannot be replayed as text — and one
// whose text is empty after trimming.
func retainPrefix(resp *ai.Response) (string, bool) {
	if resp == nil {
		return "", false
	}

	var text strings.Builder

	for _, part := range resp.Message.Parts {
		switch part := part.(type) {
		case ai.ToolCallPart:
			return "", false
		case ai.TextPart:
			text.WriteString(part.Text)
		}
	}

	retained := text.String()
	if strings.TrimSpace(retained) == "" {
		return "", false
	}

	return retained, true
}

// mergeContinuation prepends the retained prefix to the final attempt's
// response and folds adjacent text parts, so the committed message reads as one
// continuous answer. Tool calls, reasoning, citations, finish reason, and usage
// come from the final attempt.
func mergeContinuation(prefix string, resp *ai.Response) *ai.Response {
	if resp == nil {
		return nil
	}

	parts := make([]ai.AssistantPart, 0, len(resp.Message.Parts)+1)
	parts = append(parts, ai.TextPart{Text: prefix})

	for _, part := range resp.Message.Parts {
		if text, ok := part.(ai.TextPart); ok && len(parts) > 0 {
			if last, ok := parts[len(parts)-1].(ai.TextPart); ok {
				parts[len(parts)-1] = ai.TextPart{Text: last.Text + text.Text}

				continue
			}
		}

		parts = append(parts, part)
	}

	resp.Message.Parts = parts

	return resp
}

// boundIncompleteText truncates retained text to maxIncompleteTextBytes
// without splitting a UTF-8 rune, so a bounded event still encodes cleanly.
func boundIncompleteText(text string) string {
	if len(text) <= maxIncompleteTextBytes {
		return text
	}

	truncated := text[:maxIncompleteTextBytes]
	for len(truncated) > 0 {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError || size > 1 {
			break
		}

		truncated = truncated[:len(truncated)-1]
	}

	return truncated
}

// continuationFilter trims the restatement a continuation may open with. It
// sits between the model stream and both the live emit and the accumulator, so
// neither the rendered draft nor the committed message contains the repeat.
type continuationFilter struct {
	prefix  []byte
	window  int
	buffer  []byte
	trimmed bool
}

func newContinuationFilter(prefix string) *continuationFilter {
	return &continuationFilter{
		prefix: []byte(prefix),
		window: min(len(prefix), continuationOverlapBytes),
	}
}

// forward maps one model event to the events to pass downstream, buffering
// text until enough is held to detect an overlap. Non-text events pass through
// untouched and do not release the buffer, so a continuation that opens with
// reasoning still gets its text trimmed.
func (f *continuationFilter) forward(ev ai.StreamEvent) []ai.StreamEvent {
	if f == nil {
		return []ai.StreamEvent{ev}
	}

	switch ev.Type {
	case ai.StreamTextDelta:
		if f.trimmed {
			return []ai.StreamEvent{ev}
		}

		f.buffer = append(f.buffer, ev.Text...)
		if len(f.buffer) < f.window {
			return nil
		}

		return f.release()
	case ai.StreamMessageEnd:
		return append(f.release(), ev)
	default:
		return []ai.StreamEvent{ev}
	}
}

// flush releases any buffered text when a stream ends without a message-end
// event. It is a no-op for an unfiltered call.
func (f *continuationFilter) flush() []ai.StreamEvent {
	if f == nil {
		return nil
	}

	return f.release()
}

// release flushes the buffer once, dropping the overlap it repeats from the
// prefix tail. A continuation shorter than the window releases its whole
// buffer here at stream end.
func (f *continuationFilter) release() []ai.StreamEvent {
	if f.trimmed {
		return nil
	}

	f.trimmed = true

	drop := longestOverlap(f.prefix, f.buffer, f.window)
	released := f.buffer[drop:]
	f.buffer = nil

	if len(released) == 0 {
		return nil
	}

	return []ai.StreamEvent{{Type: ai.StreamTextDelta, Text: string(released)}}
}

// longestOverlap returns the length of the longest suffix of prefix, capped at
// window, that is also a prefix of buffered. A match shorter than
// continuationMinOverlapBytes is coincidence and returns zero.
func longestOverlap(prefix, buffered []byte, window int) int {
	for length := min(window, len(buffered)); length >= continuationMinOverlapBytes; length-- {
		if bytes.Equal(prefix[len(prefix)-length:], buffered[:length]) {
			return length
		}
	}

	return 0
}
