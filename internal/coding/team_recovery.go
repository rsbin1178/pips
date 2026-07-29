package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	teamRecoveryListLimit       = 100
	teamRecoveryControlLimit    = 100
	teamRecoverySessionLimit    = 100
	teamRecoveryDiagnosticLimit = 16
)

// TeamRecoveryDisposition is the safe action available for one retained Team.
type TeamRecoveryDisposition string

// Team recovery dispositions. Only TeamRecoveryResume may be passed to the
// explicit resume path; blocked candidates require external repair or review.
const (
	TeamRecoveryResume          TeamRecoveryDisposition = "resume"
	TeamRecoveryBlockedIdentity TeamRecoveryDisposition = "blocked_identity"
	TeamRecoveryBlockedState    TeamRecoveryDisposition = "blocked_state"
)

// TeamRecoveryDiagnostic is a content-free recovery classification. Code is
// stable for UI decisions; it deliberately carries no path or transcript.
type TeamRecoveryDiagnostic struct {
	Code string
}

// TeamAttemptRecoveryCandidate is the bounded identity and durable state of a
// running Team Attempt discovered without opening its Worker Runtime.
type TeamAttemptRecoveryCandidate struct {
	TaskID            team.TaskID
	AttemptID         team.AttemptID
	MemberID          team.MemberID
	ContinuationID    continuation.ID
	SessionID         string
	ResourceState     teamstate.AttemptState
	ContinuationState continuation.Status
	ContinuationPhase continuation.Phase
	RetryWork         bool
	Diagnostic        string
}

// TeamRecoveryCandidate is the read-only startup projection for one retained
// Team owned by this Lead Session. It contains no filesystem paths or child
// transcript content.
type TeamRecoveryCandidate struct {
	TeamID             team.ID
	ResourceRevision   team.Revision
	ResourceState      teamstate.State
	TeamStatus         team.Status
	Disposition        TeamRecoveryDisposition
	Attempts           []TeamAttemptRecoveryCandidate
	PendingControls    int
	InterruptedControl int
	UpdatedAt          time.Time
	Diagnostics        []TeamRecoveryDiagnostic
}

// DiscoverTeamRecovery performs a fresh bounded, read-only reconciliation for
// this Lead Session. It never acquires a Team lease, repairs a continuation,
// opens a child Session, starts a scheduler, or invokes a model or Tool.
func (r *Runtime) DiscoverTeamRecovery(ctx context.Context) ([]TeamRecoveryCandidate, error) {
	if r == nil {
		return nil, ErrRuntimeInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.profile == profileTeamWorker {
		r.mu.Unlock()

		return nil, fmt.Errorf("%w: Team Workers cannot discover parent Teams", ErrTeamAdmission)
	}
	if r.closed {
		r.mu.Unlock()

		return nil, ErrRuntimeClosed
	}
	r.mu.Unlock()

	candidates, err := r.discoverTeamRecovery(ctx)
	r.mu.Lock()
	r.teamRecovery = cloneTeamRecoveryCandidates(candidates)
	r.teamRecoveryErr = err
	r.mu.Unlock()
	r.publishTeamRecoveryCandidates(ctx, candidates)

	return candidates, err
}

// TeamRecoveryCandidates returns the startup discovery snapshot. Call
// DiscoverTeamRecovery to refresh it after external state changes.
func (r *Runtime) TeamRecoveryCandidates() ([]TeamRecoveryCandidate, error) {
	if r == nil {
		return nil, ErrRuntimeInvalid
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return cloneTeamRecoveryCandidates(r.teamRecovery), r.teamRecoveryErr
}

//nolint:gocyclo,funlen // Discovery keeps every read-only identity edge explicit.
func (r *Runtime) discoverTeamRecovery(ctx context.Context) ([]TeamRecoveryCandidate, error) {
	if _, err := os.Stat(r.paths.TeamResourcesDir()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("coding Team recovery: inspect resource store: %w", err)
	}

	stateStore, err := teamstate.New(r.paths.TeamResourcesDir(), teamstate.Limits{})
	if err != nil {
		return nil, err
	}
	parentSessionID := r.handle.Metadata().ID
	snapshots, err := stateStore.ListByParent(ctx, parentSessionID, teamRecoveryListLimit)
	if err != nil {
		return nil, err
	}

	aggregateStore, aggregateErr := openExistingTeamStore(r.paths.TeamAggregatesDir())
	continuationStore, continuationErr := openExistingContinuationStore(r.paths.TeamContinuationsDir())
	controlStore, controlErr := openExistingControlStore(r.paths.TeamControlDir())
	manager, managerErr := r.openRetainedWorktreeManager()

	candidates := make([]TeamRecoveryCandidate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if teamstate.IsTerminal(snapshot.State) {
			continue
		}

		candidate := TeamRecoveryCandidate{
			TeamID: snapshot.TeamID, ResourceRevision: snapshot.Revision,
			ResourceState: snapshot.State, Disposition: TeamRecoveryResume,
			UpdatedAt: snapshot.UpdatedAt,
		}
		if !r.parentRecoveryIdentityMatches(ctx, snapshot) {
			candidate.blockIdentity("parent_identity_mismatch")
		}

		var aggregate team.Team
		if aggregateErr != nil {
			candidate.blockIdentity("aggregate_store_unavailable")
		} else {
			engine, engineErr := team.New(aggregateStore)
			if engineErr != nil {
				candidate.blockIdentity("aggregate_store_unavailable")
			} else if aggregate, engineErr = engine.Get(ctx, snapshot.TeamID); engineErr != nil {
				candidate.blockIdentity("aggregate_unavailable")
			} else {
				candidate.TeamStatus = aggregate.Status
				if aggregate.ID != snapshot.TeamID {
					candidate.blockIdentity("aggregate_identity_mismatch")
				} else if !recoveryAggregateMatchesResources(aggregate, snapshot) {
					candidate.blockIdentity("aggregate_resource_identity_mismatch")
				} else if aggregate.Status != team.StatusActive {
					candidate.blockState("aggregate_terminal")
				}
			}
		}

		if !recoverableTeamResourceState(snapshot.State) {
			candidate.blockState("resource_state_not_resumable")
		}
		workers, workersErr := r.repository.ListTeamWorkers(
			ctx,
			parentSessionID,
			snapshot.TeamID,
			teamRecoverySessionLimit,
		)
		if workersErr != nil {
			candidate.blockIdentity("worker_sessions_unavailable")
		}

		if aggregate.ID != "" {
			candidate.Attempts = classifyRecoveryAttempts(
				ctx,
				snapshot,
				aggregate,
				workers,
				continuationStore,
				continuationErr,
				manager,
				managerErr,
				&candidate,
			)
		}
		if controlErr != nil && !errors.Is(controlErr, os.ErrNotExist) {
			candidate.blockState("control_store_unavailable")
		} else if controlStore != nil {
			pending, pendingErr := controlStore.Pending(ctx, snapshot.TeamID, teamRecoveryControlLimit)
			if pendingErr != nil && !errors.Is(pendingErr, teamcontrol.ErrNotFound) {
				candidate.blockState("control_journal_unavailable")
			} else {
				candidate.PendingControls = len(pending)
				for _, record := range pending {
					if record.Entry.State == teamcontrol.StateApplying {
						candidate.InterruptedControl++
					}
				}
			}
		}

		candidates = append(candidates, candidate)
	}

	slices.SortFunc(candidates, func(left, right TeamRecoveryCandidate) int {
		if order := right.UpdatedAt.Compare(left.UpdatedAt); order != 0 {
			return order
		}

		return strings.Compare(string(left.TeamID), string(right.TeamID))
	})

	return candidates, nil
}

func classifyRecoveryAttempts(
	ctx context.Context,
	snapshot teamstate.Snapshot,
	aggregate team.Team,
	workers []session.Metadata,
	continuations *continuation.JSONLStore,
	continuationErr error,
	manager *teamworktree.Manager,
	managerErr error,
	candidate *TeamRecoveryCandidate,
) []TeamAttemptRecoveryCandidate {
	resources := make(map[team.AttemptID]teamstate.AttemptResource, len(snapshot.Attempts))
	for _, resource := range snapshot.Attempts {
		resources[resource.AttemptID] = resource
	}
	workerByID := make(map[string][]session.Metadata, len(workers))
	for _, worker := range workers {
		workerByID[worker.ID] = append(workerByID[worker.ID], worker)
	}

	attempts := make([]TeamAttemptRecoveryCandidate, 0)
	for _, taskValue := range aggregate.Tasks {
		if taskValue.Status != team.TaskStatusRunning || len(taskValue.Attempts) == 0 {
			continue
		}
		domainAttempt := taskValue.Attempts[len(taskValue.Attempts)-1]
		if domainAttempt.Status != team.AttemptStatusRunning {
			candidate.blockIdentity("attempt_domain_mismatch")
			continue
		}
		value := TeamAttemptRecoveryCandidate{
			TaskID: taskValue.ID, AttemptID: domainAttempt.ID,
			MemberID: domainAttempt.MemberID, ContinuationID: domainAttempt.ContinuationID,
		}
		resource, found := resources[domainAttempt.ID]
		if found {
			value.ResourceState = resource.State
			value.SessionID = resource.Session.SessionID
			if resource.TaskID != taskValue.ID || resource.MemberID != domainAttempt.MemberID ||
				resource.ContinuationID != domainAttempt.ContinuationID {
				value.Diagnostic = "attempt_resource_identity_mismatch"
				candidate.blockIdentity(value.Diagnostic)
			}
			if resource.Session.SessionID != "" && !exactRecoveryWorkerSession(
				workerByID[resource.Session.SessionID],
				snapshot,
				resource,
			) {
				value.Diagnostic = "worker_session_identity_mismatch"
				candidate.blockIdentity(value.Diagnostic)
			}
			if resource.Worktree != (teamstate.WorktreeResource{}) {
				if managerErr != nil || manager == nil {
					value.Diagnostic = "worktree_manager_unavailable"
					candidate.blockIdentity(value.Diagnostic)
				} else {
					owner := teamworktree.Owner{
						TeamID: snapshot.TeamID, MemberID: resource.MemberID,
						AttemptID:       resource.AttemptID,
						LeaseGeneration: resource.Worktree.LeaseGeneration,
					}
					retained := worktreeResourceFromState(owner, resource.Worktree)
					if _, inspectErr := manager.InspectRetained(ctx, retained); inspectErr != nil {
						value.Diagnostic = "worktree_identity_mismatch"
						candidate.blockIdentity(value.Diagnostic)
					}
				}
			}
		}

		if continuationErr != nil || continuations == nil {
			// A crash after the Team Attempt start and before continuation
			// creation is recoverable. The explicit owner will create it.
			if found && resource.State != teamstate.AttemptPlanned &&
				resource.State != teamstate.AttemptBasePrepared &&
				resource.State != teamstate.AttemptWorktreeReady &&
				resource.State != teamstate.AttemptSessionReady {
				value.Diagnostic = "continuation_store_unavailable"
				candidate.blockState(value.Diagnostic)
			}
		} else {
			record, loadErr := continuations.Load(ctx, domainAttempt.ContinuationID)
			if loadErr == nil {
				execution := record.Execution
				value.ContinuationState = execution.Status
				value.ContinuationPhase = execution.Phase
				if execution.ID != domainAttempt.ContinuationID ||
					execution.Target.Kind != attemptTargetKind ||
					execution.Target.ID != string(domainAttempt.ID) ||
					execution.Worker != attemptWorkerRef ||
					execution.Controller != team.DefaultAttemptControllerRef() {
					value.Diagnostic = "continuation_identity_mismatch"
					candidate.blockIdentity(value.Diagnostic)
				} else {
					value.RetryWork = recoveryNeedsWorkRetry(execution)
					if !recoverableContinuationState(execution) {
						value.Diagnostic = "continuation_state_not_resumable"
						candidate.blockState(value.Diagnostic)
					}
				}
			} else if !errors.Is(loadErr, continuation.ErrNotFound) {
				value.Diagnostic = "continuation_unavailable"
				candidate.blockIdentity(value.Diagnostic)
			}
		}

		attempts = append(attempts, value)
	}

	return attempts
}

func exactRecoveryWorkerSession(
	values []session.Metadata,
	snapshot teamstate.Snapshot,
	resource teamstate.AttemptResource,
) bool {
	if len(values) != 1 {
		return false
	}
	value := values[0]
	lineage := value.TeamWorker

	return value.Kind == session.KindTeamWorker &&
		value.WorkspaceID == resource.Session.WorkspaceID &&
		lineage.ParentSessionID == snapshot.Parent.SessionID &&
		lineage.TeamID == snapshot.TeamID && lineage.MemberID == resource.MemberID &&
		lineage.TaskID == resource.TaskID && lineage.AttemptID == resource.AttemptID &&
		lineage.ContinuationID == resource.ContinuationID
}

func recoveryNeedsWorkRetry(execution continuation.Execution) bool {
	if execution.Status == continuation.StatusInterrupted && execution.Phase == continuation.PhaseWork {
		return true
	}

	return execution.Status == continuation.StatusPaused && execution.Suspension != nil &&
		execution.Suspension.Phase == continuation.PhaseWork && execution.Suspension.RetryRequired
}

func recoverableContinuationState(execution continuation.Execution) bool {
	if execution.Status.Terminal() || recoveryNeedsWorkRetry(execution) ||
		recoveryNeedsDecisionRetry(execution) {
		return true
	}
	switch execution.Status {
	case continuation.StatusReady, continuation.StatusRunning,
		continuation.StatusPauseRequested, continuation.StatusPaused,
		continuation.StatusCancelRequested:
		return true
	default:
		return false
	}
}

func recoveryNeedsDecisionRetry(execution continuation.Execution) bool {
	return execution.Status == continuation.StatusInterrupted &&
		execution.Phase == continuation.PhaseDecision
}

func (r *Runtime) parentRecoveryIdentityMatches(
	ctx context.Context,
	snapshot teamstate.Snapshot,
) bool {
	identity := r.workspace.Identity()
	if snapshot.Parent.SessionID != r.handle.Metadata().ID ||
		snapshot.Parent.WorkspaceID != identity.Key() ||
		snapshot.Parent.Workspace.Path != identity.Path() ||
		snapshot.Parent.Workspace.Device != identity.Device() ||
		snapshot.Parent.Workspace.Inode != identity.Inode() {
		return false
	}
	common, err := workspace.Open(snapshot.Repository.CommonDir.Path)
	if err != nil {
		return false
	}
	commonIdentity := common.Identity()
	if commonIdentity.Path() != snapshot.Repository.CommonDir.Path ||
		commonIdentity.Device() != snapshot.Repository.CommonDir.Device ||
		commonIdentity.Inode() != snapshot.Repository.CommonDir.Inode {
		return false
	}
	runner, err := gitcontrol.New(r.opts.GitPath, gitcontrol.Limits{
		OutputBytes: r.opts.GitLimits.GitBytes,
		InputBytes:  r.opts.GitLimits.CopyBytes,
		Timeout:     r.opts.GitLimits.GitTimeout,
		TreeEntries: r.opts.GitLimits.Files,
	})
	if err != nil {
		return false
	}
	repository, err := runner.InspectRepository(ctx, r.workspace.Root())

	return err == nil && repository.TopLevel == r.workspace.Root() &&
		repository.CommonDir == snapshot.Repository.CommonDir.Path &&
		repository.HeadOID == snapshot.Repository.BaseOID
}

func (r *Runtime) openRetainedWorktreeManager() (*teamworktree.Manager, error) {
	for _, directory := range []string{r.paths.WorktreesRoot(), r.paths.TeamLeasesDir()} {
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("recovery root is not a directory")
		}
	}

	return teamworktree.New(teamworktree.Options{
		GitPath: r.opts.GitPath, ProductRoot: r.paths.Root(),
		WorktreesRoot: r.paths.WorktreesRoot(), LeasesRoot: r.paths.TeamLeasesDir(),
		Limits: teamworktree.DefaultLimits(),
	})
}

func openExistingTeamStore(directory string) (*team.JSONLStore, error) {
	if _, err := os.Stat(directory); err != nil {
		return nil, err
	}

	return team.NewJSONLStore(directory)
}

func openExistingContinuationStore(directory string) (*continuation.JSONLStore, error) {
	if _, err := os.Stat(directory); err != nil {
		return nil, err
	}

	return continuation.NewJSONLStore(directory)
}

func openExistingControlStore(directory string) (*teamcontrol.Store, error) {
	if _, err := os.Stat(directory); err != nil {
		return nil, err
	}

	return teamcontrol.New(directory, teamcontrol.Limits{})
}

func recoverableTeamResourceState(state teamstate.State) bool {
	switch state {
	case teamstate.StateAdmitted, teamstate.StateProvisioning,
		teamstate.StateActive, teamstate.StateInterrupted:
		return true
	default:
		return false
	}
}

func (c *TeamRecoveryCandidate) blockIdentity(code string) {
	if c.Disposition != TeamRecoveryBlockedIdentity {
		c.Disposition = TeamRecoveryBlockedIdentity
	}
	c.addDiagnostic(code)
}

func (c *TeamRecoveryCandidate) blockState(code string) {
	if c.Disposition == TeamRecoveryResume {
		c.Disposition = TeamRecoveryBlockedState
	}
	c.addDiagnostic(code)
}

func (c *TeamRecoveryCandidate) addDiagnostic(code string) {
	if code == "" || len(c.Diagnostics) >= teamRecoveryDiagnosticLimit {
		return
	}
	for _, value := range c.Diagnostics {
		if value.Code == code {
			return
		}
	}
	c.Diagnostics = append(c.Diagnostics, TeamRecoveryDiagnostic{Code: code})
}

func cloneTeamRecoveryCandidates(values []TeamRecoveryCandidate) []TeamRecoveryCandidate {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]TeamRecoveryCandidate, len(values))
	copy(cloned, values)
	for index := range cloned {
		cloned[index].Attempts = slices.Clone(values[index].Attempts)
		cloned[index].Diagnostics = slices.Clone(values[index].Diagnostics)
	}

	return cloned
}
