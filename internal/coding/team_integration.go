//nolint:wsl_v5 // Integration selection, approval, apply, and recovery stay in transaction order.
package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/teamintegration"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

var (
	// ErrTeamIntegrationUnavailable means there is no admitted parent Team.
	ErrTeamIntegrationUnavailable = errors.New("coding Team integration unavailable")
	// ErrTeamIntegrationSelection means selected Tasks do not have one exact captured result closure.
	ErrTeamIntegrationSelection = errors.New("coding Team integration selection is invalid")
)

// TeamIntegrationRequest selects current captured results. Empty TaskIDs selects
// every completed Task. Verification is optional and runs in the isolated Worktree.
type TeamIntegrationRequest struct {
	TaskIDs      []team.TaskID
	Verification *execution.OperationSpec
}

// TeamIntegrationPreview is UI-neutral approval evidence.
type TeamIntegrationPreview struct {
	ID                      string
	AttemptIDs              []team.AttemptID
	ResourceRevision        team.Revision
	CommitOID               string
	TreeOID                 string
	Manifest                teamintegration.Manifest
	DiffDigest              string
	Verification            teamintegration.Verification
	ApprovalToken           string
	ApprovalTokenHash       string
	ExpiresAt               time.Time
	ProducesUnstagedChanges bool
}

// TeamIntegrationApproval binds Apply to the preview the user reviewed.
type TeamIntegrationApproval struct {
	ID    string
	Token string
}

// TeamIntegrationResult reports a terminal integration transaction.
type TeamIntegrationResult struct {
	ID             string
	State          string
	ManifestDigest string
	Files          int
}

// TeamIntegrationRecovery is one read-only interrupted journal projection.
type TeamIntegrationRecovery = teamintegration.RecoveryCandidate

// TeamIntegrationRecoveryRequest selects explicit recovery convergence.
type TeamIntegrationRecoveryRequest struct {
	ID     string
	Action teamintegration.RecoveryAction
}

// PrepareTeamIntegration deterministically composes selected captured results.
//
//nolint:funlen,gocyclo,nestif // Preparation keeps selection, artifact, persistence, and lifecycle convergence together.
func (r *Runtime) PrepareTeamIntegration(
	ctx context.Context,
	request TeamIntegrationRequest,
) (TeamIntegrationPreview, error) {
	if r == nil || r.isTeamWorker() {
		return TeamIntegrationPreview{}, ErrTeamIntegrationUnavailable
	}
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamIntegration, runtimeResolution{}, nil,
	)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	defer r.endOperation(operation)

	coordinator, err := r.currentTeamCoordinator()
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()
	manager, err := r.integrationManager(coordinator)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	aggregate, err := coordinator.engine.Get(operationCtx, coordinator.id)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	resources, err := coordinator.state.Load(operationCtx, coordinator.id)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	selection, err := buildIntegrationSelection(aggregate, resources, request.TaskIDs)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}

	var verifier teamintegration.Verifier
	if request.Verification != nil {
		verifier = runtimeIntegrationVerifier{runtime: r, spec: *request.Verification}
	}
	prepared, err := manager.Prepare(operationCtx, teamintegration.PrepareRequest{
		Workspace: r.workspace.Root(), Selection: selection, Verifier: verifier,
	})
	if err != nil {
		conflict := new(teamintegration.ConflictError)
		if !errors.As(err, &conflict) {
			retained := new(teamintegration.RetainedError)
			if !errors.As(err, &retained) || prepared.ID == "" {
				return TeamIntegrationPreview{}, err
			}

			attemptIDs := make([]team.AttemptID, len(prepared.AttemptIDs))
			for index, attemptID := range prepared.AttemptIDs {
				attemptIDs[index] = team.AttemptID(attemptID)
			}
			_, persistErr := mutateIntegrationResource(
				context.WithoutCancel(operationCtx), coordinator.state, coordinator.id,
				prepared.ID, "prepare-retained",
				func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
					value.ID = prepared.ID
					value.AttemptIDs = slices.Clone(attemptIDs)
					value.ResourceRevision = team.Revision(selection.ResourceRevision)
					value.State = teamstate.IntegrationInterrupted
					value.TreeOID = prepared.TreeOID
					value.CommitOID = prepared.CommitOID
					value.Cleanup = teamstate.CleanupRetain
					snapshot.State = teamstate.StateInterrupted

					return nil
				},
			)
			if persistErr == nil {
				r.publishTeamIntegrationLifecycle(
					context.WithoutCancel(operationCtx),
					TeamIntegrationLifecycle{
						TeamID: coordinator.id, IntegrationID: prepared.ID,
						State: TeamIntegrationInterrupted, Attempts: len(prepared.AttemptIDs),
						Code: "artifact_retained",
					},
				)
			}

			return TeamIntegrationPreview{
				ID: prepared.ID, AttemptIDs: attemptIDs,
				ResourceRevision: team.Revision(selection.ResourceRevision),
				CommitOID:        prepared.CommitOID, TreeOID: prepared.TreeOID,
				ProducesUnstagedChanges: true,
			}, errors.Join(err, persistErr)
		}
		_, persistErr := mutateIntegrationResource(
			operationCtx, coordinator.state, coordinator.id, conflict.IntegrationID, "conflict",
			func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
				if uint64(snapshot.Revision) != selection.ResourceRevision {
					return teamstate.ErrConflict
				}
				value.ID = conflict.IntegrationID
				value.AttemptIDs = make([]team.AttemptID, len(conflict.AttemptIDs))
				for index, attemptID := range conflict.AttemptIDs {
					value.AttemptIDs[index] = team.AttemptID(attemptID)
				}
				value.ResourceRevision = team.Revision(selection.ResourceRevision)
				value.State = teamstate.IntegrationConflict
				value.DiffDigest = conflict.Composition.Digest
				value.Cleanup = teamstate.CleanupRetain
				snapshot.State = teamstate.StateBlockedConflict

				return nil
			},
		)
		preview := TeamIntegrationPreview{
			ID: conflict.IntegrationID, ResourceRevision: team.Revision(selection.ResourceRevision),
			DiffDigest: conflict.Composition.Digest, ProducesUnstagedChanges: true,
		}
		preview.AttemptIDs = make([]team.AttemptID, len(conflict.AttemptIDs))
		for index, attemptID := range conflict.AttemptIDs {
			preview.AttemptIDs[index] = team.AttemptID(attemptID)
		}
		if persistErr == nil {
			r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
				TeamID: coordinator.id, IntegrationID: conflict.IntegrationID,
				State: TeamIntegrationConflict, Attempts: len(conflict.AttemptIDs),
				Code: "composition_conflict",
			})
		}

		return preview, errors.Join(err, persistErr)
	}
	resource, err := manager.Resource(prepared.ID)
	if err != nil {
		return TeamIntegrationPreview{}, err
	}
	state := teamstate.IntegrationReady
	if prepared.Verification.Status == teamintegration.VerificationPassed {
		state = teamstate.IntegrationVerified
	} else if prepared.Verification.Status != teamintegration.VerificationNotRun {
		state = teamstate.IntegrationFailed
	}
	if _, err := mutateIntegrationResource(
		operationCtx, coordinator.state, coordinator.id, prepared.ID, "prepared",
		func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			if uint64(snapshot.Revision) != selection.ResourceRevision {
				return teamstate.ErrConflict
			}
			if value.ID != "" {
				return teamstate.ErrConflict
			}
			value.ID = prepared.ID
			value.AttemptIDs = make([]team.AttemptID, len(prepared.AttemptIDs))
			for index, attemptID := range prepared.AttemptIDs {
				value.AttemptIDs[index] = team.AttemptID(attemptID)
			}
			value.ResourceRevision = team.Revision(selection.ResourceRevision)
			value.Worktree = integrationWorktreeState(resource, uint64(snapshot.Revision)+1)
			value.State = state
			value.DiffDigest = prepared.DiffDigest
			value.TreeOID = prepared.TreeOID
			value.CommitOID = prepared.CommitOID
			value.ManifestDigest = prepared.Manifest.Digest
			value.VerificationDigest = prepared.Verification.Digest
			value.ApprovalTokenHash = prepared.ApprovalTokenHash
			value.Cleanup = teamstate.CleanupRetain
			snapshot.State = teamstate.StateIntegrationPending

			return nil
		},
	); err != nil {
		_ = manager.Reject(prepared.ID, prepared.ApprovalToken)
		retainedErr := persistPreparedIntegrationRetained(
			context.WithoutCancel(operationCtx), coordinator.state, coordinator.id,
			prepared, resource, team.Revision(selection.ResourceRevision),
		)
		if retainedErr == nil {
			r.publishTeamIntegrationLifecycle(
				context.WithoutCancel(operationCtx),
				integrationLifecycle(
					coordinator.id, prepared.ID, TeamIntegrationInterrupted,
					"state_persistence_failed", len(prepared.AttemptIDs),
					prepared.Manifest, prepared.Verification,
				),
			)
		}

		return TeamIntegrationPreview{
			ID: prepared.ID, AttemptIDs: integrationAttemptIDs(prepared.AttemptIDs),
			ResourceRevision: team.Revision(selection.ResourceRevision),
			CommitOID:        prepared.CommitOID, TreeOID: prepared.TreeOID,
			Manifest: prepared.Manifest, DiffDigest: prepared.DiffDigest,
			Verification: prepared.Verification, ExpiresAt: prepared.ExpiresAt,
			ProducesUnstagedChanges: true,
		}, errors.Join(err, retainedErr)
	}

	attemptIDs := make([]team.AttemptID, len(prepared.AttemptIDs))
	for index, attemptID := range prepared.AttemptIDs {
		attemptIDs[index] = team.AttemptID(attemptID)
	}
	lifecycleState, lifecycleCode := preparedIntegrationLifecycle(prepared.Verification)
	r.publishTeamIntegrationLifecycle(operationCtx, integrationLifecycle(
		coordinator.id, prepared.ID, lifecycleState, lifecycleCode,
		len(prepared.AttemptIDs), prepared.Manifest, prepared.Verification,
	))

	return TeamIntegrationPreview{
		ID: prepared.ID, AttemptIDs: attemptIDs, ResourceRevision: team.Revision(selection.ResourceRevision),
		CommitOID: prepared.CommitOID, TreeOID: prepared.TreeOID,
		Manifest: prepared.Manifest, DiffDigest: prepared.DiffDigest,
		Verification: prepared.Verification, ApprovalToken: prepared.ApprovalToken,
		ApprovalTokenHash: prepared.ApprovalTokenHash, ExpiresAt: prepared.ExpiresAt,
		ProducesUnstagedChanges: true,
	}, nil
}

// ApplyTeamIntegration applies an approved manifest as unstaged/untracked parent changes.
func (r *Runtime) ApplyTeamIntegration(
	ctx context.Context,
	approval TeamIntegrationApproval,
) (TeamIntegrationResult, error) {
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamIntegration, runtimeResolution{}, nil,
	)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	defer r.endOperation(operation)
	coordinator, err := r.currentTeamCoordinator()
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()
	manager, err := r.integrationManager(coordinator)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	approved, err := validateIntegrationApproval(
		operationCtx, coordinator.state, coordinator.id, approval,
	)
	if err != nil {
		_ = manager.Reject(approval.ID, approval.Token)

		return TeamIntegrationResult{}, err
	}
	if _, err := mutateIntegrationResource(
		operationCtx, coordinator.state, coordinator.id, approval.ID, "applying",
		func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			if snapshot.Revision != approved.ResourceRevision+1 ||
				(value.State != teamstate.IntegrationReady && value.State != teamstate.IntegrationVerified) {
				return teamintegration.ErrStale
			}
			value.State = teamstate.IntegrationApplying

			return nil
		},
	); err != nil {
		_ = manager.Reject(approval.ID, approval.Token)

		return TeamIntegrationResult{}, err
	}
	r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
		TeamID: coordinator.id, IntegrationID: approval.ID,
		State: TeamIntegrationApplying,
	})
	result, err := manager.Apply(operationCtx, approval.ID, approval.Token)
	if err != nil {
		state := teamstate.IntegrationRetained
		lifecycleState := TeamIntegrationRejected
		code := "apply_rejected"
		if retained := new(teamintegration.RetainedError); errors.As(err, &retained) {
			state = teamstate.IntegrationInterrupted
			lifecycleState = TeamIntegrationInterrupted
			code = "apply_interrupted"
		}
		if rolledBack := new(teamintegration.RolledBackError); errors.As(err, &rolledBack) {
			state = teamstate.IntegrationRolledBack
			lifecycleState = TeamIntegrationRolledBack
			code = ""
		}
		_, _ = mutateIntegrationResource(
			context.WithoutCancel(operationCtx), coordinator.state, coordinator.id,
			approval.ID, "apply-failed",
			func(_ *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
				value.State = state

				return nil
			},
		)
		r.publishTeamIntegrationLifecycle(context.WithoutCancel(operationCtx), TeamIntegrationLifecycle{
			TeamID: coordinator.id, IntegrationID: approval.ID,
			State: lifecycleState, Code: code,
		})

		return TeamIntegrationResult{}, err
	}
	if _, err := mutateIntegrationResource(
		operationCtx, coordinator.state, coordinator.id, approval.ID, "applied",
		func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			value.State = teamstate.IntegrationApplied
			value.Cleanup = teamstate.CleanupEligible
			snapshot.State = teamstate.StateIntegrated

			return nil
		},
	); err != nil {
		return TeamIntegrationResult{}, &teamintegration.RetainedError{
			IntegrationID: approval.ID, Cause: err,
		}
	}
	r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
		TeamID: coordinator.id, IntegrationID: approval.ID,
		State: TeamIntegrationApplied,
	})

	return TeamIntegrationResult(result), nil
}

// RejectTeamIntegration consumes a preview token and retains its artifacts.
func (r *Runtime) RejectTeamIntegration(ctx context.Context, approval TeamIntegrationApproval) error {
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamIntegration, runtimeResolution{}, nil,
	)
	if err != nil {
		return err
	}
	defer r.endOperation(operation)
	coordinator, err := r.currentTeamCoordinator()
	if err != nil {
		return err
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()
	manager, err := r.integrationManager(coordinator)
	if err != nil {
		return err
	}
	if err := manager.Reject(approval.ID, approval.Token); err != nil {
		return err
	}
	_, err = mutateIntegrationResource(
		operationCtx, coordinator.state, coordinator.id, approval.ID, "rejected",
		func(_ *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			value.State = teamstate.IntegrationRetained

			return nil
		},
	)
	if err == nil {
		r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
			TeamID: coordinator.id, IntegrationID: approval.ID,
			State: TeamIntegrationRejected,
		})
	}

	return err
}

// TeamIntegrationRecoveries discovers interrupted journals without mutation.
func (r *Runtime) TeamIntegrationRecoveries(ctx context.Context) ([]TeamIntegrationRecovery, error) {
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamIntegration, runtimeResolution{}, nil,
	)
	if err != nil {
		return nil, err
	}
	defer r.endOperation(operation)
	coordinator, err := r.currentTeamCoordinator()
	if err != nil {
		return nil, err
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()
	manager, err := r.integrationManager(coordinator)
	if err != nil {
		return nil, err
	}

	candidates, err := manager.Discover(operationCtx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
			TeamID: coordinator.id, IntegrationID: candidate.ID,
			State: TeamIntegrationRecoverable, Code: "recovery_available",
		})
	}

	return candidates, nil
}

// RecoverTeamIntegration explicitly completes or rolls back one journal.
func (r *Runtime) RecoverTeamIntegration(
	ctx context.Context,
	request TeamIntegrationRecoveryRequest,
) (TeamIntegrationResult, error) {
	operationCtx, operation, err := r.beginOperation(
		ctx, operationTeamIntegration, runtimeResolution{}, nil,
	)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	defer r.endOperation(operation)
	coordinator, err := r.currentTeamCoordinator()
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	coordinator.integrationMu.Lock()
	defer coordinator.integrationMu.Unlock()
	manager, err := r.integrationManager(coordinator)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	result, err := manager.Recover(operationCtx, request.ID, request.Action)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	state := teamstate.IntegrationApplied
	if request.Action == teamintegration.RecoveryRollback {
		state = teamstate.IntegrationRolledBack
	}
	_, err = mutateIntegrationResource(
		operationCtx, coordinator.state, coordinator.id, request.ID, "recovered",
		func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			value.State = state
			value.Cleanup = teamstate.CleanupEligible
			if state == teamstate.IntegrationApplied {
				snapshot.State = teamstate.StateIntegrated
			}

			return nil
		},
	)
	if err != nil {
		return TeamIntegrationResult{}, err
	}
	lifecycleState := TeamIntegrationApplied
	if request.Action == teamintegration.RecoveryRollback {
		lifecycleState = TeamIntegrationRolledBack
	}
	r.publishTeamIntegrationLifecycle(operationCtx, TeamIntegrationLifecycle{
		TeamID: coordinator.id, IntegrationID: request.ID,
		State: lifecycleState,
	})

	return TeamIntegrationResult(result), nil
}

func preparedIntegrationLifecycle(
	verification teamintegration.Verification,
) (TeamIntegrationStatus, string) {
	switch verification.Status {
	case teamintegration.VerificationNotRun:
		return TeamIntegrationReady, ""
	case teamintegration.VerificationPassed:
		return TeamIntegrationVerified, ""
	case teamintegration.VerificationApprovalNeeded:
		return TeamIntegrationApprovalRequired, ""
	case teamintegration.VerificationTimeout:
		return TeamIntegrationVerificationFailed, "verification_timeout"
	case teamintegration.VerificationFailed:
		return TeamIntegrationVerificationFailed, "verification_failed"
	default:
		return TeamIntegrationVerificationFailed, "verification_invalid"
	}
}

func integrationLifecycle(
	teamID team.ID,
	id string,
	state TeamIntegrationStatus,
	code string,
	attempts int,
	manifest teamintegration.Manifest,
	verification teamintegration.Verification,
) TeamIntegrationLifecycle {
	return TeamIntegrationLifecycle{
		TeamID: teamID, IntegrationID: id, State: state, Code: code,
		VerificationState: string(verification.Status), Attempts: attempts,
		Files: len(manifest.Entries), Added: manifest.Added, Changed: manifest.Changed,
		Deleted: manifest.Deleted, Binary: manifest.Binary,
	}
}

func (r *Runtime) currentTeamCoordinator() (*teamCoordinator, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}
	r.mu.Lock()
	coordinator := r.team
	r.mu.Unlock()
	if coordinator == nil {
		return nil, ErrTeamIntegrationUnavailable
	}

	return coordinator, nil
}

func (r *Runtime) integrationManager(coordinator *teamCoordinator) (*teamintegration.Manager, error) {
	if coordinator.integration != nil {
		return coordinator.integration, nil
	}
	manager, err := teamintegration.New(teamintegration.Options{
		GitPath: r.opts.GitPath, ProductRoot: r.paths.Root(),
		WorktreesRoot: r.paths.WorktreesRoot(), IntegrationsRoot: r.paths.TeamIntegrationsDir(),
		Limits: teamintegration.DefaultLimits(),
	})
	if err != nil {
		return nil, err
	}
	coordinator.integration = manager

	return manager, nil
}

//nolint:gocyclo // Dependency closure and captured-result identity are validated in one stable traversal.
func buildIntegrationSelection(
	aggregate team.Team,
	resources teamstate.Snapshot,
	selected []team.TaskID,
) (teamintegration.Selection, error) {
	if aggregate.ID == "" || aggregate.ID != resources.TeamID || resources.Revision == 0 ||
		resources.Repository.BaseOID == "" {
		return teamintegration.Selection{}, ErrTeamIntegrationSelection
	}
	tasks := make(map[team.TaskID]team.Task, len(aggregate.Tasks))
	for _, taskValue := range aggregate.Tasks {
		if _, duplicate := tasks[taskValue.ID]; duplicate {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
		tasks[taskValue.ID] = taskValue
	}
	if len(selected) == 0 {
		for _, taskValue := range aggregate.Tasks {
			if taskValue.Status == team.TaskStatusCompleted {
				selected = append(selected, taskValue.ID)
			}
		}
	}
	selected = slices.Clone(selected)
	slices.Sort(selected)
	if len(selected) == 0 {
		return teamintegration.Selection{}, ErrTeamIntegrationSelection
	}
	for index := 1; index < len(selected); index++ {
		if selected[index] == selected[index-1] {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
	}
	closure := make(map[team.TaskID]struct{}, len(selected))
	var visit func(team.TaskID) error
	visit = func(id team.TaskID) error {
		if _, exists := closure[id]; exists {
			return nil
		}
		taskValue, exists := tasks[id]
		if !exists || taskValue.Status != team.TaskStatusCompleted {
			return ErrTeamIntegrationSelection
		}
		closure[id] = struct{}{}
		for _, dependency := range taskValue.DependencyIDs {
			if err := visit(dependency); err != nil {
				return err
			}
		}

		return nil
	}
	for _, id := range selected {
		if err := visit(id); err != nil {
			return teamintegration.Selection{}, err
		}
	}
	ordered, err := stableTaskOrder(tasks, closure)
	if err != nil {
		return teamintegration.Selection{}, err
	}
	resourcesByAttempt := make(map[team.AttemptID]teamstate.AttemptResource, len(resources.Attempts))
	for _, value := range resources.Attempts {
		if _, duplicate := resourcesByAttempt[value.AttemptID]; duplicate {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
		resourcesByAttempt[value.AttemptID] = value
	}
	artifacts := make([]teamintegration.Artifact, 0, len(ordered))
	for _, taskID := range ordered {
		taskValue := tasks[taskID]
		if len(taskValue.Attempts) == 0 {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
		attempt := taskValue.Attempts[len(taskValue.Attempts)-1]
		if attempt.Status != team.AttemptStatusCompleted {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
		resource, exists := resourcesByAttempt[attempt.ID]
		if !exists || resource.TaskID != taskID ||
			(resource.State != teamstate.AttemptCaptured && resource.State != teamstate.AttemptTerminal) ||
			resource.Worktree.BaseOID == "" || resource.Worktree.ResultRef == "" ||
			resource.Worktree.ResultCommitOID == "" {
			return teamintegration.Selection{}, ErrTeamIntegrationSelection
		}
		artifacts = append(artifacts, teamintegration.Artifact{
			TaskID: string(taskID), AttemptID: string(attempt.ID),
			BaseOID: resource.Worktree.BaseOID, ResultOID: resource.Worktree.ResultCommitOID,
			ResultRef: resource.Worktree.ResultRef,
		})
	}

	return teamintegration.Selection{
		TeamID: string(aggregate.ID), ResourceRevision: uint64(resources.Revision),
		BaseOID: resources.Repository.BaseOID, Artifacts: artifacts,
	}, nil
}

func stableTaskOrder(
	tasks map[team.TaskID]team.Task,
	closure map[team.TaskID]struct{},
) ([]team.TaskID, error) {
	indegree := make(map[team.TaskID]int, len(closure))
	dependents := make(map[team.TaskID][]team.TaskID, len(closure))
	for id := range closure {
		for _, dependency := range tasks[id].DependencyIDs {
			if _, selected := closure[dependency]; !selected {
				continue
			}
			indegree[id]++
			dependents[dependency] = append(dependents[dependency], id)
		}
	}
	ready := make([]team.TaskID, 0)
	for id := range closure {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	result := make([]team.TaskID, 0, len(closure))
	for len(ready) != 0 {
		id := ready[0]
		ready = ready[1:]
		result = append(result, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				slices.Sort(ready)
			}
		}
	}
	if len(result) != len(closure) {
		return nil, ErrTeamIntegrationSelection
	}

	return result, nil
}

func mutateIntegrationResource(
	ctx context.Context,
	store *teamstate.Store,
	teamID team.ID,
	id, action string,
	change func(*teamstate.Snapshot, *teamstate.IntegrationResource) error,
) (teamstate.IntegrationResource, error) {
	if store == nil || teamID == "" || id == "" || change == nil {
		return teamstate.IntegrationResource{}, ErrTeamIntegrationUnavailable
	}
	for range teamStateConflictRetries {
		snapshot, err := store.Load(ctx, teamID)
		if err != nil {
			return teamstate.IntegrationResource{}, err
		}
		next := cloneTeamResourceSnapshot(snapshot)
		index := integrationResourceIndex(next.Integrations, id)
		if index < 0 {
			next.Integrations = append(next.Integrations, teamstate.IntegrationResource{})
			index = len(next.Integrations) - 1
		}
		if err := change(&next, &next.Integrations[index]); err != nil {
			return teamstate.IntegrationResource{}, err
		}
		next.Revision = snapshot.Revision + 1
		next.UpdatedAt = time.Now().UTC()
		committed, err := store.Commit(ctx, teamstate.Mutation{
			CommandID: admissionCommandID(
				teamID, fmt.Sprintf("integration-%s-%d", action, snapshot.Revision), id,
			),
			ExpectedRevision: snapshot.Revision, Snapshot: next,
		})
		if errors.Is(err, teamstate.ErrConflict) {
			continue
		}
		if err != nil {
			return teamstate.IntegrationResource{}, err
		}
		index = integrationResourceIndex(committed.Integrations, id)
		if index < 0 {
			return teamstate.IntegrationResource{}, teamstate.ErrNotFound
		}

		return committed.Integrations[index], nil
	}

	return teamstate.IntegrationResource{}, teamstate.ErrConflict
}

func integrationResourceIndex(values []teamstate.IntegrationResource, id string) int {
	for index := range values {
		if values[index].ID == id {
			return index
		}
	}

	return -1
}

func integrationAttemptIDs(values []string) []team.AttemptID {
	result := make([]team.AttemptID, len(values))
	for index, value := range values {
		result[index] = team.AttemptID(value)
	}

	return result
}

func persistPreparedIntegrationRetained(
	ctx context.Context,
	store *teamstate.Store,
	teamID team.ID,
	prepared teamintegration.Preview,
	resource teamintegration.Resource,
	resourceRevision team.Revision,
) error {
	_, err := mutateIntegrationResource(
		ctx, store, teamID, prepared.ID, "prepared-retained",
		func(snapshot *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			value.ID = prepared.ID
			value.AttemptIDs = make([]team.AttemptID, len(prepared.AttemptIDs))
			for index, attemptID := range prepared.AttemptIDs {
				value.AttemptIDs[index] = team.AttemptID(attemptID)
			}
			value.ResourceRevision = resourceRevision
			value.Worktree = integrationWorktreeState(resource, uint64(snapshot.Revision)+1)
			value.State = teamstate.IntegrationInterrupted
			value.DiffDigest = prepared.DiffDigest
			value.TreeOID = prepared.TreeOID
			value.CommitOID = prepared.CommitOID
			value.ManifestDigest = prepared.Manifest.Digest
			value.VerificationDigest = prepared.Verification.Digest
			value.ApprovalTokenHash = prepared.ApprovalTokenHash
			value.Cleanup = teamstate.CleanupRetain
			snapshot.State = teamstate.StateInterrupted

			return nil
		},
	)

	return err
}

func integrationWorktreeState(
	resource teamintegration.Resource,
	generation uint64,
) teamstate.WorktreeResource {
	identity := func(value interface {
		Path() string
		Device() uint64
		Inode() uint64
	},
	) teamstate.FileIdentity {
		return teamstate.FileIdentity{Path: value.Path(), Device: value.Device(), Inode: value.Inode()}
	}

	return teamstate.WorktreeResource{
		ID: resource.ID, Workspace: identity(resource.Workspace), Directory: identity(resource.Directory),
		GitDir: identity(resource.GitDir), CommonDir: identity(resource.CommonDir),
		ObjectFormat: resource.ObjectFormat, BranchRef: resource.BranchRef,
		BaseOID: resource.BaseOID, ResultRef: resource.IntegrationRef,
		ResultCommitOID: resource.CommitOID, LockReason: resource.LockReason,
		LeaseGeneration: generation,
	}
}

func validateIntegrationApproval(
	ctx context.Context,
	store *teamstate.Store,
	teamID team.ID,
	approval TeamIntegrationApproval,
) (teamstate.IntegrationResource, error) {
	if approval.ID == "" || approval.Token == "" {
		return teamstate.IntegrationResource{}, teamintegration.ErrInvalid
	}
	snapshot, err := store.Load(ctx, teamID)
	if err != nil {
		return teamstate.IntegrationResource{}, err
	}

	return validateIntegrationApprovalSnapshot(snapshot, approval)
}

func validateIntegrationApprovalSnapshot(
	snapshot teamstate.Snapshot,
	approval TeamIntegrationApproval,
) (teamstate.IntegrationResource, error) {
	index := integrationResourceIndex(snapshot.Integrations, approval.ID)
	if index < 0 {
		return teamstate.IntegrationResource{}, teamstate.ErrNotFound
	}
	resource := snapshot.Integrations[index]
	sum := sha256.Sum256([]byte(approval.Token))
	if resource.ApprovalTokenHash == "" || resource.ApprovalTokenHash != hex.EncodeToString(sum[:]) {
		return teamstate.IntegrationResource{}, teamintegration.ErrStale
	}
	if resource.ResourceRevision == 0 || snapshot.Revision != resource.ResourceRevision+1 ||
		(resource.State != teamstate.IntegrationReady && resource.State != teamstate.IntegrationVerified) {
		return teamstate.IntegrationResource{}, teamintegration.ErrStale
	}

	return resource, nil
}

type runtimeIntegrationVerifier struct {
	runtime *Runtime
	spec    execution.OperationSpec
}

func (v runtimeIntegrationVerifier) Verify(
	ctx context.Context,
	request teamintegration.VerificationRequest,
) (teamintegration.Verification, error) {
	source, _ := v.runtime.config.Source(config.FieldSandbox)
	policy, err := execution.NewPolicy(request.Workspace, execution.PolicyConfig{
		Sandbox: v.runtime.config.Sandbox, Approval: v.runtime.config.Approval,
		SandboxSource: source, Protected: []string{v.runtime.paths.Root()},
	})
	if err != nil {
		return teamintegration.Verification{}, err
	}
	executor, err := execution.NewExecutor(request.Workspace, execution.ExecutorConfig{
		TempRoot: v.runtime.opts.TempRoot, Environment: v.runtime.opts.Environment,
		Protected: []string{v.runtime.paths.Root()},
	})
	if err != nil {
		return teamintegration.Verification{}, err
	}
	operation, err := execution.NewOperation(ctx, request.Workspace, v.spec)
	if err != nil {
		return teamintegration.Verification{}, err
	}
	verification := teamintegration.Verification{
		TreeOID: request.TreeOID, CommandFingerprint: operation.Fingerprint().String(),
	}
	decision := policy.Evaluate(operation)
	if decision.Verdict() == execution.VerdictReview {
		verification.Status = teamintegration.VerificationApprovalNeeded

		return verification, nil
	}
	authorization, allowed := decision.Authorization()
	if !allowed {
		verification.Status = teamintegration.VerificationFailed
		verification.OutputDigest = digestVerificationOutput([]byte(decision.Reason()))

		return verification, nil
	}
	result, runErr := executor.Execute(ctx, operation, authorization, execution.SinkFunc(
		func(context.Context, execution.OutputChunk) error { return nil },
	))
	verification.ExitCode = result.ExitCode
	verification.Signal = result.Signal
	verification.DurationMillis = result.Duration.Milliseconds()
	verification.Truncated = result.Stdout.Truncated() || result.Stderr.Truncated()
	verification.OutputDigest = digestVerificationOutput(
		append(append(result.Stdout.Head(), result.Stdout.Tail()...),
			append(result.Stderr.Head(), result.Stderr.Tail()...)...),
	)
	switch {
	case result.Status == execution.StatusTimedOut:
		verification.Status = teamintegration.VerificationTimeout
	case runErr == nil && result.Status == execution.StatusExited && result.ExitCode == 0:
		verification.Status = teamintegration.VerificationPassed
	default:
		verification.Status = teamintegration.VerificationFailed
	}

	return verification, nil
}

func digestVerificationOutput(value []byte) string {
	sum := sha256.Sum256(value)

	return hex.EncodeToString(sum[:])
}
