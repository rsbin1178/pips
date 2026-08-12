//nolint:wsl_v5 // Draft generation keeps the non-persistence and frozen-preview boundary explicit.
package coding

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

const (
	agentDraftToolName              = "propose_agent"
	agentDraftAgentName             = "coding/agent-draft-proposal"
	agentDraftMaxIntentBytes        = 16 << 10
	agentDraftMaxCount              = 32
	agentDraftAgentMaxTurns         = 4
	agentDraftAgentMaxTokens        = 64_000
	agentDraftAgentMaxOutputTokens  = 32_768
	agentDraftOpaqueIDBytes         = 16
	agentDraftDefinitionDescription = "Submit exactly one strict non-executable pips.agent/v1alpha1 Markdown proposal."
)

// AgentDraftScope selects the only two private Pips Agent roots eligible for
// explicit draft promotion.
type AgentDraftScope string

const (
	// AgentDraftScopeUser targets the user-owned Pips Agent root.
	AgentDraftScopeUser AgentDraftScope = "user"
	// AgentDraftScopeProject targets the trusted Workspace's Pips Agent root.
	AgentDraftScopeProject AgentDraftScope = "project"
)

// GenerateAgentDraftRequest is explicit user intent for one model proposal.
// Intent is sent to the configured model but is never stored in the main
// Session or retained in the resulting Draft.
type GenerateAgentDraftRequest struct {
	AgentID string
	Intent  string
	Scope   AgentDraftScope
}

// AgentDraftOrigin is content-free provenance for one ephemeral proposal run.
type AgentDraftOrigin struct {
	SessionID     string      `json:"session_id"`
	InteractionID string      `json:"interaction_id"`
	RunID         string      `json:"run_id"`
	Provider      ai.Provider `json:"provider"`
	Model         string      `json:"model"`
	GenerationID  uint64      `json:"generation_id"`
}

// AgentDraftPreview is the detached declared/effective authority comparison
// compiled from the immutable generation captured during proposal review.
type AgentDraftPreview struct {
	GenerationID      uint64                         `json:"generation_id"`
	PlanDigest        string                         `json:"plan_digest"`
	Model             string                         `json:"model"`
	DeclaredTools     []string                       `json:"declared_tools"`
	Capabilities      []subagent.EffectiveCapability `json:"capabilities"`
	DeclaredSkills    []string                       `json:"declared_skills"`
	Skills            []string                       `json:"skills"`
	PreloadedSkills   []string                       `json:"preloaded_skills"`
	DelegationTargets []string                       `json:"delegation_targets"`
	PrivateMCP        []string                       `json:"private_mcp"`
	PrivateHooks      []string                       `json:"private_hooks"`
	ToolSearch        bool                           `json:"tool_search"`
	Limits            subagent.Limits                `json:"limits"`
	Output            subagent.OutputContract        `json:"output"`
}

// Clone returns a fully detached preview.
func (p AgentDraftPreview) Clone() AgentDraftPreview {
	p.DeclaredTools = slices.Clone(p.DeclaredTools)
	p.Capabilities = slices.Clone(p.Capabilities)
	p.DeclaredSkills = slices.Clone(p.DeclaredSkills)
	p.Skills = slices.Clone(p.Skills)
	p.PreloadedSkills = slices.Clone(p.PreloadedSkills)
	p.DelegationTargets = slices.Clone(p.DelegationTargets)
	p.PrivateMCP = slices.Clone(p.PrivateMCP)
	p.PrivateHooks = slices.Clone(p.PrivateHooks)
	p.Output = p.Output.Clone()

	return p
}

// AgentDraftSummary is safe for list views. It deliberately omits model output,
// description, selectors, definition content, paths, and opaque plan bindings.
type AgentDraftSummary struct {
	DraftID             string          `json:"draft_id"`
	AgentID             string          `json:"agent_id"`
	Scope               AgentDraftScope `json:"scope"`
	DefinitionDigest    string          `json:"definition_digest"`
	CreatedAt           time.Time       `json:"created_at"`
	GenerationID        uint64          `json:"generation_id"`
	EffectiveToolCount  int             `json:"effective_tool_count"`
	SelectedSkillCount  int             `json:"selected_skill_count"`
	PrivateBindingCount int             `json:"private_binding_count"`
}

// AgentDraft is the full local review surface. Definition is never written to
// a Session, Event, telemetry projection, or filesystem before promotion.
type AgentDraft struct {
	Summary    AgentDraftSummary
	Origin     AgentDraftOrigin
	Definition []byte
	Preview    AgentDraftPreview
}

// Clone returns a fully detached Draft.
func (d AgentDraft) Clone() AgentDraft {
	d.Definition = slices.Clone(d.Definition)
	d.Preview = d.Preview.Clone()

	return d
}

type agentDraftRecord struct {
	draft     AgentDraft
	promoting bool
}

type agentDraftProposal struct {
	definition []byte
	parsed     agentprofile.Definition
	runID      string
	stop       agent.StopReason
	usage      ai.Usage
}

type agentDraftProposalRequest struct {
	Definition string `json:"definition" description:"Complete strict pips.agent/v1alpha1 Markdown for the caller-supplied Agent ID"`
}

type agentDraftCollector struct {
	mu         sync.Mutex
	calls      int
	agentID    string
	limits     agentprofile.Limits
	definition []byte
	parsed     *agentprofile.Definition
	err        error
	schema     *ai.Schema
}

func newAgentDraftCollector(agentID string, limits agentprofile.Limits) (*agentDraftCollector, error) {
	schema, err := ai.SchemaFor[agentDraftProposalRequest]()
	if err != nil {
		return nil, fmt.Errorf("%w: build proposal schema", ErrAgentDraft)
	}

	return &agentDraftCollector{agentID: agentID, limits: limits, schema: schema}, nil
}

func (c *agentDraftCollector) Decl() ai.Tool {
	return ai.Tool{
		Name: agentDraftToolName, Description: agentDraftDefinitionDescription, InputSchema: c.schema,
	}
}

func (c *agentDraftCollector) Exec(
	_ context.Context,
	call agent.ToolCall,
) ([]ai.Part, error) {
	c.mu.Lock()
	c.calls++
	duplicate := c.calls != 1
	if duplicate && c.err == nil {
		c.err = fmt.Errorf("%w: propose_agent must be called exactly once", ErrAgentDraft)
	}
	c.mu.Unlock()

	if duplicate {
		return agent.TextResult("proposal rejected"), agent.ErrTerminate
	}

	definition, parsed, err := decodeAgentDraftProposal(call.Args, c.agentID, c.limits)
	if err != nil {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()

		return agent.TextResult("proposal rejected"), agent.ErrTerminate
	}

	c.mu.Lock()
	c.definition = slices.Clone(definition)
	value := parsed.Clone()
	c.parsed = &value
	c.mu.Unlock()

	return agent.TextResult("proposal accepted for explicit local review"), agent.ErrTerminate
}

func (c *agentDraftCollector) called() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls != 0
}

func (c *agentDraftCollector) proposal() ([]byte, agentprofile.Definition, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return nil, agentprofile.Definition{}, c.err
	}
	if c.calls != 1 || c.parsed == nil || len(c.definition) == 0 {
		return nil, agentprofile.Definition{}, fmt.Errorf(
			"%w: proposal Agent did not submit exactly one definition", ErrAgentDraft,
		)
	}

	return slices.Clone(c.definition), c.parsed.Clone(), nil
}

func decodeAgentDraftProposal(
	raw ai.JSON,
	agentID string,
	limits agentprofile.Limits,
) ([]byte, agentprofile.Definition, error) {
	if len(raw) == 0 {
		return nil, agentprofile.Definition{}, fmt.Errorf("%w: empty propose_agent arguments", ErrAgentDraft)
	}
	maximumArguments := limits.MaxDefinitionBytes*6 + (4 << 10)
	if limits.MaxDefinitionBytes <= 0 || int64(len(raw)) > maximumArguments {
		return nil, agentprofile.Definition{}, fmt.Errorf("%w: invalid propose_agent arguments", ErrAgentDraft)
	}

	var request agentDraftProposalRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, agentprofile.Definition{}, fmt.Errorf("%w: invalid propose_agent arguments", ErrAgentDraft)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, agentprofile.Definition{}, fmt.Errorf("%w: invalid propose_agent arguments", ErrAgentDraft)
	}

	definition := []byte(request.Definition)
	parsed, err := agentprofile.ParseOneShot(agentID, definition, limits)
	if err != nil {
		return nil, agentprofile.Definition{}, fmt.Errorf("%w: proposed definition is invalid", ErrAgentDraft)
	}

	return slices.Clone(definition), parsed, nil
}

// GenerateAgentDraft runs one isolated proposal Agent and stores only a
// process-local, non-executable Draft after strict parsing and current-
// generation compilation succeed.
func (r *Runtime) GenerateAgentDraft(
	ctx context.Context,
	request GenerateAgentDraftRequest,
) (AgentDraft, error) {
	if r == nil {
		return AgentDraft{}, ErrRuntimeClosed
	}

	request.Intent = strings.TrimSpace(request.Intent)
	if request.Scope == "" {
		request.Scope = AgentDraftScopeUser
	}
	if err := validateGenerateAgentDraftRequest(request); err != nil {
		return AgentDraft{}, err
	}
	operationCtx, operation, err := r.beginOperation(
		ctx, operationAgentDraft, runtimeResolution{}, nil,
	)
	if err != nil {
		return AgentDraft{}, err
	}
	defer r.endOperation(operation)

	if err := r.validateAgentDraftGeneration(request.Scope); err != nil {
		return AgentDraft{}, err
	}

	draftID, err := newAgentDraftID()
	if err != nil {
		return AgentDraft{}, err
	}

	var draft AgentDraft
	err = r.withAgentDraftInteraction(operationCtx, func(current *interaction) error {
		proposal, proposalErr := r.runAgentDraftProposal(operationCtx, request)
		if proposalErr != nil {
			return proposalErr
		}
		current.stop = proposal.stop
		current.usage = tokenUsageFromAI(proposal.usage)
		current.runIDs = append(current.runIDs, proposal.runID)

		definition := agentDraftCustomDefinition(proposal.parsed, request.Scope)
		preview, previewErr := compileAgentDraftPreview(operationCtx, current, definition)
		if previewErr != nil {
			return previewErr
		}

		draft = newAgentDraft(
			r, draftID, request.Scope, proposal.definition, definition,
			current.id, proposal.runID, preview,
		)
		return nil
	})
	if err != nil {
		return AgentDraft{}, err
	}

	if err := r.storeAgentDraft(draft); err != nil {
		return AgentDraft{}, err
	}

	return draft.Clone(), nil
}

func (r *Runtime) validateAgentDraftGeneration(scope AgentDraftScope) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isTeamWorker() || !r.config.DynamicSubagents || r.state.Mode != ModeAgent {
		return fmt.Errorf("%w: Agent draft generation is unavailable", ErrAgentDraft)
	}
	if scope == AgentDraftScopeProject && !r.trusted {
		return fmt.Errorf("%w: project Agent promotion requires a trusted Workspace", ErrAgentDraft)
	}
	if len(r.agentDrafts) >= agentDraftMaxCount {
		return fmt.Errorf("%w: draft count limit reached", ErrAgentDraft)
	}

	return nil
}

func (r *Runtime) storeAgentDraft(draft AgentDraft) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing || r.agentDrafts == nil {
		return ErrRuntimeClosed
	}
	if len(r.agentDrafts) >= agentDraftMaxCount {
		return fmt.Errorf("%w: draft count limit reached", ErrAgentDraft)
	}
	r.agentDrafts[draft.Summary.DraftID] = agentDraftRecord{draft: draft.Clone()}

	return nil
}

func validateGenerateAgentDraftRequest(request GenerateAgentDraftRequest) error {
	if strings.TrimSpace(request.AgentID) != request.AgentID || request.Intent == "" ||
		len(request.Intent) > agentDraftMaxIntentBytes || !utf8.ValidString(request.Intent) ||
		strings.ContainsRune(request.Intent, '\x00') ||
		(request.Scope != AgentDraftScopeUser && request.Scope != AgentDraftScopeProject) {
		return fmt.Errorf("%w: invalid Agent ID, intent, or scope", ErrAgentDraft)
	}

	// Reuse the strict ID parser without inventing a second ID grammar.
	_, err := agentprofile.ParseOneShot(
		request.AgentID,
		[]byte("---\nschema: pips.agent/v1alpha1\nname: Draft\ndescription: Validate ID.\n---\nValidate only."),
		agentprofile.DefaultLimits(),
	)
	if err != nil {
		return fmt.Errorf("%w: invalid Agent ID, intent, or scope", ErrAgentDraft)
	}

	return nil
}

func (r *Runtime) runAgentDraftProposal(
	ctx context.Context,
	request GenerateAgentDraftRequest,
) (agentDraftProposal, error) {
	limits := r.opts.AgentProfileLimits
	if limits == (agentprofile.Limits{}) {
		limits = agentprofile.DefaultLimits()
	}
	collector, err := newAgentDraftCollector(request.AgentID, limits)
	if err != nil {
		return agentDraftProposal{}, err
	}

	memorySession, err := harness.NewSession(harness.NewMemoryStore(""))
	if err != nil {
		return agentDraftProposal{}, fmt.Errorf("%w: create in-memory proposal session", ErrAgentDraft)
	}
	system, err := agentDraftSystemPrompt(request)
	if err != nil {
		return agentDraftProposal{}, err
	}
	proposalHarness, err := harness.New(
		r.model,
		memorySession,
		harness.WithSystem(system),
		harness.WithTools(collector),
		harness.WithAgentOptions(
			agent.WithName(agentDraftAgentName),
			agent.WithMaxTurns(agentDraftAgentMaxTurns),
			agent.WithMaxTokens(agentDraftAgentMaxTokens),
			agent.WithParallelTools(1),
			agent.WithToolTimeout(r.opts.ToolTimeout),
			agent.WithStopWhen(func(agent.RunInfo) bool { return collector.called() }),
			agent.WithRequest(func(request *ai.Request) {
				r.agentDraftRequestPolicy(request, collector.Decl())
			}),
		),
	)
	if err != nil {
		return agentDraftProposal{}, fmt.Errorf("%w: create proposal Agent", ErrAgentDraft)
	}

	result, err := proposalHarness.Prompt(
		ctx,
		"Call propose_agent exactly once with the complete strict Markdown definition.",
	)
	if err != nil {
		return agentDraftProposal{}, fmt.Errorf("%w: proposal Agent run failed", ErrAgentDraft)
	}
	definition, parsed, err := collector.proposal()
	if err != nil {
		return agentDraftProposal{}, err
	}

	return agentDraftProposal{
		definition: definition, parsed: parsed, runID: result.RunID,
		stop: result.Stop, usage: result.Usage,
	}, nil
}

func (r *Runtime) agentDraftRequestPolicy(request *ai.Request, proposalTool ai.Tool) {
	if r.requestPolicy != nil {
		r.requestPolicy(request)
	}
	// Request policies may tune provider parameters but cannot expand this
	// proposal Agent's authority or structured-output surface.
	request.Tools = []ai.Tool{proposalTool}
	if request.MaxTokens == nil || *request.MaxTokens > agentDraftAgentMaxOutputTokens {
		request.MaxTokens = ai.Ptr(agentDraftAgentMaxOutputTokens)
	}
	request.ResponseFormat = nil
}

func agentDraftSystemPrompt(request GenerateAgentDraftRequest) (string, error) {
	input, err := json.Marshal(struct {
		AgentID string `json:"agent_id"`
		Intent  string `json:"intent"`
	}{AgentID: request.AgentID, Intent: request.Intent})
	if err != nil {
		return "", fmt.Errorf("%w: encode proposal input", ErrAgentDraft)
	}

	return fmt.Sprintf(`You generate one non-executable Pips Coding Agent proposal for explicit local review.
You have no filesystem, workspace, Shell, MCP, Hook, Skill, Subagent, approval, question, registry,
promotion, or persistence authority. propose_agent is your only Tool and a successful call ends the run.

Return complete strict Markdown with pips.agent/v1alpha1 YAML frontmatter and a non-empty instruction body.
The caller already chose the file ID; do not invent paths or credentials. Tool, Skill, delegation, private
integration, model, limit, delivery, visibility, and output declarations are requests only and may be
rejected by the current Runtime compiler. Never include secrets from the intent. Call propose_agent once;
do not answer with prose and do not claim the Agent was installed, trusted, available, or executed.

Proposal input JSON:
%s`, input), nil
}

func agentDraftCustomDefinition(
	definition agentprofile.Definition,
	scope AgentDraftScope,
) agentprofile.Definition {
	definition.Kind = agentprofile.KindCustom
	definition.Scope = draftAgentProfileScope(scope)
	definition.Source = draftAgentProfileSource(scope, definition.ID)

	return definition.Clone()
}

func draftAgentProfileScope(scope AgentDraftScope) agentprofile.Scope {
	if scope == AgentDraftScopeProject {
		return agentprofile.ScopeProjectPips
	}

	return agentprofile.ScopeUserPips
}

func draftAgentProfileSource(scope AgentDraftScope, id string) string {
	return string(draftAgentProfileScope(scope)) + "/" + id + ".md"
}

func validDraftDefinitionSource(definition agentprofile.Definition) bool {
	return definition.Source == draftAgentProfileSource(AgentDraftScopeUser, definition.ID) ||
		definition.Source == draftAgentProfileSource(AgentDraftScopeProject, definition.ID)
}

func compileAgentDraftPreview(
	ctx context.Context,
	current *interaction,
	definition agentprofile.Definition,
) (AgentDraftPreview, error) {
	if current == nil || current.integration == nil {
		return AgentDraftPreview{}, fmt.Errorf("%w: dynamic Agent compiler is unavailable", ErrAgentDraft)
	}
	for _, entry := range current.integration.agentProfilesSnapshot().Entries() {
		if entry.ID == definition.ID {
			return AgentDraftPreview{}, fmt.Errorf("%w: Agent ID already exists", ErrAgentDraft)
		}
	}
	dispatcher, ok := current.userDispatcher.(*customSubagentDispatcher)
	if !ok {
		return AgentDraftPreview{}, fmt.Errorf("%w: dynamic Agent compiler is unavailable", ErrAgentDraft)
	}
	dispatcher, err := dispatcher.withDraft(definition)
	if err != nil {
		return AgentDraftPreview{}, fmt.Errorf("%w: draft compiler rejected the definition", ErrAgentDraft)
	}

	delivery := subagent.DeliveryForeground
	if !definitionAllowsDelivery(definition, delivery) {
		delivery = subagent.DeliveryBackground
	}
	plan, err := dispatcher.Compile(ctx, subagent.Request{
		AgentID: definition.ID, Task: "Preview declared Agent authority.", Delivery: delivery,
	})
	if err != nil {
		return AgentDraftPreview{}, fmt.Errorf("%w: proposed authority is unavailable", ErrAgentDraft)
	}
	digest, err := plan.Digest()
	if err != nil {
		return AgentDraftPreview{}, fmt.Errorf("%w: digest compiled preview", ErrAgentDraft)
	}

	return previewFromAgentDraftPlan(definition, plan, digest), nil
}

func previewFromAgentDraftPlan(
	definition agentprofile.Definition,
	plan subagent.ExecutionPlan,
	digest string,
) AgentDraftPreview {
	declaredTools := make([]string, 0, len(definition.Tools.Allow)+len(definition.Tools.Require))
	for _, selector := range definition.Tools.Allow {
		declaredTools = append(declaredTools, "allow:"+selector.String())
	}
	for _, selector := range definition.Tools.Require {
		declaredTools = append(declaredTools, "require:"+selector.String())
	}
	declaredSkills := append(slices.Clone(definition.Skills.Allow), definition.Skills.Preload...)

	return AgentDraftPreview{
		GenerationID: plan.GenerationID, PlanDigest: digest, Model: plan.Model,
		DeclaredTools: declaredTools, Capabilities: slices.Clone(plan.Capabilities),
		DeclaredSkills: declaredSkills, Skills: slices.Clone(plan.Skills),
		PreloadedSkills: slices.Clone(plan.PreloadedSkills), ToolSearch: plan.ToolSearch,
		DelegationTargets: slices.Clone(plan.DelegationTargets),
		PrivateMCP:        bindingIDs(plan.PrivateMCP), PrivateHooks: bindingIDs(plan.PrivateHooks),
		Limits: plan.Limits, Output: plan.Output.Clone(),
	}
}

func bindingIDs(values []subagent.PrivateBinding) []string {
	ids := make([]string, len(values))
	for index, value := range values {
		ids[index] = value.ID
	}

	return ids
}

func newAgentDraft(
	r *Runtime,
	draftID string,
	scope AgentDraftScope,
	definitionBytes []byte,
	definition agentprofile.Definition,
	interactionID string,
	runID string,
	preview AgentDraftPreview,
) AgentDraft {
	privateCount := len(preview.PrivateMCP) + len(preview.PrivateHooks)

	return AgentDraft{
		Summary: AgentDraftSummary{
			DraftID: draftID, AgentID: definition.ID, Scope: scope,
			DefinitionDigest: definition.Digest, CreatedAt: time.Now().UTC(),
			GenerationID: preview.GenerationID, EffectiveToolCount: len(preview.Capabilities),
			SelectedSkillCount: len(preview.Skills), PrivateBindingCount: privateCount,
		},
		Origin: AgentDraftOrigin{
			SessionID: r.handle.Metadata().ID, InteractionID: interactionID,
			RunID:    runID,
			Provider: r.model.Provider(), Model: r.model.ModelID(),
			GenerationID: preview.GenerationID,
		},
		Definition: slices.Clone(definitionBytes), Preview: preview.Clone(),
	}
}

func newAgentDraftID() (string, error) {
	value := make([]byte, agentDraftOpaqueIDBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("%w: generate draft identity", ErrAgentDraft)
	}

	return "draft-" + hex.EncodeToString(value), nil
}

func (r *Runtime) withAgentDraftInteraction(
	ctx context.Context,
	fn func(*interaction) error,
) error {
	emitter := newEventEmitter(ctx, r, nil, false)
	interactionID, err := r.journal.start()
	if err != nil {
		return err
	}
	started := InteractionStarted{Mode: r.currentOperatingMode()}
	if err := emitter.emit(interactionID, "", EventInteractionStarted, started); err != nil {
		return errors.Join(
			err, r.finishUnopenedInteraction(interactionID, InteractionCanceled, emitter),
		)
	}

	current, err := r.openInteraction(ctx, interactionID, false, emitter, started, nil, nil)
	if err != nil {
		return errors.Join(
			err, r.finishUnopenedInteraction(interactionID, InteractionFailed, emitter),
		)
	}
	if err := emitter.emit(current.id, "", EventStatusChanged, StatusChanged{Phase: PhaseRunning}); err != nil {
		return errors.Join(
			err, r.finishInteraction(context.WithoutCancel(ctx), current, InteractionCanceled, emitter),
		)
	}

	operationErr := fn(current)
	outcome := InteractionSucceeded
	if operationErr != nil {
		outcome = InteractionFailed
		if errors.Is(operationErr, context.Canceled) {
			outcome = InteractionCanceled
		}
	}

	return errors.Join(
		operationErr,
		r.finishInteraction(context.WithoutCancel(ctx), current, outcome, emitter),
	)
}

// ListAgentDrafts returns deterministic content-safe process-local summaries.
func (r *Runtime) ListAgentDrafts(ctx context.Context) ([]AgentDraftSummary, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return nil, ErrRuntimeClosed
	}

	values := make([]AgentDraftSummary, 0, len(r.agentDrafts))
	for _, record := range r.agentDrafts {
		values = append(values, record.draft.Summary)
	}
	slices.SortFunc(values, func(left, right AgentDraftSummary) int {
		if value := left.CreatedAt.Compare(right.CreatedAt); value != 0 {
			return value
		}

		return strings.Compare(left.DraftID, right.DraftID)
	})

	return values, nil
}

// InspectAgentDraft returns one detached full definition for explicit local
// review. It is never used by model-facing Tool composition.
func (r *Runtime) InspectAgentDraft(ctx context.Context, draftID string) (AgentDraft, error) {
	if r == nil {
		return AgentDraft{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return AgentDraft{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return AgentDraft{}, ErrRuntimeClosed
	}
	record, exists := r.agentDrafts[draftID]
	if !exists {
		return AgentDraft{}, fmt.Errorf("%w: unknown draft", ErrAgentDraft)
	}

	return record.draft.Clone(), nil
}

// DiscardAgentDraft removes one exact process-local draft. The expected digest
// prevents a stale review surface from discarding a replacement.
func (r *Runtime) DiscardAgentDraft(
	ctx context.Context,
	draftID string,
	expectedDigest string,
) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return ErrRuntimeClosed
	}
	record, exists := r.agentDrafts[draftID]
	if !exists || record.promoting || expectedDigest == "" ||
		record.draft.Summary.DefinitionDigest != expectedDigest {
		return fmt.Errorf("%w: stale or unknown draft", ErrAgentDraft)
	}
	delete(r.agentDrafts, draftID)

	return nil
}
