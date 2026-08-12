package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

const (
	teamProposalToolName             = "propose_team"
	maximumTeamProposalFeedbackBytes = 16 << 10
	teamProposalAgentMaxTurns        = 12
	teamProposalAgentMaxTokens       = 96_000
	teamProposalAgentMaxOutputTokens = 16_384
	teamProposalAgentCatalogID       = "coding.team.proposal"
	teamProposalAgentName            = "coding/team-proposal"
	teamProposalReadToolName         = "read"
	teamProposalListToolName         = "ls"
	teamProposalGlobToolName         = "glob"
	teamProposalGrepToolName         = "grep"
)

var teamProposalReadToolNames = []string{
	teamProposalReadToolName,
	teamProposalListToolName,
	teamProposalGlobToolName,
	teamProposalGrepToolName,
}

type teamProposalAgentInput struct {
	Objective           string               `json:"objective"`
	Previous            *TeamProposalRequest `json:"previous,omitempty"`
	Feedback            string               `json:"feedback,omitempty"`
	ProjectInstructions string               `json:"project_instructions,omitempty"`
}

type teamProposalCollector struct {
	mu      sync.Mutex
	calls   int
	request *TeamProposalRequest
	err     error
	schema  *ai.Schema
}

func newTeamProposalCollector() (*teamProposalCollector, error) {
	schema, err := ai.SchemaFor[TeamProposalRequest]()
	if err != nil {
		return nil, fmt.Errorf("%w: build proposal schema: %w", ErrTeamAdmission, err)
	}

	return &teamProposalCollector{schema: schema}, nil
}

func (c *teamProposalCollector) Decl() ai.Tool {
	return ai.Tool{
		Name:        teamProposalToolName,
		Description: "Submit exactly one validated Team proposal. A successful call ends this proposal run.",
		InputSchema: c.schema,
	}
}

func (c *teamProposalCollector) Exec(
	_ context.Context,
	call agent.ToolCall,
) ([]ai.Part, error) {
	c.mu.Lock()
	c.calls++

	duplicate := c.calls != 1
	if duplicate && c.err == nil {
		c.err = fmt.Errorf("%w: propose_team must be called exactly once", ErrTeamAdmission)
	}
	c.mu.Unlock()

	if duplicate {
		return agent.TextResult("proposal rejected"), agent.ErrTerminate
	}

	request, err := decodeTeamProposalToolRequest(call.Args)
	if err != nil {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()

		return agent.TextResult("proposal rejected"), agent.ErrTerminate
	}

	c.mu.Lock()
	value := cloneTeamProposalRequest(request)
	c.request = &value
	c.mu.Unlock()

	return agent.TextResult("proposal accepted"), agent.ErrTerminate
}

func (c *teamProposalCollector) called() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls != 0
}

func (c *teamProposalCollector) proposal() (TeamProposalRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return TeamProposalRequest{}, c.err
	}

	if c.calls != 1 || c.request == nil {
		return TeamProposalRequest{}, fmt.Errorf(
			"%w: proposal Agent did not submit exactly one proposal",
			ErrTeamAdmission,
		)
	}

	return cloneTeamProposalRequest(*c.request), nil
}

func decodeTeamProposalToolRequest(raw ai.JSON) (TeamProposalRequest, error) {
	if len(raw) == 0 {
		return TeamProposalRequest{}, fmt.Errorf("%w: empty propose_team arguments", ErrTeamAdmission)
	}

	var request TeamProposalRequest

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		return TeamProposalRequest{}, fmt.Errorf("%w: invalid propose_team arguments", ErrTeamAdmission)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return TeamProposalRequest{}, fmt.Errorf("%w: invalid propose_team arguments", ErrTeamAdmission)
	}

	normalized, _, err := validateTeamProposalRequest(request)
	if err != nil {
		return TeamProposalRequest{}, err
	}

	return normalized, nil
}

// GenerateTeamProposal runs an isolated, read-only proposal Agent and only
// installs its validated preview after the run and repository preflight end.
func (r *Runtime) GenerateTeamProposal(
	ctx context.Context,
	prompt TeamProposalPrompt,
) (TeamProposal, error) {
	if r == nil {
		return TeamProposal{}, ErrRuntimeClosed
	}

	if r.isTeamWorker() {
		return TeamProposal{}, fmt.Errorf("%w: Team Workers cannot create Teams", ErrTeamAdmission)
	}

	operationCtx, operation, err := r.beginOperation(
		ctx,
		operationTeamProposalAgent,
		runtimeResolution{},
		nil,
	)
	if err != nil {
		return TeamProposal{}, err
	}
	defer r.endOperation(operation)

	if !validTeamText(prompt.Objective, 16<<10, true) {
		return TeamProposal{}, fmt.Errorf("%w: invalid objective", ErrTeamAdmission)
	}

	if err := r.validateInitialTeamProposalAgentBoundary(); err != nil {
		return TeamProposal{}, err
	}

	if _, err := r.preflightTeam(operationCtx, false); err != nil {
		return TeamProposal{}, err
	}

	request, err := r.runTeamProposalAgent(operationCtx, teamProposalAgentInput{
		Objective: prompt.Objective, ProjectInstructions: r.projectInstructions,
	})
	if err != nil {
		return TeamProposal{}, err
	}

	record, err := r.prepareTeamProposal(operationCtx, request, true)
	if err != nil {
		return TeamProposal{}, err
	}

	return r.installTeamProposal(operationCtx, record)
}

// ReviseTeamProposal replaces one exact current proposal only after an
// isolated proposal run and a fresh preflight both succeed.
func (r *Runtime) ReviseTeamProposal(
	ctx context.Context,
	proposalID string,
	feedback string,
) (TeamProposal, error) {
	if r == nil {
		return TeamProposal{}, ErrRuntimeClosed
	}

	if r.isTeamWorker() {
		return TeamProposal{}, fmt.Errorf("%w: Team Workers cannot revise Teams", ErrTeamAdmission)
	}

	operationCtx, operation, err := r.beginOperation(
		ctx,
		operationTeamProposalAgent,
		runtimeResolution{},
		nil,
	)
	if err != nil {
		return TeamProposal{}, err
	}
	defer r.endOperation(operation)

	if !validTeamText(feedback, maximumTeamProposalFeedbackBytes, true) {
		return TeamProposal{}, fmt.Errorf("%w: invalid proposal feedback", ErrTeamAdmission)
	}

	previous, err := r.currentTeamProposalRecord(proposalID)
	if err != nil {
		return TeamProposal{}, err
	}

	previousRequest := cloneTeamProposalRequest(previous.view.Request)

	request, err := r.runTeamProposalAgent(operationCtx, teamProposalAgentInput{
		Objective: previous.view.Request.Objective,
		Previous:  &previousRequest, Feedback: feedback,
		ProjectInstructions: r.projectInstructions,
	})
	if err != nil {
		return TeamProposal{}, err
	}

	next, err := r.prepareTeamProposal(operationCtx, request, true)
	if err != nil {
		return TeamProposal{}, err
	}

	return r.replaceTeamProposal(operationCtx, previous, next)
}

func (r *Runtime) validateInitialTeamProposalAgentBoundary() error {
	r.mu.Lock()
	mode := r.state.Mode
	coordinator := r.team
	r.mu.Unlock()

	if mode != ModeAgent {
		return fmt.Errorf("%w: Team proposal generation requires Agent mode", ErrTeamAdmission)
	}

	guardState, _ := r.teamGuard.snapshot()
	if coordinator != nil || guardState != teamGuardInactive || r.admission.hasProposals() {
		return ErrTeamActive
	}

	return nil
}

func (r *Runtime) currentTeamProposalRecord(proposalID string) (teamProposalRecord, error) {
	r.mu.Lock()
	mode := r.state.Mode
	coordinator := r.team
	r.mu.Unlock()

	if mode != ModeAgent {
		return teamProposalRecord{}, fmt.Errorf(
			"%w: Team proposal revision requires Agent mode",
			ErrTeamAdmission,
		)
	}

	guardState, _ := r.teamGuard.snapshot()
	if coordinator != nil || guardState != teamGuardProposed {
		return teamProposalRecord{}, ErrTeamProposalNotFound
	}

	r.admission.mu.Lock()
	record, found := r.admission.proposals[proposalID]
	record = cloneTeamProposalRecord(record)
	r.admission.mu.Unlock()

	if !found {
		return teamProposalRecord{}, ErrTeamProposalNotFound
	}

	if !r.admission.now().UTC().Before(record.view.ExpiresAt) {
		return teamProposalRecord{}, ErrTeamProposalStale
	}

	if record.view.ID != proposalID || record.teamID == "" || record.leadID == "" {
		return teamProposalRecord{}, ErrTeamProposalStale
	}

	if _, _, err := validateTeamProposalRequest(record.view.Request); err != nil {
		return teamProposalRecord{}, ErrTeamProposalStale
	}

	digest, err := digestJSON(record.view.Request)
	if err != nil || digest != record.requestDigest {
		return teamProposalRecord{}, ErrTeamProposalStale
	}

	return record, nil
}

func (r *Runtime) replaceTeamProposal(
	ctx context.Context,
	previous teamProposalRecord,
	next teamProposalRecord,
) (TeamProposal, error) {
	if err := ctx.Err(); err != nil {
		return TeamProposal{}, err
	}

	if next.view.ID == previous.view.ID || next.teamID == previous.teamID {
		return TeamProposal{}, fmt.Errorf("%w: proposal identity was not renewed", ErrTeamAdmission)
	}

	guardState, _ := r.teamGuard.snapshot()
	if guardState != teamGuardProposed {
		return TeamProposal{}, ErrTeamProposalNotFound
	}

	r.admission.mu.Lock()

	current, found := r.admission.proposals[previous.view.ID]
	if !found {
		r.admission.mu.Unlock()

		return TeamProposal{}, ErrTeamProposalNotFound
	}

	if len(r.admission.proposals) != 1 ||
		!teamProposalRecordsMatch(current, previous, r.admission.now().UTC()) {
		r.admission.mu.Unlock()

		return TeamProposal{}, ErrTeamProposalStale
	}

	delete(r.admission.proposals, previous.view.ID)
	r.admission.proposals[next.view.ID] = cloneTeamProposalRecord(next)
	r.admission.mu.Unlock()

	r.publishTeamLifecycle(ctx, TeamLifecycle{
		TeamID: previous.teamID, State: TeamLifecycleCancelled,
	})
	r.publishTeamLifecycle(ctx, TeamLifecycle{
		TeamID: next.teamID, State: TeamLifecycleProposed,
	})

	return cloneTeamProposal(next.view), nil
}

func teamProposalRecordsMatch(
	current teamProposalRecord,
	previous teamProposalRecord,
	now time.Time,
) bool {
	if current.view.ID != previous.view.ID || current.teamID != previous.teamID ||
		current.view.Dirty != previous.view.Dirty ||
		!current.view.ExpiresAt.Equal(previous.view.ExpiresAt) ||
		current.requestDigest != previous.requestDigest ||
		!now.Before(current.view.ExpiresAt) {
		return false
	}

	currentDigest, err := digestJSON(current.view.Request)

	return err == nil && currentDigest == previous.requestDigest
}

func (r *Runtime) runTeamProposalAgent(
	ctx context.Context,
	input teamProposalAgentInput,
) (TeamProposalRequest, error) {
	collector, err := newTeamProposalCollector()
	if err != nil {
		return TeamProposalRequest{}, err
	}

	agentTools, err := r.teamProposalAgentTools(ctx, collector)
	if err != nil {
		return TeamProposalRequest{}, err
	}

	systemPrompt, err := teamProposalAgentSystemPrompt(input)
	if err != nil {
		return TeamProposalRequest{}, err
	}

	session, err := harness.NewSession(harness.NewMemoryStore(""))
	if err != nil {
		return TeamProposalRequest{}, fmt.Errorf("%w: create proposal session: %w", ErrTeamAdmission, err)
	}

	proposalHarness, err := harness.New(
		r.model,
		session,
		harness.WithSystem(systemPrompt),
		harness.WithTools(agentTools...),
		harness.WithAgentOptions(
			agent.WithName(teamProposalAgentName),
			agent.WithMaxTurns(teamProposalAgentMaxTurns),
			agent.WithMaxTokens(teamProposalAgentMaxTokens),
			agent.WithParallelTools(1),
			agent.WithToolTimeout(r.opts.ToolTimeout),
			agent.WithStopWhen(func(agent.RunInfo) bool { return collector.called() }),
			agent.WithRequest(r.teamProposalRequestPolicy),
		),
	)
	if err != nil {
		return TeamProposalRequest{}, fmt.Errorf("%w: create proposal Agent: %w", ErrTeamAdmission, err)
	}

	if _, err := proposalHarness.Prompt(
		ctx,
		"Inspect only the relevant workspace context, then call propose_team exactly once.",
	); err != nil {
		return TeamProposalRequest{}, fmt.Errorf("%w: proposal Agent run: %w", ErrTeamAdmission, err)
	}

	return collector.proposal()
}

func (r *Runtime) teamProposalAgentTools(
	ctx context.Context,
	collector *teamProposalCollector,
) ([]agent.Tool, error) {
	local, err := tools.NewCatalog(r.tree, r.opts.ToolLimits)
	if err != nil {
		return nil, err
	}

	proposal, err := catalog.New(catalog.Local(
		teamProposalAgentCatalogID,
		catalog.RiskRead,
		collector,
	)...)
	if err != nil {
		return nil, err
	}

	merged, err := catalog.Merge(local, proposal)
	if err != nil {
		return nil, err
	}

	names := append(slices.Clone(teamProposalReadToolNames), teamProposalToolName)
	policy := catalog.Policy{
		TenantID: r.workspace.Identity().Key(), Allowlist: slices.Clone(names),
		MaxRisk: catalog.RiskRead,
	}

	values, err := merged.Tools(ctx, policy, names...)
	if err != nil {
		return nil, err
	}

	for index, name := range names {
		if index >= len(values) || values[index].Decl().Name != name {
			return nil, fmt.Errorf("%w: proposal Agent catalog mismatch", ErrTeamAdmission)
		}
	}

	return values, nil
}

func (r *Runtime) teamProposalRequestPolicy(request *ai.Request) {
	if r.requestPolicy != nil {
		r.requestPolicy(request)
	}

	if request.MaxTokens == nil || *request.MaxTokens > teamProposalAgentMaxOutputTokens {
		request.MaxTokens = ai.Ptr(teamProposalAgentMaxOutputTokens)
	}

	request.ResponseFormat = nil
}

func teamProposalAgentSystemPrompt(input teamProposalAgentInput) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("%w: encode proposal instructions: %w", ErrTeamAdmission, err)
	}

	return fmt.Sprintf(`You are the constrained Lead proposal planner for one Pips Coding Team preview.
You may inspect the workspace only through read, ls, glob, and grep. You have no write, shell, approval,
question, Plan, Skill, MCP, Extension, Subagent, Team member, or admission authority.

Produce a Team with 1-%d Workers and 1-%d tasks. Worker names must be unique. Every task must have a
stable ID, title, assigned Worker, and an acyclic dependency list. Use the supplied objective, previous
proposal, revision feedback, and project instructions as planning context. Do not claim that work has run.
Call propose_team exactly once with the complete objective/workers/tasks value. Do not answer with a prose
proposal; propose_team is the only accepted result and ends the run.

Proposal input JSON:
%s`, maximumTeamWorkers, maximumTeamTasks, encoded), nil
}
