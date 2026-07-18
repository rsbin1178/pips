package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

// Phase is what a [Harness] is currently doing.
type Phase string

// Harness phases.
const (
	PhaseIdle          Phase = "idle"
	PhaseTurn          Phase = "turn"
	PhaseCompaction    Phase = "compaction"
	PhaseBranchSummary Phase = "branch_summary"
)

// Harness orchestrates an agent over a persistent [Session]: each prompt
// reconstructs the model-visible context from the tree, runs the agent loop,
// and records every new message back — with usage accounting, automatic
// compaction, branch navigation, and skill/template resources.
//
// A harness runs one operation at a time ([ErrBusy] otherwise); its phases
// are visible through [Harness.Phase].
type Harness struct {
	mu      sync.Mutex
	phase   Phase
	model   ai.LanguageModel
	session *Session
	active  *agent.Session
	cfg     hconfig
}

type hconfig struct {
	tools        []agent.Tool
	system       string
	systemFn     func(SystemContext) string
	skills       []Skill
	templates    []PromptTemplate
	compaction   *Settings
	summaryModel ai.LanguageModel
	onEvent      func(context.Context, agent.Event)
	agentOpts    []agent.Option
}

// Option configures a [Harness].
type Option func(*hconfig)

// WithTools sets the agent tool set.
func WithTools(tools ...agent.Tool) Option {
	return func(c *hconfig) { c.tools = append(c.tools, tools...) }
}

// WithSystem sets a static system prompt (the skills block is appended).
func WithSystem(s string) Option {
	return func(c *hconfig) { c.system = s }
}

// WithSystemFunc assembles the system prompt per prompt run; the skills
// block is appended to its result. It overrides [WithSystem].
func WithSystemFunc(fn func(SystemContext) string) Option {
	return func(c *hconfig) { c.systemFn = fn }
}

// WithSkills registers skills, surfaced to the model through the system
// prompt (see [FormatSkillsPrompt]).
func WithSkills(skills ...Skill) Option {
	return func(c *hconfig) { c.skills = append(c.skills, skills...) }
}

// WithTemplates registers prompt templates for [Harness.PromptTemplate].
func WithTemplates(templates ...PromptTemplate) Option {
	return func(c *hconfig) { c.templates = append(c.templates, templates...) }
}

// WithCompaction enables automatic compaction: before each prompt, when the
// estimated context crosses the threshold, the harness compacts first.
func WithCompaction(s Settings) Option {
	return func(c *hconfig) { c.compaction = &s }
}

// WithSummaryModel sets a dedicated (typically cheaper) model for compaction
// and branch summaries; the main model is used otherwise.
func WithSummaryModel(m ai.LanguageModel) Option {
	return func(c *hconfig) { c.summaryModel = m }
}

// WithOnEvent observes agent events across all prompt runs. Configure event
// observation here rather than through [WithAgentOptions] — the harness
// chains its own recording callback.
func WithOnEvent(fn func(context.Context, agent.Event)) Option {
	return func(c *hconfig) { c.onEvent = fn }
}

// WithAgentOptions passes options through to the underlying agent (gates,
// hooks, limits, request tweaks). Do not pass agent.WithOnEvent here — use
// [WithOnEvent]; and tools/system configured on the harness win.
func WithAgentOptions(opts ...agent.Option) Option {
	return func(c *hconfig) { c.agentOpts = append(c.agentOpts, opts...) }
}

// New returns a harness driving model over the given session.
func New(model ai.LanguageModel, sess *Session, opts ...Option) (*Harness, error) {
	if model == nil {
		return nil, errors.New("harness: nil model")
	}

	if sess == nil {
		return nil, errors.New("harness: nil session")
	}

	h := &Harness{phase: PhaseIdle, model: model, session: sess}
	for _, opt := range opts {
		opt(&h.cfg)
	}

	return h, nil
}

// Session returns the underlying session tree.
func (h *Harness) Session() *Session {
	return h.session
}

// Model returns the model serving the next prompt.
func (h *Harness) Model() ai.LanguageModel {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.model
}

// Phase reports what the harness is currently doing.
func (h *Harness) Phase() Phase {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.phase
}

// SetModel switches the model for later prompts, recording a model_change
// entry. It fails with [ErrBusy] during an active operation.
func (h *Harness) SetModel(m ai.LanguageModel) error {
	if err := h.enter(PhaseTurn); err != nil {
		return err
	}
	defer h.exit()

	if _, err := h.session.AppendModelChange(m.Provider(), m.ModelID()); err != nil {
		return err
	}

	h.mu.Lock()
	h.model = m
	h.mu.Unlock()

	return nil
}

// Steer queues messages into the active run (see [agent.Session.Steer]). It
// fails with [ErrIdle] when no run is active.
func (h *Harness) Steer(msgs ...ai.Message) error {
	return h.queue(func(s *agent.Session) { s.Steer(msgs...) })
}

// FollowUp queues follow-up work into the active run (see
// [agent.Session.FollowUp]). It fails with [ErrIdle] when no run is active.
func (h *Harness) FollowUp(msgs ...ai.Message) error {
	return h.queue(func(s *agent.Session) { s.FollowUp(msgs...) })
}

func (h *Harness) queue(fn func(*agent.Session)) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.active == nil {
		return ErrIdle
	}

	fn(h.active)

	return nil
}

// Prompt runs one user prompt through the agent loop, persisting the
// exchange to the session.
func (h *Harness) Prompt(ctx context.Context, text string) (*agent.RunResult, error) {
	return h.PromptMessages(ctx, ai.UserText(text))
}

// PromptTemplate formats the named template with args (see
// [PromptTemplate.Format]) and prompts with the result.
func (h *Harness) PromptTemplate(ctx context.Context, name string, args ...string) (*agent.RunResult, error) {
	for _, t := range h.cfg.templates {
		if t.Name == name {
			return h.Prompt(ctx, t.Format(args...))
		}
	}

	return nil, fmt.Errorf("harness: unknown prompt template %q", name)
}

// PromptMessages runs the agent loop over the session's reconstructed
// context plus msgs. When automatic compaction is enabled and the estimated
// context crosses the threshold, the harness compacts first. All new
// messages — prompts, assistant turns, tool results, injected steering —
// are persisted at turn boundaries, with assistant entries carrying their
// turn's usage; a failed run keeps everything recorded up to the failure.
func (h *Harness) PromptMessages(ctx context.Context, msgs ...ai.Message) (*agent.RunResult, error) {
	if err := h.enter(PhaseTurn); err != nil {
		return nil, err
	}
	defer h.exit()

	if h.cfg.compaction != nil && ShouldCompact(EstimateContext(h.session.Path()), *h.cfg.compaction) {
		h.setPhase(PhaseCompaction)

		if err := h.compact(ctx, ""); err != nil {
			return nil, err
		}

		h.setPhase(PhaseTurn)
	}

	cctx, err := h.session.Context()
	if err != nil {
		return nil, err
	}

	for _, msg := range msgs {
		if _, err := h.session.AppendMessage(msg, nil); err != nil {
			return nil, err
		}
	}

	rec := &recorder{session: h.session, user: h.cfg.onEvent}

	ag, err := h.buildAgent(rec)
	if err != nil {
		return nil, err
	}

	runSess := agent.NewSession(cctx.Messages...)

	h.mu.Lock()
	h.active = runSess
	h.mu.Unlock()

	result, runErr := ag.Run(ctx, runSess, msgs...)

	rec.flush(nil) // save-point: whatever the outcome, keep what completed

	if runErr != nil {
		return result, runErr
	}

	return result, rec.err
}

// ResolvePending answers tool calls left pending by a paused run (see
// [agent.StopPaused]), persisting the resolution so the next prompt can
// continue.
func (h *Harness) ResolvePending(ctx context.Context, fn func(ctx context.Context, call ai.ToolCallPart) ([]ai.Part, error)) error {
	if err := h.enter(PhaseTurn); err != nil {
		return err
	}
	defer h.exit()

	cctx, err := h.session.Context()
	if err != nil {
		return err
	}

	runSess := agent.NewSession(cctx.Messages...)
	if err := runSess.ResolvePending(ctx, fn); err != nil {
		return err
	}

	resolved := runSess.Messages()
	for _, msg := range resolved[len(cctx.Messages):] {
		if _, err := h.session.AppendMessage(msg, nil); err != nil {
			return err
		}
	}

	return nil
}

// Compact summarizes older history into a compaction entry, optionally
// focused by instructions. It fails with [ErrNothingToCompact] when there is
// nothing to fold.
func (h *Harness) Compact(ctx context.Context, instructions string) error {
	if err := h.enter(PhaseCompaction); err != nil {
		return err
	}
	defer h.exit()

	return h.compact(ctx, instructions)
}

func (h *Harness) compact(ctx context.Context, instructions string) error {
	settings := Settings{}
	if h.cfg.compaction != nil {
		settings = *h.cfg.compaction
	}

	prep := Prepare(h.session.Path(), settings)
	if prep == nil {
		return ErrNothingToCompact
	}

	summary, err := Summarize(ctx, h.summaryModel(), prep, settings, instructions)
	if err != nil {
		return err
	}

	_, err = h.session.AppendCompaction(summary, prep.FirstKeptID, prep.TokensBefore)

	return err
}

// NavigateTo moves the session to an earlier entry, branching the tree.
// With summarize, the abandoned branch is summarized (with the summary
// model) and recorded so its context carries over.
func (h *Harness) NavigateTo(ctx context.Context, entryID string, summarize bool) error {
	if err := h.enter(PhaseBranchSummary); err != nil {
		return err
	}
	defer h.exit()

	summary := ""

	if summarize {
		ancestor, err := h.session.CommonAncestor(h.session.LeafID(), entryID)
		if err != nil {
			return err
		}

		summary, err = SummarizeBranch(ctx, h.summaryModel(), h.session, h.session.LeafID(), ancestor)
		if err != nil && !errors.Is(err, ErrNothingToCompact) {
			return err
		}
	}

	return h.session.MoveTo(entryID, summary)
}

func (h *Harness) summaryModel() ai.LanguageModel {
	if h.cfg.summaryModel != nil {
		return h.cfg.summaryModel
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.model
}

// buildAgent assembles the per-prompt agent: user pass-through options
// first, then the harness-owned tools, system prompt, and event recorder
// (which chains the user's observer).
func (h *Harness) buildAgent(rec *recorder) (*agent.Agent, error) {
	opts := append([]agent.Option{}, h.cfg.agentOpts...)
	opts = append(opts,
		agent.WithTools(h.cfg.tools...),
		agent.WithSystem(h.systemPrompt()),
		agent.WithOnEvent(rec.onEvent),
	)

	h.mu.Lock()
	model := h.model
	h.mu.Unlock()

	return agent.New(model, opts...)
}

// systemPrompt assembles the effective system prompt: the configured base
// (static or callback) plus the skills block.
func (h *Harness) systemPrompt() string {
	base := h.cfg.system
	if h.cfg.systemFn != nil {
		base = h.cfg.systemFn(SystemContext{
			Model:     h.model,
			Skills:    h.cfg.skills,
			Templates: h.cfg.templates,
			Session:   h.session,
		})
	}

	block := FormatSkillsPrompt(h.cfg.skills)

	switch {
	case base == "":
		return block
	case block == "":
		return base
	default:
		return base + "\n\n" + block
	}
}

func (h *Harness) enter(p Phase) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.phase != PhaseIdle {
		return ErrBusy
	}

	h.phase = p

	return nil
}

func (h *Harness) setPhase(p Phase) {
	h.mu.Lock()
	h.phase = p
	h.mu.Unlock()
}

func (h *Harness) exit() {
	h.mu.Lock()
	h.phase = PhaseIdle
	h.active = nil
	h.mu.Unlock()
}

// recorder captures run events, persisting each turn's messages at its
// turn_end save point with per-turn usage derived from the cumulative
// counters. Events arrive on the run goroutine, so no locking is needed.
type recorder struct {
	session *Session
	user    func(context.Context, agent.Event)

	buffer  []ai.Message
	lastCum ai.Usage
	err     error
}

func (r *recorder) onEvent(ctx context.Context, ev agent.Event) {
	switch ev.Type {
	case agent.EventMessage:
		r.buffer = append(r.buffer, *ev.Message)
	case agent.EventTurnEnd:
		delta := usageDelta(ev.Usage, r.lastCum)
		r.lastCum = ev.Usage
		r.flush(&delta)
	default:
	}

	if r.user != nil {
		r.user(ctx, ev)
	}
}

// flush persists buffered messages; usage (when known) lands on the turn's
// assistant entry.
func (r *recorder) flush(usage *ai.Usage) {
	for _, msg := range r.buffer {
		var u *ai.Usage
		if usage != nil && msg.Role == ai.RoleAssistant {
			u = usage
		}

		if _, err := r.session.AppendMessage(msg, u); err != nil && r.err == nil {
			r.err = err
		}
	}

	r.buffer = nil
}

func usageDelta(cum, prev ai.Usage) ai.Usage {
	return ai.Usage{
		InputTokens:       cum.InputTokens - prev.InputTokens,
		OutputTokens:      cum.OutputTokens - prev.OutputTokens,
		ReasoningTokens:   cum.ReasoningTokens - prev.ReasoningTokens,
		CachedInputTokens: cum.CachedInputTokens - prev.CachedInputTokens,
		CacheWriteTokens:  cum.CacheWriteTokens - prev.CacheWriteTokens,
	}
}
