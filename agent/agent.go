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

type config struct {
	system        string
	tools         []Tool
	maxTurns      int
	maxTokens     int
	toolTimeout   time.Duration
	parallelTools int
	stopWhen      func(RunInfo) bool
	beforeTool    gate
	onEvent       func(context.Context, Event)
	requestFn     func(*ai.Request)
}

// Option configures an [Agent].
type Option func(*config)

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

// WithOnEvent installs a callback receiving every run event, for both
// [Agent.Run] and [Agent.Stream] (Run produces no delta events). The callback
// runs on the run's goroutine; keep it fast and non-blocking.
func WithOnEvent(fn func(ctx context.Context, ev Event)) Option {
	return func(c *config) { c.onEvent = fn }
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

	if cfg.parallelTools < 1 {
		cfg.parallelTools = 1
	}

	tools, err := newToolbox(cfg.tools)
	if err != nil {
		return nil, err
	}

	return &Agent{model: model, tools: tools, cfg: cfg}, nil
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
