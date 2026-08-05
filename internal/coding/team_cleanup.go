//nolint:wsl_v5 // Cleanup keeps each exact resource mutation beside its durable classification.
package coding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamintegration"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/rsbin/pips/internal/coding/workspace"
)

var (
	// ErrTeamCleanupUnavailable means there is no exact active Team cleanup owner.
	ErrTeamCleanupUnavailable = errors.New("coding Team cleanup unavailable")
	// ErrTeamCleanupStale means the reviewed Team resource revision changed.
	ErrTeamCleanupStale = errors.New("coding Team cleanup review is stale")
	// ErrTeamCleanupIntegrationRequired means Integration must be applied, recovered, or rejected first.
	ErrTeamCleanupIntegrationRequired = errors.New("coding Team cleanup requires an Integration decision")
	// ErrTeamCleanupRetained means one or more exact resources were deliberately preserved.
	ErrTeamCleanupRetained = errors.New("coding Team cleanup retained resources")
)

// TeamCleanupRequest binds one explicit cleanup decision to current durable resources.
type TeamCleanupRequest struct {
	TeamID                   team.ID
	ExpectedResourceRevision team.Revision
	CloseWithoutIntegration  bool
}

// TeamCleanupResult is a path-free cleanup projection suitable for frontends.
type TeamCleanupResult struct {
	TeamID   team.ID
	State    teamstate.State
	Cleaned  int
	Retained int
}

// CleanupTeam performs non-force cleanup of exact terminal Team artifacts.
// Captured Attempt result refs are retained intentionally.
//
//nolint:funlen,gocyclo // The transaction makes every retained-evidence branch explicit.
func (r *Runtime) CleanupTeam(
	ctx context.Context,
	request TeamCleanupRequest,
) (TeamCleanupResult, error) {
	if r == nil || r.isTeamWorker() || request.TeamID == "" ||
		request.ExpectedResourceRevision == 0 {
		return TeamCleanupResult{}, ErrTeamCleanupUnavailable
	}
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamCleanup, runtimeResolution{}, nil,
	)
	if err != nil {
		return TeamCleanupResult{}, err
	}
	defer r.endOperation(operation)

	coordinator, err := r.currentTeamCoordinator()
	if err != nil || coordinator.id != request.TeamID {
		return TeamCleanupResult{}, errors.Join(ErrTeamCleanupUnavailable, err)
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()

	aggregate, err := coordinator.engine.Get(operationCtx, coordinator.id)
	if err != nil {
		return TeamCleanupResult{}, err
	}
	resources, err := coordinator.state.Load(operationCtx, coordinator.id)
	if err != nil {
		return TeamCleanupResult{}, err
	}
	if resources.Revision != request.ExpectedResourceRevision {
		return TeamCleanupResult{}, ErrTeamCleanupStale
	}
	if coordinator.ownerCount() != 0 {
		return TeamCleanupResult{}, ErrTeamCleanupUnavailable
	}
	if err := validateTeamCleanupDecision(aggregate, resources, request.CloseWithoutIntegration); err != nil {
		return TeamCleanupResult{}, err
	}

	resources, err = beginTeamCleanup(operationCtx, coordinator.state, resources)
	if err != nil {
		return TeamCleanupResult{}, err
	}
	failures := make([]error, 0)
	persistenceCtx := context.WithoutCancel(operationCtx)
	for _, attempt := range resources.Attempts {
		if attempt.Cleanup != teamstate.CleanupPending {
			continue
		}
		var cleanupErr error
		if attempt.Base.OwnedRef != "" {
			manager, managerErr := r.integrationManager(coordinator)
			cleanupErr = managerErr
			if cleanupErr == nil {
				cleanupErr = manager.CleanupAttemptBase(
					operationCtx,
					teamintegration.AttemptBaseCleanupRequest{
						Workspace: resources.Parent.Workspace.Path,
						TeamID:    string(coordinator.id), AttemptID: string(attempt.AttemptID),
						Ref: attempt.Base.OwnedRef, CommitOID: attempt.Base.OID,
					},
				)
			}
		}
		if cleanupErr == nil && attempt.Worktree != (teamstate.WorktreeResource{}) {
			owner := teamworktree.Owner{
				TeamID: coordinator.id, MemberID: attempt.MemberID, AttemptID: attempt.AttemptID,
				LeaseGeneration: attempt.Worktree.LeaseGeneration,
			}
			resource := worktreeResourceFromState(owner, attempt.Worktree)
			_, cleanupErr = coordinator.worktree.Cleanup(
				operationCtx, coordinator.lease, resource,
			)
		}
		class := classifyAttemptCleanup(cleanupErr)
		if cleanupErr != nil {
			failures = append(failures, cleanupErr)
		}
		resources, err = commitTeamCleanupClass(
			persistenceCtx, coordinator.state, coordinator.id,
			"attempt", string(attempt.AttemptID), class,
		)
		if err != nil {
			return teamCleanupResult(resources), errors.Join(ErrTeamCleanupRetained, err)
		}
	}

	manager, managerErr := r.integrationManagerForCleanup(coordinator, resources)
	if managerErr != nil {
		return teamCleanupResult(resources), errors.Join(ErrTeamCleanupRetained, managerErr)
	}
	for _, integration := range resources.Integrations {
		if integration.Cleanup != teamstate.CleanupPending {
			continue
		}
		resource, resourceErr := integrationResourceFromTeamState(integration.Worktree)
		cleanupErr := resourceErr
		if cleanupErr == nil {
			_, cleanupErr = manager.Cleanup(operationCtx, teamintegration.CleanupRequest{
				TeamID: string(coordinator.id), Resource: resource,
			})
		}
		class := classifyIntegrationCleanup(cleanupErr)
		if cleanupErr != nil {
			failures = append(failures, cleanupErr)
		}
		resources, err = commitTeamCleanupClass(
			persistenceCtx, coordinator.state, coordinator.id,
			"integration", integration.ID, class,
		)
		if err != nil {
			return teamCleanupResult(resources), errors.Join(ErrTeamCleanupRetained, err)
		}
	}

	resources, complete, err := finishTeamCleanup(
		persistenceCtx, coordinator.state, coordinator.id, request.CloseWithoutIntegration,
	)
	result := teamCleanupResult(resources)
	if err != nil {
		return result, errors.Join(ErrTeamCleanupRetained, err)
	}
	if !complete {
		return result, errors.Join(append([]error{ErrTeamCleanupRetained}, failures...)...)
	}
	if !r.teamGuard.beginClose(coordinator.id) {
		return result, ErrTeamCleanupUnavailable
	}
	if err := coordinator.close(operationCtx); err != nil {
		return result, err
	}
	r.mu.Lock()
	if r.team == coordinator {
		r.team = nil
	}
	r.mu.Unlock()
	r.teamGuard.deactivate(coordinator.id)

	return result, nil
}

func validateTeamCleanupDecision(
	aggregate team.Team,
	resources teamstate.Snapshot,
	closeWithoutIntegration bool,
) error {
	switch aggregate.Status {
	case team.StatusCompleted, team.StatusFailed, team.StatusCancelled:
	case team.StatusActive:
		for _, taskValue := range aggregate.Tasks {
			switch taskValue.Status {
			case team.TaskStatusCompleted, team.TaskStatusFailed, team.TaskStatusCancelled:
			default:
				return ErrTeamCleanupUnavailable
			}
		}
	default:
		return ErrTeamCleanupUnavailable
	}
	if resources.State == teamstate.StateIntegrated {
		if closeWithoutIntegration {
			return fmt.Errorf("%w: Team already integrated", ErrTeamCleanupStale)
		}

		return nil
	}
	if !closeWithoutIntegration {
		return ErrTeamCleanupIntegrationRequired
	}
	for _, integration := range resources.Integrations {
		switch integration.State {
		case teamstate.IntegrationRetained, teamstate.IntegrationRolledBack,
			teamstate.IntegrationFailed, teamstate.IntegrationConflict:
		default:
			return ErrTeamCleanupIntegrationRequired
		}
	}

	return nil
}

func beginTeamCleanup(
	ctx context.Context,
	store *teamstate.Store,
	current teamstate.Snapshot,
) (teamstate.Snapshot, error) {
	next := cloneTeamResourceSnapshot(current)
	for index := range next.Attempts {
		attempt := &next.Attempts[index]
		if attempt.Worktree == (teamstate.WorktreeResource{}) && attempt.Base.OwnedRef == "" {
			attempt.Cleanup = teamstate.CleanupComplete
		} else if cleanupAttemptState(attempt.State) && attempt.Cleanup != teamstate.CleanupComplete {
			attempt.Cleanup = teamstate.CleanupPending
		}
	}
	for index := range next.Integrations {
		integration := &next.Integrations[index]
		if integration.Worktree == (teamstate.WorktreeResource{}) {
			integration.Cleanup = teamstate.CleanupComplete
		} else if cleanupIntegrationState(integration.State) &&
			integration.Cleanup != teamstate.CleanupComplete {
			integration.Cleanup = teamstate.CleanupPending
		}
	}
	next.Cleanup = teamstate.CleanupPending

	return commitExactTeamCleanup(ctx, store, current, next, "begin", "")
}

func cleanupAttemptState(state teamstate.AttemptState) bool {
	switch state {
	case teamstate.AttemptCaptured, teamstate.AttemptTerminal, teamstate.AttemptFailed,
		teamstate.AttemptCancelled, teamstate.AttemptConflicted, teamstate.AttemptCaptureFailed:
		return true
	default:
		return false
	}
}

func cleanupIntegrationState(state teamstate.IntegrationState) bool {
	switch state {
	case teamstate.IntegrationApplied, teamstate.IntegrationRolledBack,
		teamstate.IntegrationRetained, teamstate.IntegrationFailed,
		teamstate.IntegrationConflict:
		return true
	default:
		return false
	}
}

func (r *Runtime) integrationManagerForCleanup(
	coordinator *teamCoordinator,
	resources teamstate.Snapshot,
) (*teamintegration.Manager, error) {
	for _, integration := range resources.Integrations {
		if integration.Cleanup == teamstate.CleanupPending {
			return r.integrationManager(coordinator)
		}
	}

	return nil, nil
}

func classifyAttemptCleanup(err error) teamstate.CleanupClass {
	if err == nil {
		return teamstate.CleanupComplete
	}
	if errors.Is(err, teamworktree.ErrIdentity) || errors.Is(err, teamworktree.ErrLeaseLost) ||
		errors.Is(err, teamworktree.ErrInvalid) || errors.Is(err, teamintegration.ErrStale) ||
		errors.Is(err, teamintegration.ErrInvalid) {
		return teamstate.CleanupOrphaned
	}

	return teamstate.CleanupRetain
}

func classifyIntegrationCleanup(err error) teamstate.CleanupClass {
	if err == nil {
		return teamstate.CleanupComplete
	}
	if errors.Is(err, teamintegration.ErrStale) || errors.Is(err, teamintegration.ErrInvalid) ||
		errors.Is(err, workspace.ErrChanged) || errors.Is(err, workspace.ErrInvalid) {
		return teamstate.CleanupOrphaned
	}

	return teamstate.CleanupRetain
}

func commitTeamCleanupClass(
	ctx context.Context,
	store *teamstate.Store,
	teamID team.ID,
	kind, id string,
	class teamstate.CleanupClass,
) (teamstate.Snapshot, error) {
	current, err := store.Load(ctx, teamID)
	if err != nil {
		return teamstate.Snapshot{}, err
	}
	next := cloneTeamResourceSnapshot(current)
	switch kind {
	case "attempt":
		index := attemptResourceIndex(next.Attempts, team.AttemptID(id))
		if index < 0 || next.Attempts[index].Cleanup != teamstate.CleanupPending {
			return current, ErrTeamCleanupStale
		}
		next.Attempts[index].Cleanup = class
	case "integration":
		index := integrationResourceIndex(next.Integrations, id)
		if index < 0 || next.Integrations[index].Cleanup != teamstate.CleanupPending {
			return current, ErrTeamCleanupStale
		}
		next.Integrations[index].Cleanup = class
	default:
		return current, ErrTeamCleanupUnavailable
	}

	return commitExactTeamCleanup(ctx, store, current, next, kind, id)
}

func finishTeamCleanup(
	ctx context.Context,
	store *teamstate.Store,
	teamID team.ID,
	closeWithoutIntegration bool,
) (teamstate.Snapshot, bool, error) {
	current, err := store.Load(ctx, teamID)
	if err != nil {
		return teamstate.Snapshot{}, false, err
	}
	complete := true
	for _, attempt := range current.Attempts {
		complete = complete && attempt.Cleanup == teamstate.CleanupComplete
	}
	for _, integration := range current.Integrations {
		complete = complete && integration.Cleanup == teamstate.CleanupComplete
	}
	next := cloneTeamResourceSnapshot(current)
	if complete {
		next.Cleanup = teamstate.CleanupComplete
		if closeWithoutIntegration {
			next.State = teamstate.StateClosedWithoutIntegration
		}
	} else {
		next.Cleanup = teamstate.CleanupRetain
	}
	committed, err := commitExactTeamCleanup(ctx, store, current, next, "finish", "")

	return committed, complete, err
}

func commitExactTeamCleanup(
	ctx context.Context,
	store *teamstate.Store,
	current, next teamstate.Snapshot,
	action, target string,
) (teamstate.Snapshot, error) {
	next.Revision = current.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	committed, err := store.Commit(ctx, teamstate.Mutation{
		CommandID: admissionCommandID(
			current.TeamID, fmt.Sprintf("cleanup-%s-%d", action, current.Revision), target,
		),
		ExpectedRevision: current.Revision,
		Snapshot:         next,
	})
	if errors.Is(err, teamstate.ErrConflict) {
		return teamstate.Snapshot{}, ErrTeamCleanupStale
	}

	return committed, err
}

func teamCleanupResult(snapshot teamstate.Snapshot) TeamCleanupResult {
	result := TeamCleanupResult{TeamID: snapshot.TeamID, State: snapshot.State}
	for _, attempt := range snapshot.Attempts {
		addTeamCleanupResult(&result, attempt.Cleanup)
	}
	for _, integration := range snapshot.Integrations {
		addTeamCleanupResult(&result, integration.Cleanup)
	}

	return result
}

func addTeamCleanupResult(result *TeamCleanupResult, class teamstate.CleanupClass) {
	if class == teamstate.CleanupComplete {
		result.Cleaned++
	} else {
		result.Retained++
	}
}

func integrationResourceFromTeamState(
	value teamstate.WorktreeResource,
) (teamintegration.Resource, error) {
	openIdentity := func(expected teamstate.FileIdentity) (workspace.Identity, error) {
		opened, err := workspace.Open(expected.Path)
		if err != nil {
			return workspace.Identity{}, err
		}
		identity := opened.Identity()
		if identity.Device() != expected.Device || identity.Inode() != expected.Inode {
			return workspace.Identity{}, workspace.ErrChanged
		}

		return identity, nil
	}
	workspaceIdentity, err := openIdentity(value.Workspace)
	if err != nil {
		return teamintegration.Resource{}, err
	}
	directory, err := openIdentity(value.Directory)
	if err != nil {
		return teamintegration.Resource{}, err
	}
	gitDir, err := openIdentity(value.GitDir)
	if err != nil {
		return teamintegration.Resource{}, err
	}
	commonDir, err := openIdentity(value.CommonDir)
	if err != nil {
		return teamintegration.Resource{}, err
	}

	return teamintegration.Resource{
		ID: value.ID, Workspace: workspaceIdentity, Directory: directory,
		GitDir: gitDir, CommonDir: commonDir, ObjectFormat: value.ObjectFormat,
		BranchRef: value.BranchRef, IntegrationRef: value.ResultRef,
		BaseOID: value.BaseOID, CommitOID: value.ResultCommitOID,
		LockReason: value.LockReason,
	}, nil
}
