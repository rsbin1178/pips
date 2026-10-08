package agent

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/rsbin1178/pips/ai"
)

// Default limits.
const (
	// DefaultMaxTurns bounds a run to 25 model calls unless overridden with
	// [WithMaxTurns]. An unbounded loop is the classic agent failure mode, so
	// the limit is opt-out rather than opt-in.
	DefaultMaxTurns = 25
	// DefaultParallelTools is the per-turn concurrency limit for tools marked
	// with [Parallel].
	DefaultParallelTools = 4
)

// Agent binds a language model to a system prompt, a tool set, and loop
// policy. It is immutable after construction and safe for concurrent use;
// run-scoped state lives in the [Session] passed to each run.
//
// Resilience is layered at the model, not the agent: wrap the model with
// [ai.Chain] and the ai middleware packages before passing it in. The one
// exception is [WithStreamRecovery]: only the loop can retract output a
// frontend has already rendered, so a turn whose stream broke after producing
// output is re-issued here rather than inside the middleware.
type Agent struct {
	model ai.LanguageModel
	tools *toolbox
	cfg   config
}

// QueueMode controls how many queued messages a drain point injects (see
// [Session.Steer] and [Session.FollowUp]).
type QueueMode int

// Queue modes.
const (
	// QueueDrainOne injects only the oldest queued message per drain point,
	// leaving the rest queued for later points.
	QueueDrainOne QueueMode = iota
	// QueueDrainAll injects every queued message at each drain point.
	QueueDrainAll
)

type config struct {
	name           string
	system         string
	tools          []Tool
	maxTurns       int
	maxTokens      int
	toolTimeout    time.Duration
	parallelTools  int
	steeringMode   QueueMode
	followUpMode   QueueMode
	stopWhen       func(RunInfo) bool
	streamRecovery streamRecovery
	// streamRecoveryWindow bounds the wall clock one turn's re-issue episode may
	// spend, independently of the attempt budget.
	streamRecoveryWindow time.Duration
	beforeTool           gate
	afterTool            func(context.Context, ToolResultInfo) *ToolResultOverride
	prepareTurn          func(context.Context, RunInfo) TurnUpdate
	candidate            func(context.Context, CandidateAnswerInfo) CandidateAnswerDecision
	transform            func(context.Context, []ai.Message) ([]ai.Message, error)
	onEvent              func(context.Context, Event)
	requestFn            func(*ai.Request)
	inputGuards          []inputGuardrail
	outputGuards         []outputGuardrail
}

// Stream recovery limits. The backoff doubles per re-issue up to the ceiling,
// which mirrors the retry middleware's shape one layer up.
const (
	// DefaultStreamRecoveryBase is the backoff before the first re-issue.
	DefaultStreamRecoveryBase = time.Second
	// DefaultStreamRecoveryMax caps one backoff delay.
	DefaultStreamRecoveryMax = 30 * time.Second
)

// streamRecovery bounds the re-issue of a turn whose model stream failed after
// it had already streamed output.
type streamRecovery struct {
	mode     recoveryMode
	attempts int
	base     time.Duration
	max      time.Duration
}

// recoveryMode selects what a re-issue does with the partial answer a failed
// attempt streamed.
type recoveryMode int

const (
	// recoveryDiscard retracts the partial and re-issues the same request.
	// It is the zero value, so an unconfigured agent keeps today's semantics.
	recoveryDiscard recoveryMode = iota
	// recoveryContinue keeps the partial and sends it back as a trailing
	// assistant prefix, asking the model for the missing remainder.
	recoveryContinue
)

func newStreamRecovery(mode recoveryMode, attempts int, base, ceiling time.Duration) streamRecovery {
	if attempts <= 0 {
		return streamRecovery{}
	}

	recovery := streamRecovery{mode: mode, attempts: attempts, base: base, max: ceiling}
	if recovery.base <= 0 {
		recovery.base = DefaultStreamRecoveryBase
	}

	if recovery.max < recovery.base {
		recovery.max = DefaultStreamRecoveryMax
	}

	return recovery
}

// backoff returns the delay before re-issue number attempt (zero-based).
func (r streamRecovery) backoff(attempt int) time.Duration {
	delay := float64(r.base) * math.Pow(2, float64(attempt))
	if delay > float64(r.max) {
		delay = float64(r.max)
	}

	return time.Duration(delay)
}

// Option configures an [Agent].
type Option func(*config)

// WithName assigns a stable human-readable name used in run metadata and
// events. It does not affect model prompts or tool names.
func WithName(name string) Option {
	return func(c *config) { c.name = name }
}

// WithSystem sets the system prompt sent on every model call.
func WithSystem(s string) Option {
	return func(c *config) { c.system = s }
}

// WithTools adds tools to the agent. Tool names must be unique and non-empty;
// [New] fails otherwise.
func WithTools(tools ...Tool) Option {
	return func(c *config) { c.tools = append(c.tools, tools...) }
}

// WithMaxTurns caps the number of model calls per run (default
// [DefaultMaxTurns]). Zero or negative means unlimited — combine with another
// stop condition to avoid unbounded loops.
func WithMaxTurns(n int) Option {
	return func(c *config) { c.maxTurns = n }
}

// WithMaxTokens stops the run with [StopBudget] once its cumulative input
// plus output tokens reach n. Zero (the default) means unlimited.
func WithMaxTokens(n int) Option {
	return func(c *config) { c.maxTokens = n }
}

// WithToolTimeout bounds each tool execution; on expiry the call becomes an
// error tool result and the run continues. Zero (the default) means no
// per-tool deadline.
func WithToolTimeout(d time.Duration) Option {
	return func(c *config) { c.toolTimeout = d }
}

// WithParallelTools caps how many [Parallel]-marked tools of one turn execute
// concurrently (default [DefaultParallelTools]). One serializes everything.
func WithParallelTools(limit int) Option {
	return func(c *config) { c.parallelTools = limit }
}

// WithStopWhen stops the run with [StopWhen] once cond reports true. It is
// checked after each completed turn.
func WithStopWhen(cond func(RunInfo) bool) Option {
	return func(c *config) { c.stopWhen = cond }
}

// WithStreamRecovery re-issues a turn whose model stream failed after it had
// already streamed output, up to attempts extra tries with exponential backoff
// (base, capped by maxDelay; zero values select [DefaultStreamRecoveryBase] and
// [DefaultStreamRecoveryMax]). Zero attempts — the default — surfaces the
// failure instead.
//
// The middleware owns failures that produced nothing; this option covers the
// ones that arrived after output, where replaying from the middleware would
// duplicate what the frontend already rendered. Before each re-issue the loop
// emits [CandidateDiscarded] so a consumer drops the partial output, which
// makes the retry safe: nothing from the failed attempt was committed and no
// tool ran.
func WithStreamRecovery(attempts int, base, maxDelay time.Duration) Option {
	return func(c *config) {
		c.streamRecovery = newStreamRecovery(recoveryDiscard, attempts, base, maxDelay)
	}
}

// ComposeOptions applies several options as one, so a frontend can hand the loop
// a single coherent policy value without knowing which options make it up.
func ComposeOptions(options ...Option) Option {
	return func(c *config) {
		for _, option := range options {
			if option != nil {
				option(c)
			}
		}
	}
}

// WithStreamRecoveryWindow bounds the wall clock one turn's re-issue episode may
// spend. The episode starts when the first re-issue is scheduled, so a slow but
// successful first attempt never counts against it, and once the window is spent
// no further re-issue starts: the turn gives up through the same path as an
// exhausted budget, reporting the retained fragment as incomplete. Zero — the
// default — leaves the attempt budget as the only bound.
//
// It applies whenever recovery is enabled and is independent of the attempt and
// backoff options, so the two can be set in either order.
func WithStreamRecoveryWindow(window time.Duration) Option {
	return func(c *config) { c.streamRecoveryWindow = window }
}

// WithStreamContinuation is [WithStreamRecovery]'s alternative: a re-issue
// keeps the partial answer and sends it back as a trailing assistant prefix,
// asking the model only for the missing remainder (see
// [WithStreamRecovery] for the attempt and backoff semantics). Retained text
// is never retracted or regenerated, so a consumer's live draft keeps growing
// across the re-issue.
//
// Both options configure the same budget; the one set last wins. When no
// text prefix can be retained — the partial carried a tool call, or produced
// no text — the loop falls back to the discard path. Zero attempts (the
// default) surfaces the failure instead.
func WithStreamContinuation(attempts int, base, maxDelay time.Duration) Option {
	return func(c *config) {
		c.streamRecovery = newStreamRecovery(recoveryContinue, attempts, base, maxDelay)
	}
}

// WithBeforeTool installs a gate consulted before each tool call executes.
// Gates run serially in call order on the run's goroutine. See
// [ToolDecisionAction] for the available verdicts.
func WithBeforeTool(fn func(ctx context.Context, info ToolCallInfo) ToolDecision) Option {
	return func(c *config) { c.beforeTool = fn }
}

// WithAfterTool installs a hook that runs after each executed tool call,
// before its result is recorded and emitted. The returned [ToolResultOverride]
// replaces result fields (nil keeps everything). The hook only sees calls
// that actually executed — denials, unknown tools, undecodable arguments,
// and cancellations skip it. It runs serially on the run's goroutine; a
// panic converts the result into an error result.
func WithAfterTool(fn func(ctx context.Context, info ToolResultInfo) *ToolResultOverride) Option {
	return func(c *config) { c.afterTool = fn }
}

// WithPrepareTurn installs a hook that runs after each completed turn,
// before the loop decides whether to continue. The returned [TurnUpdate]
// can swap the model for subsequent turns or rewrite the session history
// (the commit point for context compaction).
func WithPrepareTurn(fn func(ctx context.Context, info RunInfo) TurnUpdate) Option {
	return func(c *config) { c.prepareTurn = fn }
}

// WithCandidateAnswer installs a hook for no-Tool assistant answers before
// they are committed. The hook may accept, abort, or discard and retry with a
// one-request constraint. Streaming events are provisional; a retry emits an
// [EventCandidateDiscarded] event so consumers can clear the rejected draft.
func WithCandidateAnswer(
	fn func(context.Context, CandidateAnswerInfo) CandidateAnswerDecision,
) Option {
	return func(c *config) { c.candidate = fn }
}

// WithTransformContext installs a transform applied to the session snapshot
// before each model call — a non-destructive injection point for context
// pruning or augmentation. The session itself is untouched; use
// [WithPrepareTurn] or [Session.Replace] to rewrite history permanently. A
// returned error terminates the run with that error.
func WithTransformContext(fn func(ctx context.Context, msgs []ai.Message) ([]ai.Message, error)) Option {
	return func(c *config) { c.transform = fn }
}

// WithSteeringMode sets how queued steering messages are drained (default
// [QueueDrainOne]).
func WithSteeringMode(m QueueMode) Option {
	return func(c *config) { c.steeringMode = m }
}

// WithFollowUpMode sets how queued follow-up messages are drained (default
// [QueueDrainOne]).
func WithFollowUpMode(m QueueMode) Option {
	return func(c *config) { c.followUpMode = m }
}

// WithOnEvent installs a callback receiving every run event, for both
// [Agent.Run] and [Agent.Stream] (Run produces no [EventModelStream] events).
// The callback runs on the run's goroutine; keep it fast and non-blocking.
func WithOnEvent(fn func(ctx context.Context, ev Event)) Option {
	return func(c *config) { c.onEvent = fn }
}

// WithInputGuardrail appends a named validator that runs once per invocation,
// before new messages are committed or a model is called. Validators run in
// option order; the first non-nil error rejects the run with a
// [GuardrailError]. Use [WithBeforeTool] for tool side-effect policy.
func WithInputGuardrail(
	name string,
	fn func(context.Context, InputGuardrailInfo) error,
) Option {
	return func(c *config) {
		c.inputGuards = append(c.inputGuards, inputGuardrail{name: name, fn: fn})
	}
}

// WithOutputGuardrail appends a named validator for every candidate assistant
// answer (a response with no tool calls). It runs before that message is
// committed. Queued steering or follow-up can extend the run after a validated
// answer. During [Agent.Stream], model deltas are provisional and may already
// have been observed before validation rejects them.
func WithOutputGuardrail(
	name string,
	fn func(context.Context, OutputGuardrailInfo) error,
) Option {
	return func(c *config) {
		c.outputGuards = append(c.outputGuards, outputGuardrail{name: name, fn: fn})
	}
}

// WithRequest installs an escape hatch applied to each [ai.Request] just
// before it is sent, after the agent has set Messages and Tools. The configured
// system prompt is the leading [ai.SystemMessage] in Messages. Use
// it for generation parameters, reasoning configuration, or provider options:
//
//	agent.WithRequest(func(req *ai.Request) {
//	    req.Temperature = ai.Ptr(0.2)
//	})
func WithRequest(fn func(*ai.Request)) Option {
	return func(c *config) { c.requestFn = fn }
}

// New returns an agent bound to model. It fails when model is nil or the
// configured tools have duplicate or empty names.
func New(model ai.LanguageModel, opts ...Option) (*Agent, error) {
	if model == nil {
		return nil, errors.New("agent: nil model")
	}

	cfg := config{
		maxTurns:      DefaultMaxTurns,
		parallelTools: DefaultParallelTools,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	for _, guard := range cfg.inputGuards {
		if guard.name == "" || guard.fn == nil {
			return nil, errors.New("agent: input guardrail requires a name and function")
		}
	}

	for _, guard := range cfg.outputGuards {
		if guard.name == "" || guard.fn == nil {
			return nil, errors.New("agent: output guardrail requires a name and function")
		}
	}

	if cfg.parallelTools < 1 {
		cfg.parallelTools = 1
	}

	tools, err := newToolbox(cfg.tools)
	if err != nil {
		return nil, err
	}

	return &Agent{model: model, tools: tools, cfg: cfg}, nil
}

// Name returns the agent's configured name, or "" when unnamed.
func (a *Agent) Name() string {
	return a.cfg.name
}

func (a *Agent) requestWithTools(
	msgs []ai.Message,
	tools *toolbox,
	update *runModelRequest,
) ai.Request {
	messages := make(ai.Messages, 0, len(msgs)+1)
	if a.cfg.system != "" {
		messages = append(messages, ai.SystemText(a.cfg.system))
	}

	messages = append(messages, msgs...)

	req := ai.Request{
		Messages: messages,
		Tools:    tools.decls,
	}
	if a.cfg.requestFn != nil {
		a.cfg.requestFn(&req)
	}

	if update != nil {
		if update.systemSuffix != "" {
			prefix := 0
			for prefix < len(req.Messages) {
				if _, ok := req.Messages[prefix].(ai.SystemMessage); !ok {
					break
				}

				prefix++
			}

			suffix := update.systemSuffix
			if prefix > 0 {
				suffix = "\n" + suffix
			}

			req.Messages = slices.Insert(req.Messages, prefix, ai.Message(ai.SystemText(suffix)))
		}

		if update.toolChoice.Mode != "" {
			req.ToolChoice = update.toolChoice
		}
	}

	return req
}
