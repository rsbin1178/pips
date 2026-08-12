package coding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	maximumTeamWorkers          = 3
	maximumTeamTasks            = 128
	defaultTeamTaskAttemptLimit = 3
	teamProposalLifetime        = 15 * time.Minute
)

var (
	// ErrTeamAdmission reports a malformed or unavailable Team admission.
	ErrTeamAdmission = errors.New("coding Team admission failed")
	// ErrTeamRepositoryRequired reports a Workspace that cannot provide the
	// committed repository root required for isolated Worker Worktrees.
	ErrTeamRepositoryRequired = errors.New("coding Team requires a repository root with HEAD")
	// ErrTeamActive reports an existing proposal or admitted Team owner.
	ErrTeamActive = errors.New("coding Team is already active")
	// ErrTeamProposalNotFound reports an unknown or already consumed proposal.
	ErrTeamProposalNotFound = errors.New("coding Team proposal not found")
	// ErrTeamProposalStale reports repository or Runtime drift after preview.
	ErrTeamProposalStale = errors.New("coding Team proposal is stale")
	// ErrTeamDirty requires an explicit HEAD-only admission choice.
	ErrTeamDirty = errors.New("coding Team workspace has uncommitted changes")
	// ErrTeamInteractionRequired prevents implicit writable non-interactive admission.
	ErrTeamInteractionRequired = errors.New("interaction_required")
)

var teamAdmissionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// TeamWorkerSpec is one user-visible logical Worker. Runtime policy, model,
// paths, credentials, and Git refs are intentionally absent.
type TeamWorkerSpec struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// TeamTaskSpec is one immutable task and its dependency/assignment edges.
type TeamTaskSpec struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Description    string   `json:"description,omitempty"`
	AssignedWorker string   `json:"assigned_worker"`
	Dependencies   []string `json:"dependencies,omitempty"`
}

// TeamProposalRequest contains the complete model/user-controllable Team
// shape. It cannot carry approval, policy, repository, or credential data.
type TeamProposalRequest struct {
	Objective string           `json:"objective"`
	Workers   []TeamWorkerSpec `json:"workers"`
	Tasks     []TeamTaskSpec   `json:"tasks"`
}

// TeamProposalPrompt is the user-owned objective for one constrained,
// model-assisted Team proposal run.
type TeamProposalPrompt struct {
	Objective string `json:"objective"`
}

// TeamProposal is a safe preview. The opaque ID binds repository and Runtime
// truth internally without disclosing local absolute paths.
type TeamProposal struct {
	ID        string              `json:"id"`
	Request   TeamProposalRequest `json:"request"`
	Dirty     bool                `json:"dirty"`
	ExpiresAt time.Time           `json:"expires_at"`
}

// Clone returns a fully detached Team proposal preview.
func (value TeamProposal) Clone() TeamProposal {
	return cloneTeamProposal(value)
}

// TeamAdmissionMode records which parent state the user explicitly admitted.
type TeamAdmissionMode string

const (
	TeamAdmissionClean    TeamAdmissionMode = "clean"
	TeamAdmissionHEADOnly TeamAdmissionMode = "head_only"
)

// TeamConfirmation consumes one exact proposal.
type TeamConfirmation struct {
	ProposalID string            `json:"proposal_id"`
	Admission  TeamAdmissionMode `json:"admission"`
}

// TeamReference identifies the durable admitted Team without exposing its
// private control-plane paths.
type TeamReference struct {
	TeamID     team.ID           `json:"team_id"`
	Admission  TeamAdmissionMode `json:"admission"`
	BaseOID    string            `json:"base_oid"`
	LeadMember team.MemberID     `json:"lead_member_id"`
}

type teamAdmission struct {
	mu        sync.Mutex
	proposals map[string]teamProposalRecord
	now       func() time.Time
	newID     func(string) (string, error)
}

type teamProposalRecord struct {
	view                  TeamProposal
	requestDigest         string
	statusDigest          string
	repository            gitcontrol.Repository
	workspace             workspace.Identity
	commonDir             workspace.Identity
	teamID                team.ID
	leadID                team.MemberID
	capabilityFingerprint string
	workers               []admittedWorker
	tasks                 []TeamTaskSpec
}

type admittedWorker struct {
	spec TeamWorkerSpec
	id   team.MemberID
}

type teamPreflight struct {
	repository   gitcontrol.Repository
	workspace    workspace.Identity
	commonDir    workspace.Identity
	statusDigest string
	dirty        bool
}

func newTeamAdmission() *teamAdmission {
	return &teamAdmission{
		proposals: make(map[string]teamProposalRecord),
		now:       time.Now,
		newID:     randomTeamAdmissionID,
	}
}

// ProposeTeam validates and previews a Team without creating any durable Team,
// Continuation, Session, Worktree, branch/ref, lease, or goroutine.
func (r *Runtime) ProposeTeam(
	ctx context.Context,
	request TeamProposalRequest,
) (TeamProposal, error) {
	if r == nil {
		return TeamProposal{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return TeamProposal{}, fmt.Errorf("%w: Team Workers cannot create Teams", ErrTeamAdmission)
	}

	operationCtx, operation, err := r.beginOperation(
		ctx,
		operationTeamPropose,
		runtimeResolution{},
		nil,
	)
	if err != nil {
		return TeamProposal{}, err
	}
	defer r.endOperation(operation)

	record, err := r.prepareTeamProposal(operationCtx, request, true)
	if err != nil {
		return TeamProposal{}, err
	}

	return r.installTeamProposal(operationCtx, record)
}

func (r *Runtime) prepareTeamProposal(
	ctx context.Context,
	request TeamProposalRequest,
	probeSandbox bool,
) (teamProposalRecord, error) {
	normalized, workers, err := validateTeamProposalRequest(request)
	if err != nil {
		return teamProposalRecord{}, err
	}

	preflight, err := r.preflightTeam(ctx, probeSandbox)
	if err != nil {
		return teamProposalRecord{}, err
	}

	requestDigest, err := digestJSON(normalized)
	if err != nil {
		return teamProposalRecord{}, fmt.Errorf("%w: digest proposal: %w", ErrTeamAdmission, err)
	}
	capabilityFingerprint, err := r.teamCapabilityFingerprint()
	if err != nil {
		return teamProposalRecord{}, err
	}

	proposalID, err := r.admission.newID("proposal")
	if err != nil {
		return teamProposalRecord{}, fmt.Errorf("%w: create proposal identity: %w", ErrTeamAdmission, err)
	}
	teamIDValue, err := r.admission.newID("team")
	if err != nil {
		return teamProposalRecord{}, fmt.Errorf("%w: create Team identity: %w", ErrTeamAdmission, err)
	}

	now := r.admission.now().UTC()
	view := TeamProposal{
		ID: proposalID, Request: cloneTeamProposalRequest(normalized),
		Dirty: preflight.dirty, ExpiresAt: now.Add(teamProposalLifetime),
	}

	return teamProposalRecord{
		view: view, requestDigest: requestDigest, statusDigest: preflight.statusDigest,
		repository: preflight.repository, workspace: preflight.workspace,
		commonDir: preflight.commonDir, teamID: team.ID(teamIDValue), leadID: "lead",
		capabilityFingerprint: capabilityFingerprint,
		workers:               workers, tasks: cloneTeamTasks(normalized.Tasks),
	}, nil
}

func (r *Runtime) installTeamProposal(
	ctx context.Context,
	record teamProposalRecord,
) (TeamProposal, error) {
	if err := ctx.Err(); err != nil {
		return TeamProposal{}, err
	}
	if err := r.teamGuard.propose(); err != nil {
		return TeamProposal{}, err
	}

	r.admission.mu.Lock()
	if len(r.admission.proposals) != 0 {
		r.admission.mu.Unlock()
		r.teamGuard.decline()

		return TeamProposal{}, ErrTeamActive
	}

	r.admission.proposals[record.view.ID] = cloneTeamProposalRecord(record)
	r.admission.mu.Unlock()
	r.publishTeamLifecycle(ctx, TeamLifecycle{
		TeamID: record.teamID, State: TeamLifecycleProposed,
	})

	return cloneTeamProposal(record.view), nil
}

// DeclineTeam consumes a proposal without creating resources.
func (r *Runtime) DeclineTeam(ctx context.Context, proposalID string) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.admission.mu.Lock()
	record, found := r.admission.proposals[proposalID]
	if found {
		delete(r.admission.proposals, proposalID)
	}
	r.admission.mu.Unlock()
	if !found {
		return ErrTeamProposalNotFound
	}

	r.teamGuard.decline()
	r.publishTeamLifecycle(ctx, TeamLifecycle{
		TeamID: record.teamID, State: TeamLifecycleCancelled,
	})

	return nil
}

// ConfirmTeam revalidates and consumes one proposal before crossing the first
// durable resource boundary.
func (r *Runtime) ConfirmTeam(
	ctx context.Context,
	confirmation TeamConfirmation,
) (TeamReference, error) {
	if r == nil {
		return TeamReference{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return TeamReference{}, fmt.Errorf("%w: Team Workers cannot admit Teams", ErrTeamAdmission)
	}

	operationCtx, operation, err := r.beginOperation(
		ctx,
		operationTeamConfirm,
		runtimeResolution{},
		nil,
	)
	if err != nil {
		return TeamReference{}, err
	}
	defer r.endOperation(operation)

	record, found := r.admission.take(confirmation.ProposalID)
	if !found {
		return TeamReference{}, ErrTeamProposalNotFound
	}
	lifecycleSettled := false
	defer func() {
		if !lifecycleSettled {
			r.publishTeamLifecycle(operationCtx, TeamLifecycle{
				TeamID: record.teamID, State: TeamLifecycleInterrupted,
				Code: "team_admission_failed",
			})
		}
	}()

	activated := false
	defer func() {
		if !activated {
			r.teamGuard.decline()
		}
	}()

	if !r.admission.now().UTC().Before(record.view.ExpiresAt) {
		return TeamReference{}, ErrTeamProposalStale
	}
	if err := validateAdmissionChoice(record.view.Dirty, confirmation.Admission); err != nil {
		return TeamReference{}, err
	}

	preflight, err := r.preflightTeam(operationCtx, false)
	if err != nil {
		return TeamReference{}, err
	}
	if preflight.repository != record.repository ||
		preflight.workspace != record.workspace ||
		preflight.commonDir != record.commonDir ||
		preflight.statusDigest != record.statusDigest ||
		preflight.dirty != record.view.Dirty {
		return TeamReference{}, ErrTeamProposalStale
	}

	requestDigest, err := digestJSON(record.view.Request)
	if err != nil || requestDigest != record.requestDigest {
		return TeamReference{}, ErrTeamProposalStale
	}

	reference, aggregateCreated, err := r.createAdmittedTeam(
		operationCtx,
		record,
		confirmation.Admission,
	)
	activated = aggregateCreated
	lifecycleSettled = err == nil

	return reference, err
}

// ConfirmTeamNonInteractive always requires an explicit interactive decision.
func (r *Runtime) ConfirmTeamNonInteractive(
	context.Context,
	TeamConfirmation,
) (TeamReference, error) {
	return TeamReference{}, ErrTeamInteractionRequired
}

func (a *teamAdmission) take(id string) (teamProposalRecord, bool) {
	if a == nil {
		return teamProposalRecord{}, false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	record, found := a.proposals[id]
	if found {
		delete(a.proposals, id)
	}

	return record, found
}

func (a *teamAdmission) hasProposals() bool {
	if a == nil {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.proposals) != 0
}

//nolint:gocyclo // Preflight keeps every zero-resource admission gate explicit.
func (r *Runtime) preflightTeam(ctx context.Context, probeSandbox bool) (teamPreflight, error) {
	if err := ctx.Err(); err != nil {
		return teamPreflight{}, err
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return teamPreflight{}, stateError("Team preflight", phase, ErrRuntimeClosed)
	}
	if r.state.Mode != ModeAgent {
		r.mu.Unlock()

		return teamPreflight{}, fmt.Errorf("%w: Team admission requires Agent mode", ErrTeamAdmission)
	}
	if r.team != nil || r.teamGuard.active() {
		r.mu.Unlock()

		return teamPreflight{}, ErrTeamActive
	}
	workspaceValue := r.workspace
	inspector := r.inspector
	executor := r.executor
	options := r.opts
	parentSessionID := r.handle.Metadata().ID
	layout := r.paths
	r.mu.Unlock()

	active, err := activeParentTeamExists(ctx, layout.TeamResourcesDir(), parentSessionID)
	if err != nil {
		return teamPreflight{}, err
	}
	if active {
		return teamPreflight{}, ErrTeamActive
	}

	status, err := inspector.Status(ctx)
	if err != nil {
		return teamPreflight{}, fmt.Errorf("%w: inspect Workspace status: %w", ErrTeamAdmission, err)
	}

	if !status.Repository() || status.Branch().Unborn {
		return teamPreflight{}, fmt.Errorf(
			"%w: %w",
			ErrTeamAdmission,
			ErrTeamRepositoryRequired,
		)
	}

	runner, err := gitcontrol.New(options.GitPath, gitcontrol.Limits{
		OutputBytes: options.GitLimits.GitBytes,
		InputBytes:  options.GitLimits.CopyBytes,
		Timeout:     options.GitLimits.GitTimeout,
		TreeEntries: options.GitLimits.Files,
	})
	if err != nil {
		return teamPreflight{}, fmt.Errorf("%w: open trusted Git inspector: %w", ErrTeamAdmission, err)
	}
	repository, err := runner.InspectRepository(ctx, workspaceValue.Root())
	if err != nil {
		return teamPreflight{}, fmt.Errorf("%w: inspect repository: %w", ErrTeamAdmission, err)
	}
	if repository.TopLevel != workspaceValue.Root() || repository.HeadOID == "" {
		return teamPreflight{}, fmt.Errorf(
			"%w: %w",
			ErrTeamAdmission,
			ErrTeamRepositoryRequired,
		)
	}
	if err := validateTeamRoots(workspaceValue.Root(), repository.CommonDir, layout); err != nil {
		return teamPreflight{}, err
	}

	commonWorkspace, err := workspace.Open(repository.CommonDir)
	if err != nil {
		return teamPreflight{}, fmt.Errorf("%w: inspect repository common directory: %w", ErrTeamAdmission, err)
	}

	if status.Branch().OID != repository.HeadOID {
		return teamPreflight{}, fmt.Errorf("%w: repository status does not match HEAD", ErrTeamAdmission)
	}
	statusDigest, err := digestWorktreeStatus(status)
	if err != nil {
		return teamPreflight{}, err
	}

	if probeSandbox {
		probe := options.SandboxProbe
		if probe == nil {
			probe = func(probeCtx context.Context, value *execution.Executor) error {
				_, probeErr := value.Probe(probeCtx)

				return probeErr
			}
		}
		if err := probe(ctx, executor); err != nil {
			return teamPreflight{}, fmt.Errorf("%w: Workspace Write sandbox probe: %w", ErrTeamAdmission, err)
		}
	}

	return teamPreflight{
		repository: repository,
		workspace:  workspaceValue.Identity(), commonDir: commonWorkspace.Identity(),
		statusDigest: statusDigest, dirty: len(status.Entries()) != 0,
	}, nil
}

//nolint:funlen // The durable order mirrors the reviewed cross-store admission protocol.
func (r *Runtime) createAdmittedTeam(
	ctx context.Context,
	record teamProposalRecord,
	admission TeamAdmissionMode,
) (TeamReference, bool, error) {
	now := r.admission.now().UTC()
	members := make([]teamstate.MemberResource, 0, len(record.workers)+1)
	members = append(members, teamstate.MemberResource{
		MemberID: record.leadID, CapabilityProfileFingerprint: record.capabilityFingerprint,
	})
	for _, worker := range record.workers {
		members = append(members, teamstate.MemberResource{
			MemberID: worker.id, CapabilityProfileFingerprint: record.capabilityFingerprint,
		})
	}

	stateStore, err := teamstate.New(r.paths.TeamResourcesDir(), teamstate.Limits{})
	if err != nil {
		return TeamReference{}, false, err
	}
	resourceSnapshot := teamstate.Snapshot{
		TeamID: record.teamID, Revision: 1, State: teamstate.StateAdmitted,
		Parent: teamstate.ParentResource{
			SessionID: r.handle.Metadata().ID, WorkspaceID: record.workspace.Key(),
			Workspace: teamstate.FileIdentity{
				Path: record.workspace.Path(), Device: record.workspace.Device(), Inode: record.workspace.Inode(),
			},
		},
		Repository: teamstate.RepositoryResource{
			CommonDir: teamstate.FileIdentity{
				Path: record.commonDir.Path(), Device: record.commonDir.Device(), Inode: record.commonDir.Inode(),
			},
			BaseOID: record.repository.HeadOID, BranchRef: record.repository.BranchRef,
			Admission: teamstate.Admission(admission),
		},
		Members: members, Cleanup: teamstate.CleanupRetain,
		CreatedAt: now, UpdatedAt: now,
	}
	resourceSnapshot, err = stateStore.Commit(ctx, teamstate.Mutation{
		CommandID:        admissionCommandID(record.teamID, "resource-admit", ""),
		ExpectedRevision: 0, Snapshot: resourceSnapshot,
	})
	if err != nil {
		return TeamReference{}, false, err
	}

	aggregateStore, err := team.NewJSONLStore(r.paths.TeamAggregatesDir())
	if err != nil {
		return TeamReference{}, false, err
	}
	engine, err := team.New(aggregateStore)
	if err != nil {
		return TeamReference{}, false, err
	}
	controlStore, err := teamcontrol.New(r.paths.TeamControlDir(), teamcontrol.Limits{})
	if err != nil {
		return TeamReference{}, false, err
	}
	coordinator, err := newTeamCoordinator(
		record.teamID,
		record.leadID,
		engine,
		stateStore,
		nil,
		min(defaultWorkerLimit, len(record.workers)),
		min(defaultWorkerKeyCapacity, len(record.workers)),
	)
	if err != nil {
		return TeamReference{}, false, err
	}
	coordinator.lifecycle = r.publishTeamLifecycle
	coordinator.controlLifecycle = r.publishTeamControlLifecycle
	coordinator.control = controlStore
	actor := team.Actor{Kind: team.ActorKindCoordinator, ID: "coding-team-coordinator"}
	aggregate, err := engine.Create(ctx, team.CreateRequest{
		Command: team.CommandMetadata{
			ID: admissionCommandID(record.teamID, "create", ""), Actor: actor,
		},
		ID: record.teamID, Objective: record.view.Request.Objective,
		Lead: team.MemberSpec{ID: record.leadID, Name: "Lead", Role: "lead"},
		Limits: team.Limits{
			MaxMembers:     len(record.workers) + 1,
			MaxActiveTasks: min(maximumTeamWorkers, len(record.workers)),
		},
	})
	if err != nil {
		return TeamReference{}, false, err
	}

	r.mu.Lock()
	if r.team != nil {
		r.mu.Unlock()

		return TeamReference{}, true, ErrTeamActive
	}
	r.team = coordinator
	r.mu.Unlock()
	if err := r.teamGuard.activate(record.teamID); err != nil {
		return TeamReference{}, true, err
	}

	for _, worker := range record.workers {
		aggregate, err = engine.RegisterMember(ctx, record.teamID, team.RegisterMemberRequest{
			Command: team.CommandMetadata{
				ID:               admissionCommandID(record.teamID, "register", string(worker.id)),
				ExpectedRevision: aggregate.Revision, Actor: actor,
			},
			Member: team.MemberSpec{
				ID: worker.id, Name: worker.spec.Name, Role: worker.spec.Role,
				CapabilityProfileRef: record.capabilityFingerprint,
			},
		})
		if err != nil {
			return TeamReference{}, true, err
		}
	}

	workerIDs := make(map[string]team.MemberID, len(record.workers))
	for _, worker := range record.workers {
		workerIDs[strings.ToLower(worker.spec.Name)] = worker.id
	}
	for _, taskSpec := range record.tasks {
		taskID := team.TaskID(taskSpec.ID)
		dependencies := make([]team.TaskID, len(taskSpec.Dependencies))
		for index, dependency := range taskSpec.Dependencies {
			dependencies[index] = team.TaskID(dependency)
		}
		aggregate, err = engine.CreateTask(ctx, record.teamID, team.CreateTaskRequest{
			Command: team.CommandMetadata{
				ID:               admissionCommandID(record.teamID, "task", taskSpec.ID),
				ExpectedRevision: aggregate.Revision, Actor: actor,
			},
			TaskID: taskID, Title: taskSpec.Title, Description: taskSpec.Description,
			Dependencies: dependencies, AttemptLimit: defaultTeamTaskAttemptLimit,
		})
		if err != nil {
			return TeamReference{}, true, err
		}
		aggregate, err = engine.AssignTask(ctx, record.teamID, team.AssignTaskRequest{
			Command: team.CommandMetadata{
				ID:               admissionCommandID(record.teamID, "assign", taskSpec.ID),
				ExpectedRevision: aggregate.Revision, Actor: actor,
			},
			TaskID: taskID, MemberID: workerIDs[strings.ToLower(taskSpec.AssignedWorker)],
		})
		if err != nil {
			return TeamReference{}, true, err
		}
	}

	manager, err := teamworktree.New(teamworktree.Options{
		GitPath: r.opts.GitPath, ProductRoot: r.paths.Root(),
		WorktreesRoot: r.paths.WorktreesRoot(), LeasesRoot: r.paths.TeamLeasesDir(),
		Limits: teamworktree.DefaultLimits(),
	})
	if err != nil {
		return TeamReference{}, true, err
	}
	leaseGeneration := uint64(resourceSnapshot.Revision)
	lease, err := manager.Acquire(ctx, record.teamID, leaseGeneration)
	if err != nil {
		return TeamReference{}, true, err
	}
	coordinator.mu.Lock()
	coordinator.worktree = manager
	coordinator.lease = lease
	coordinator.mu.Unlock()

	resourceSnapshot.Revision++
	resourceSnapshot.State = teamstate.StateActive
	resourceSnapshot.UpdatedAt = r.admission.now().UTC()
	if _, err := stateStore.Commit(ctx, teamstate.Mutation{
		CommandID:        admissionCommandID(record.teamID, "resource-activate", ""),
		ExpectedRevision: resourceSnapshot.Revision - 1, Snapshot: resourceSnapshot,
	}); err != nil {
		return TeamReference{}, true, err
	}
	r.publishTeamLifecycle(ctx, TeamLifecycle{
		TeamID: record.teamID, State: TeamLifecycleAdmitted,
	})
	ownerFactory, err := newAttemptOwnerFactory(r, coordinator, leaseGeneration, nil)
	if err != nil {
		return TeamReference{}, true, err
	}
	coordinator.mu.Lock()
	coordinator.factory = ownerFactory
	coordinator.mu.Unlock()
	if err := coordinator.start(ctx); err != nil {
		return TeamReference{}, true, err
	}

	return TeamReference{
		TeamID: record.teamID, Admission: admission, BaseOID: record.repository.HeadOID,
		LeadMember: record.leadID,
	}, true, nil
}

func (r *Runtime) teamCapabilityFingerprint() (string, error) {
	data, err := json.Marshal(struct {
		Profile  string `json:"profile"`
		Model    any    `json:"model"`
		Sandbox  string `json:"sandbox"`
		Approval string `json:"approval"`
	}{
		Profile: "team-worker/v1", Model: r.resolved.Clone(),
		Sandbox: "workspace-write", Approval: "on-request",
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode capability profile: %w", ErrTeamAdmission, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func activeParentTeamExists(ctx context.Context, directory, parentSessionID string) (bool, error) {
	_, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect Team resources: %w", ErrTeamAdmission, err)
	}

	store, err := teamstate.New(directory, teamstate.Limits{})
	if err != nil {
		return false, err
	}
	values, err := store.ListByParent(ctx, parentSessionID, 100)
	if err != nil {
		return false, err
	}
	for _, value := range values {
		switch value.State {
		case teamstate.StateIntegrated, teamstate.StateClosedWithoutIntegration,
			teamstate.StateCancelled, teamstate.StateFailed:
		default:
			return true, nil
		}
	}

	return false, nil
}

func validateTeamRoots(workspaceRoot, commonDir string, layout paths.Layout) error {
	worktreesRoot := filepath.Clean(layout.WorktreesRoot())
	productRoot := filepath.Clean(layout.Root())
	for _, protected := range []string{workspaceRoot, commonDir, productRoot} {
		if pathsOverlap(worktreesRoot, filepath.Clean(protected)) {
			return fmt.Errorf("%w: Team Worktree root overlaps a protected root", ErrTeamAdmission)
		}
	}
	if pathsOverlap(layout.TeamLeasesDir(), worktreesRoot) {
		return fmt.Errorf("%w: Team lease and Worktree roots overlap", ErrTeamAdmission)
	}

	return nil
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(value, root string) bool {
	relative, err := filepath.Rel(root, value)

	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func digestWorktreeStatus(status changes.WorktreeStatus) (string, error) {
	return digestJSON(struct {
		Repository       bool                  `json:"repository"`
		Branch           changes.Branch        `json:"branch"`
		Entries          []changes.StatusEntry `json:"entries"`
		Staged           changes.DiffSection   `json:"staged"`
		Unstaged         changes.DiffSection   `json:"unstaged"`
		Untracked        changes.DiffSection   `json:"untracked"`
		ProtectedOmitted int                   `json:"protected_omitted"`
	}{
		status.Repository(), status.Branch(), status.Entries(), status.Staged(),
		status.Unstaged(), status.Untracked(), status.ProtectedOmitted(),
	})
}

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func validateAdmissionChoice(dirty bool, admission TeamAdmissionMode) error {
	if dirty {
		if admission != TeamAdmissionHEADOnly {
			return ErrTeamDirty
		}

		return nil
	}
	if admission != TeamAdmissionClean {
		return fmt.Errorf("%w: clean Workspace requires clean admission", ErrTeamAdmission)
	}

	return nil
}

//nolint:gocyclo // Proposal validation keeps graph, assignment, and text failures fail-closed.
func validateTeamProposalRequest(
	request TeamProposalRequest,
) (TeamProposalRequest, []admittedWorker, error) {
	normalized := cloneTeamProposalRequest(request)
	if !validTeamText(normalized.Objective, 16<<10, true) {
		return TeamProposalRequest{}, nil, fmt.Errorf("%w: invalid objective", ErrTeamAdmission)
	}
	if len(normalized.Workers) < 1 || len(normalized.Workers) > maximumTeamWorkers {
		return TeamProposalRequest{}, nil, fmt.Errorf(
			"%w: Team requires one to %d Workers", ErrTeamAdmission, maximumTeamWorkers,
		)
	}
	if len(normalized.Tasks) < 1 || len(normalized.Tasks) > maximumTeamTasks {
		return TeamProposalRequest{}, nil, fmt.Errorf(
			"%w: Team requires one to %d tasks", ErrTeamAdmission, maximumTeamTasks,
		)
	}

	workers := make([]admittedWorker, len(normalized.Workers))
	workerNames := make(map[string]struct{}, len(normalized.Workers))
	for index, spec := range normalized.Workers {
		if !validTeamText(spec.Name, 128, true) || !validTeamText(spec.Role, 4<<10, true) {
			return TeamProposalRequest{}, nil, fmt.Errorf("%w: invalid Worker %d", ErrTeamAdmission, index+1)
		}
		key := strings.ToLower(spec.Name)
		if _, duplicate := workerNames[key]; duplicate {
			return TeamProposalRequest{}, nil, fmt.Errorf("%w: duplicate Worker name", ErrTeamAdmission)
		}
		workerNames[key] = struct{}{}
		workers[index] = admittedWorker{spec: spec, id: derivedMemberID(index, spec.Name)}
	}

	tasks := make(map[string]TeamTaskSpec, len(normalized.Tasks))
	for index, task := range normalized.Tasks {
		if !teamAdmissionIDPattern.MatchString(task.ID) ||
			!validTeamText(task.Title, 1<<10, true) ||
			!validTeamText(task.Description, 32<<10, false) {
			return TeamProposalRequest{}, nil, fmt.Errorf("%w: invalid task %d", ErrTeamAdmission, index+1)
		}
		if _, duplicate := tasks[task.ID]; duplicate {
			return TeamProposalRequest{}, nil, fmt.Errorf("%w: duplicate task ID", ErrTeamAdmission)
		}
		if _, found := workerNames[strings.ToLower(task.AssignedWorker)]; !found {
			return TeamProposalRequest{}, nil, fmt.Errorf("%w: task %q has unknown Worker", ErrTeamAdmission, task.ID)
		}
		tasks[task.ID] = task
	}

	ordered, err := topologicalTeamTasks(tasks)
	if err != nil {
		return TeamProposalRequest{}, nil, err
	}
	normalized.Tasks = ordered

	return normalized, workers, nil
}

func topologicalTeamTasks(tasks map[string]TeamTaskSpec) ([]TeamTaskSpec, error) {
	indegree := make(map[string]int, len(tasks))
	dependents := make(map[string][]string, len(tasks))
	for id, task := range tasks {
		seen := make(map[string]struct{}, len(task.Dependencies))
		for _, dependency := range task.Dependencies {
			if dependency == id {
				return nil, fmt.Errorf("%w: task %q depends on itself", ErrTeamAdmission, id)
			}
			if _, found := tasks[dependency]; !found {
				return nil, fmt.Errorf("%w: task %q has unknown dependency %q", ErrTeamAdmission, id, dependency)
			}
			if _, duplicate := seen[dependency]; duplicate {
				return nil, fmt.Errorf("%w: task %q repeats dependency %q", ErrTeamAdmission, id, dependency)
			}
			seen[dependency] = struct{}{}
			indegree[id]++
			dependents[dependency] = append(dependents[dependency], id)
		}
		indegree[id] += 0
	}

	ready := make([]string, 0, len(tasks))
	for id, count := range indegree {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	ordered := make([]TeamTaskSpec, 0, len(tasks))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		task := tasks[id]
		task.Dependencies = slices.Clone(task.Dependencies)
		slices.Sort(task.Dependencies)
		ordered = append(ordered, task)

		next := slices.Clone(dependents[id])
		slices.Sort(next)
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				slices.Sort(ready)
			}
		}
	}
	if len(ordered) != len(tasks) {
		return nil, fmt.Errorf("%w: task dependency graph contains a cycle", ErrTeamAdmission)
	}

	return ordered, nil
}

func validTeamText(value string, maximum int, required bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return false
	}
	trimmed := strings.TrimSpace(value)
	if (required && trimmed == "") || (!required && value != "" && trimmed == "") {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\t' {
			return false
		}
	}

	return true
}

func randomTeamAdmissionID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}

	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func derivedMemberID(index int, name string) team.MemberID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", index, strings.ToLower(name))))

	return team.MemberID(fmt.Sprintf("worker-%02d-%s", index+1, hex.EncodeToString(sum[:8])))
}

func admissionCommandID(teamID team.ID, action, target string) team.CommandID {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(teamID), action, target}, "\x00")))

	return team.CommandID("admit-" + hex.EncodeToString(sum[:16]))
}

func cloneTeamProposal(value TeamProposal) TeamProposal {
	value.Request = cloneTeamProposalRequest(value.Request)

	return value
}

func cloneTeamProposalRecord(value teamProposalRecord) teamProposalRecord {
	value.view = cloneTeamProposal(value.view)
	value.workers = slices.Clone(value.workers)
	value.tasks = cloneTeamTasks(value.tasks)

	return value
}

func cloneTeamProposalRequest(value TeamProposalRequest) TeamProposalRequest {
	value.Workers = slices.Clone(value.Workers)
	value.Tasks = cloneTeamTasks(value.Tasks)

	return value
}

func cloneTeamTasks(values []TeamTaskSpec) []TeamTaskSpec {
	cloned := slices.Clone(values)
	for index := range cloned {
		cloned[index].Dependencies = slices.Clone(cloned[index].Dependencies)
	}

	return cloned
}
