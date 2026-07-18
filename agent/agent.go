package agent

import (
	"context"
	"errors"
	"time"

	"github.com/rsbin/pips/ai"
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
// [ai.Chain] and the ai middleware packages before passing it in.
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
	// DrainOne injects only the oldest queued message per drain point,
	// leaving the rest queued for later points.
	DrainOne QueueMode = iota
	// DrainAll injects every queued message at each drain point.
	DrainAll
)

type config struct {
	name          string
	system        string
	tools         []Tool
	maxTurns      int
	maxTokens     int
	toolTimeout   time.Duration
	parallelTools int
	steeringMode  QueueMode
	followUpMode  QueueMode
	stopWhen      func(RunInfo) bool
	beforeTool    gate
	afterTool     func(context.Context, ToolResultInfo) *ResultOverride
	prepareTurn   func(context.Context, RunInfo) TurnUpdate
	transform     func(context.Context, []ai.Message) ([]ai.Message, error)
	onEvent       func(context.Context, Event)
	requestFn     func(*ai.Request)
	inputGuards   []inputGuardrail
	outputGuards  []outputGuardrail
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

// WithBeforeTool installs a gate consulted before each tool call executes.
// Gates run serially in call order on the run's goroutine. See
// [DecisionAction] for the available verdicts.
func WithBeforeTool(fn func(ctx context.Context, info ToolCallInfo) Decision) Option {
	return func(c *config) { c.beforeTool = fn }
}

// WithAfterTool installs a hook that runs after each executed tool call,
// before its result is recorded and emitted. The returned [ResultOverride]
// replaces result fields (nil keeps everything). The hook only sees calls
// that actually executed — denials, unknown tools, undecodable arguments,
// and cancellations skip it. It runs serially on the run's goroutine; a
// panic converts the result into an error result.
func WithAfterTool(fn func(ctx context.Context, info ToolResultInfo) *ResultOverride) Option {
	return func(c *config) { c.afterTool = fn }
}

// WithPrepareTurn installs a hook that runs after each completed turn,
// before the loop decides whether to continue. The returned [TurnUpdate]
// can swap the model for subsequent turns or rewrite the session history
// (the commit point for context compaction).
func WithPrepareTurn(fn func(ctx context.Context, info RunInfo) TurnUpdate) Option {
	return func(c *config) { c.prepareTurn = fn }
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
// [DrainOne]).
func WithSteeringMode(m QueueMode) Option {
	return func(c *config) { c.steeringMode = m }
}

// WithFollowUpMode sets how queued follow-up messages are drained (default
// [DrainOne]).
func WithFollowUpMode(m QueueMode) Option {
	return func(c *config) { c.followUpMode = m }
}

// WithOnEvent installs a callback receiving every run event, for both
// [Agent.Run] and [Agent.Stream] (Run produces no delta events). The callback
// runs on the run's goroutine; keep it fast and non-blocking.
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
// before it is sent, after the agent has set Messages, System, and Tools. Use
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

// request assembles the model request for one turn.
func (a *Agent) request(msgs []ai.Message) ai.Request {
	req := ai.Request{
		Messages: msgs,
		System:   a.cfg.system,
		Tools:    a.tools.decls,
	}
	if a.cfg.requestFn != nil {
		a.cfg.requestFn(&req)
	}

	return req
}
