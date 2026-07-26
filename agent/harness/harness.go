package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"strings"
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
	cancel  context.CancelFunc
	cfg     hconfig
}

type hconfig struct {
	loaders      []func(*hconfig) error
	tools        []agent.Tool
	system       string
	systemFn     func(SystemContext) string
	systemSuffix string
	skills       []Skill
	skillCatalog *SkillCatalog
	templates    []PromptTemplate
	compaction   *CompactionSettings
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

// WithSystemSuffix appends an application-owned suffix after the generated
// available-Skills block. It is intended for interaction-scoped context and
// explicitly activated Skill instructions that must not rewrite the stable
// system prefix.
func WithSystemSuffix(s string) Option {
	return func(c *hconfig) { c.systemSuffix = s }
}

// WithSkills registers skills, surfaced to the model through the system
// prompt (see [FormatSkillsPrompt]).
func WithSkills(skills ...Skill) Option {
	return func(c *hconfig) { c.skills = append(c.skills, skills...) }
}

// WithSkillCatalog registers a validated catalog. Its discovery entries are
// included in the system prompt; applications can retain the catalog to
// explicitly activate full instructions and inspect activation records.
func WithSkillCatalog(catalog *SkillCatalog) Option {
	return func(c *hconfig) { c.skillCatalog = catalog }
}

// WithSkillsDir loads skills from a directory tree at construction time
// (see [LoadSkills]); [New] fails when loading does.
func WithSkillsDir(dir string) Option {
	return func(c *hconfig) {
		c.loaders = append(c.loaders, func(c *hconfig) error { return loadSkillsInto(c, dir, nil) })
	}
}

// WithSkillsFS is [WithSkillsDir] for any fs.FS — embedded assets included.
func WithSkillsFS(fsys fs.FS) Option {
	return func(c *hconfig) {
		c.loaders = append(c.loaders, func(c *hconfig) error { return loadSkillsInto(c, "", fsys) })
	}
}

// WithTemplatesDir loads prompt templates from a directory at construction
// time (see [LoadTemplates]); [New] fails when loading does.
func WithTemplatesDir(dir string) Option {
	return func(c *hconfig) {
		c.loaders = append(c.loaders, func(c *hconfig) error { return loadTemplatesInto(c, "", os.DirFS(dir)) })
	}
}

// WithTemplatesFS is [WithTemplatesDir] for any fs.FS.
func WithTemplatesFS(fsys fs.FS) Option {
	return func(c *hconfig) {
		c.loaders = append(c.loaders, func(c *hconfig) error { return loadTemplatesInto(c, "", fsys) })
	}
}

func loadSkillsInto(c *hconfig, dir string, fsys fs.FS) error {
	var (
		skills []Skill
		err    error
	)

	if fsys != nil {
		skills, err = LoadSkillsFS(fsys)
	} else {
		skills, err = LoadSkills(dir)
	}

	if err != nil {
		return err
	}

	c.skills = append(c.skills, skills...)

	return nil
}

func loadTemplatesInto(c *hconfig, _ string, fsys fs.FS) error {
	templates, err := LoadTemplatesFS(fsys)
	if err != nil {
		return err
	}

	c.templates = append(c.templates, templates...)

	return nil
}

// WithTemplates registers prompt templates for [Harness.PromptTemplate].
func WithTemplates(templates ...PromptTemplate) Option {
	return func(c *hconfig) { c.templates = append(c.templates, templates...) }
}

// WithCompaction enables automatic compaction: before each prompt, when the
// estimated context crosses the threshold, the harness compacts first.
func WithCompaction(s CompactionSettings) Option {
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

	for _, load := range h.cfg.loaders {
		if err := load(&h.cfg); err != nil {
			return nil, err
		}
	}

	if h.cfg.skillCatalog != nil && len(h.cfg.skills) > 0 {
		return nil, errors.New("harness: WithSkillCatalog cannot be combined with WithSkills")
	}

	if h.cfg.skillCatalog == nil {
		catalog, err := NewSkillCatalog(h.cfg.skills...)
		if err != nil {
			return nil, err
		}

		h.cfg.skillCatalog = catalog
	}

	h.cfg.skills = h.cfg.skillCatalog.List()
	if err := ValidateTemplates(h.cfg.templates...); err != nil {
		return nil, err
	}

	return h, nil
}

// SkillCatalog returns the harness's validated skill catalog. It is safe to
// use concurrently with prompt runs; activation audit records are synchronized.
func (h *Harness) SkillCatalog() *SkillCatalog {
	return h.cfg.skillCatalog
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

// Cancel requests cancellation of the active prompt run. It is safe to call
// concurrently and returns [ErrIdle] when no agent run is active.
func (h *Harness) Cancel() error {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()

	if cancel == nil {
		return ErrIdle
	}

	cancel()

	return nil
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
	run, runCtx, err := h.preparePrompt(ctx, msgs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if run != nil {
			_ = h.finishPrompt(run)
		}
	}()

	result, runErr := run.runner.Run(runCtx, run.session, msgs...)
	recorderErr := h.finishPrompt(run)
	run = nil

	if recorderErr != nil {
		return result, recorderErr
	}

	if runErr != nil {
		return result, runErr
	}

	return result, nil
}

// PromptStream is the streaming form of [Harness.Prompt]. It persists the
// same save points as the blocking path and restores the harness to idle when
// the consumer stops early.
func (h *Harness) PromptStream(ctx context.Context, text string) iter.Seq2[agent.Event, error] {
	return h.PromptMessagesStream(ctx, ai.UserText(text))
}

// PromptMessagesStream is the streaming form of [Harness.PromptMessages].
// Runtime events retain their original order. Normal completion, failure,
// cancellation, and early iterator termination all preserve completed
// messages and release the active prompt lifecycle.
func (h *Harness) PromptMessagesStream(
	ctx context.Context,
	msgs ...ai.Message,
) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		run, runCtx, err := h.preparePrompt(ctx, msgs)
		if err != nil {
			yield(agent.Event{}, err)
			return
		}

		defer func() {
			if run != nil {
				_ = h.finishPrompt(run)
			}
		}()

		for ev, streamErr := range run.runner.Stream(runCtx, run.session, msgs...) {
			if streamErr != nil && run.recorder.err != nil {
				streamErr = run.recorder.err
			}

			if !yield(ev, streamErr) || streamErr != nil {
				return
			}
		}

		recorderErr := h.finishPrompt(run)
		run = nil

		if recorderErr != nil {
			yield(agent.Event{}, recorderErr)
		}
	}
}

type promptRun struct {
	runner   *agent.Agent
	session  *agent.Session
	recorder *recorder
	cancel   context.CancelFunc
}

func (h *Harness) preparePrompt(
	ctx context.Context,
	msgs []ai.Message,
) (_ *promptRun, _ context.Context, err error) {
	if err := h.enter(PhaseTurn); err != nil {
		return nil, nil, err
	}

	prepared := false
	defer func() {
		if !prepared {
			h.exit()
		}
	}()

	cctx, err := h.session.Context()
	if err != nil {
		return nil, nil, err
	}

	runSess := agent.NewSession(cctx.Messages...)
	if len(runSess.Pending()) > 0 {
		return nil, nil, agent.ErrPendingToolCalls
	}

	if h.cfg.compaction != nil && ShouldCompact(EstimateContext(h.session.Path()), *h.cfg.compaction) {
		h.setPhase(PhaseCompaction)

		if err := h.compact(ctx, ""); err != nil {
			return nil, nil, err
		}

		h.setPhase(PhaseTurn)

		cctx, err = h.session.Context()
		if err != nil {
			return nil, nil, err
		}

		runSess = agent.NewSession(cctx.Messages...)
	}

	rec := &recorder{
		session: h.session,
		user:    h.cfg.onEvent,
		input:   append([]ai.Message(nil), msgs...),
	}

	ag, err := h.buildAgent(rec)
	if err != nil {
		return nil, nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	rec.cancel = cancel

	run := &promptRun{
		runner: ag, session: runSess, recorder: rec,
		cancel: cancel,
	}

	h.mu.Lock()
	h.active = run.session
	h.cancel = cancel
	h.mu.Unlock()

	prepared = true

	return run, runCtx, nil
}

func (h *Harness) finishPrompt(run *promptRun) error {
	run.cancel()
	run.recorder.flush(nil) // save-point: whatever the outcome, keep what completed
	h.exit()

	return run.recorder.err
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

// ResolveToolCalls answers any subset of durable pending calls (see
// [agent.Session.ResolveToolCalls]) and persists the new result message.
func (h *Harness) ResolveToolCalls(resolutions ...agent.ToolResolution) error {
	if err := h.enter(PhaseTurn); err != nil {
		return err
	}
	defer h.exit()

	cctx, err := h.session.Context()
	if err != nil {
		return err
	}

	runSess := agent.NewSession(cctx.Messages...)
	if err := runSess.ResolveToolCalls(resolutions...); err != nil {
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
	settings := CompactionSettings{}
	if h.cfg.compaction != nil {
		settings = *h.cfg.compaction
	}

	prep := PlanCompaction(h.session.Path(), settings)
	if prep == nil {
		return ErrNothingToCompact
	}

	summary, err := SummarizeCompaction(ctx, h.summaryModel(), prep, settings, instructions)
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

// systemPrompt assembles the effective system prompt in the stable order:
// configured base, generated Skills index, then application-owned suffix.
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

	parts := make([]string, 0, 3)

	for _, part := range []string{base, FormatSkillsPrompt(h.cfg.skills), h.cfg.systemSuffix} {
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "\n\n")
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
	h.cancel = nil
	h.mu.Unlock()
}

// recorder captures run events, persisting each turn's messages at its
// turn_end save point with per-turn usage derived from the cumulative
// counters. Events arrive on the run goroutine, so no locking is needed.
type recorder struct {
	session *Session
	user    func(context.Context, agent.Event)
	input   []ai.Message
	cancel  context.CancelFunc

	buffer  []ai.Message
	lastCum ai.Usage
	err     error
}

func (r *recorder) onEvent(ctx context.Context, ev agent.Event) {
	switch ev.Type {
	case agent.EventTurnStart:
		r.saveInput()
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

// saveInput persists a prompt only after the runtime has passed pending-call
// and input-guardrail checks and opened its first turn.
func (r *recorder) saveInput() {
	for _, msg := range r.input {
		if _, err := r.session.AppendMessage(msg, nil); err != nil {
			if r.err == nil {
				r.err = err
			}

			if r.cancel != nil {
				r.cancel()
			}

			break
		}
	}

	r.input = nil
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
