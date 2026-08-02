package coding

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

const maximumTeamWorkerInspectionSessions = 1_000

// ErrTeamWorkerStale means an inspection target no longer identifies the
// exact durable Attempt owner generation reviewed by the caller.
var ErrTeamWorkerStale = errors.New("coding Team Worker target is stale")

type resolvedTeamWorker struct {
	coordinator *teamCoordinator
	attempt     teamstate.AttemptResource
}

// ObserveTeamWorker atomically snapshots and subscribes to one live Worker
// Runtime after validating its exact durable Attempt owner generation.
func (r *Runtime) ObserveTeamWorker(
	ctx context.Context,
	target TeamWorkerTarget,
) (EventObservation, error) {
	resolved, err := r.resolveTeamWorker(ctx, target)
	if err != nil {
		return EventObservation{}, err
	}

	if resolved.attempt.State != teamstate.AttemptRunning {
		return EventObservation{}, fmt.Errorf("%w: Team Worker is not live", ErrRuntimeBusy)
	}

	owner, err := resolved.coordinator.exactAttemptOwner(
		ctx,
		teamcontrol.ResolvedTarget{
			TeamID: target.TeamID, MemberID: target.MemberID,
			TaskID: target.TaskID, AttemptID: target.AttemptID,
			ContinuationID:  resolved.attempt.ContinuationID,
			SessionID:       resolved.attempt.Session.SessionID,
			WorkspaceID:     resolved.attempt.Session.WorkspaceID,
			OwnerGeneration: target.OwnerGeneration,
		},
	)
	if errors.Is(err, errTeamControlOwnerChanged) {
		return EventObservation{}, fmt.Errorf("%w: Team Worker ownership changed", ErrRuntimeBusy)
	}

	if err != nil {
		return EventObservation{}, err
	}

	return owner.observeEvents(resolved.attempt)
}

func (o *attemptOwner) observeEvents(
	resource teamstate.AttemptResource,
) (EventObservation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.matchesAttemptResource(resource) {
		return EventObservation{}, fmt.Errorf("%w: Team Worker ownership changed", ErrRuntimeBusy)
	}

	metadata := o.runtime.handle.Metadata()
	if metadata.ID != resource.Session.SessionID ||
		metadata.WorkspaceID != resource.Session.WorkspaceID {
		return EventObservation{}, ErrTeamWorkerStale
	}

	observation, err := o.runtime.ObserveEvents()
	if err != nil {
		if errors.Is(err, ErrEventStreamClosed) {
			return EventObservation{}, fmt.Errorf("%w: Team Worker event stream closed", ErrRuntimeBusy)
		}

		return EventObservation{}, err
	}

	if observation.State.SessionID != resource.Session.SessionID ||
		observation.Subscription == nil {
		if observation.Subscription != nil {
			observation.Subscription.Close()
		}

		return EventObservation{}, ErrTeamWorkerStale
	}

	return observation, nil
}

// matchesAttemptResource must be called while o.mu is held.
func (o *attemptOwner) matchesAttemptResource(resource teamstate.AttemptResource) bool {
	return o.runtime != nil &&
		o.factory != nil &&
		o.factory.ownerGeneration == resource.Worktree.LeaseGeneration &&
		o.candidate.memberID == resource.MemberID &&
		o.candidate.key.taskID == resource.TaskID &&
		o.candidate.key.attemptID == resource.AttemptID &&
		o.candidate.continuationID == resource.ContinuationID
}

// InspectTeamWorkerState reconstructs one terminal Worker's ordinary Coding
// State from its exact durable Session and releases the read handle before
// returning. It never opens a model, Tool runtime, or Continuation execution.
func (r *Runtime) InspectTeamWorkerState(
	ctx context.Context,
	target TeamWorkerTarget,
) (State, error) {
	resolved, err := r.resolveTeamWorker(ctx, target)
	if err != nil {
		return State{}, err
	}

	if !terminalTeamWorkerAttemptState(resolved.attempt.State) {
		return State{}, fmt.Errorf("%w: Team Worker ownership is still changing", ErrRuntimeBusy)
	}

	metadata, err := r.findTerminalTeamWorkerSession(ctx, target, resolved.attempt)
	if err != nil {
		return State{}, err
	}

	handle, err := r.repository.OpenTeamWorker(ctx, session.OpenTeamWorkerOptions{
		ID: metadata.ID, WorkspaceID: metadata.WorkspaceID,
		Lineage: metadata.TeamWorker,
	})
	if errors.Is(err, session.ErrLocked) {
		return State{}, fmt.Errorf("%w: Team Worker Session is still live", ErrRuntimeBusy)
	}

	if err != nil {
		return State{}, err
	}

	state, inspectErr := r.bootstrapTeamWorkerState(ctx, handle)

	latest, latestErr := r.resolveTeamWorker(ctx, target)
	if latestErr == nil && latest.attempt != resolved.attempt {
		latestErr = ErrTeamWorkerStale
	}

	if latestErr == nil && !terminalTeamWorkerAttemptState(latest.attempt.State) {
		latestErr = fmt.Errorf("%w: Team Worker ownership changed", ErrRuntimeBusy)
	}

	closeErr := handle.Close()
	if err := errors.Join(inspectErr, latestErr, closeErr); err != nil {
		return State{}, err
	}

	return state.Clone(), nil
}

func (r *Runtime) resolveTeamWorker(
	ctx context.Context,
	target TeamWorkerTarget,
) (resolvedTeamWorker, error) {
	if err := r.validateTeamWorkerTarget(ctx, target); err != nil {
		return resolvedTeamWorker{}, err
	}

	coordinator, err := r.resolveTeamReadCoordinator(ctx, target.TeamID)
	if err != nil {
		return resolvedTeamWorker{}, fmt.Errorf("%w: Team is unavailable", ErrTeamWorkerStale)
	}

	attempt, err := r.loadTeamWorkerAttempt(ctx, coordinator, target)
	if err != nil {
		return resolvedTeamWorker{}, err
	}

	return resolvedTeamWorker{
		coordinator: coordinator,
		attempt:     attempt,
	}, nil
}

func (r *Runtime) validateTeamWorkerTarget(
	ctx context.Context,
	target TeamWorkerTarget,
) error {
	if r == nil {
		return ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if r.isTeamWorker() || !validTeamWorkerTarget(target) {
		return ErrTeamWorkerStale
	}

	return nil
}

func (r *Runtime) loadTeamWorkerAttempt(
	ctx context.Context,
	coordinator *teamCoordinator,
	target TeamWorkerTarget,
) (teamstate.AttemptResource, error) {
	resources, err := coordinator.state.Load(ctx, target.TeamID)
	if err != nil {
		return teamstate.AttemptResource{}, err
	}

	if resources.TeamID != target.TeamID ||
		resources.Parent.SessionID != r.handle.Metadata().ID {
		return teamstate.AttemptResource{}, ErrTeamWorkerStale
	}

	index := attemptResourceIndex(resources.Attempts, target.AttemptID)
	if index < 0 {
		return teamstate.AttemptResource{}, ErrTeamWorkerStale
	}

	attempt := resources.Attempts[index]
	if err := validateTeamWorkerAttempt(attempt, target); err != nil {
		return teamstate.AttemptResource{}, err
	}

	return attempt, nil
}

func validateTeamWorkerAttempt(
	attempt teamstate.AttemptResource,
	target TeamWorkerTarget,
) error {
	if attempt.MemberID != target.MemberID || attempt.TaskID != target.TaskID ||
		attempt.AttemptID != target.AttemptID ||
		attempt.Worktree.LeaseGeneration != target.OwnerGeneration {
		return ErrTeamWorkerStale
	}

	if attempt.Session.SessionID == "" || attempt.Session.WorkspaceID == "" ||
		attempt.ContinuationID == "" {
		return fmt.Errorf("%w: Team Worker Session is not ready", ErrRuntimeBusy)
	}

	if err := session.ValidateID(attempt.Session.SessionID); err != nil {
		return ErrTeamWorkerStale
	}

	return nil
}

func validTeamWorkerTarget(target TeamWorkerTarget) bool {
	if target.OwnerGeneration == 0 {
		return false
	}

	for _, value := range []string{
		string(target.TeamID), string(target.MemberID),
		string(target.TaskID), string(target.AttemptID),
	} {
		if !validTeamRoutingID(value, true) {
			return false
		}
	}

	return true
}

func terminalTeamWorkerAttemptState(state teamstate.AttemptState) bool {
	switch state {
	case teamstate.AttemptTerminal,
		teamstate.AttemptInterrupted,
		teamstate.AttemptFailed,
		teamstate.AttemptCancelled,
		teamstate.AttemptConflicted,
		teamstate.AttemptCaptureFailed,
		teamstate.AttemptOrphaned:
		return true
	default:
		return false
	}
}

func (r *Runtime) findTerminalTeamWorkerSession(
	ctx context.Context,
	target TeamWorkerTarget,
	attempt teamstate.AttemptResource,
) (session.Metadata, error) {
	parentSessionID := r.handle.Metadata().ID

	values, err := r.repository.ListTeamWorkers(
		ctx,
		parentSessionID,
		target.TeamID,
		maximumTeamWorkerInspectionSessions,
	)
	if err != nil {
		return session.Metadata{}, err
	}

	want := session.TeamWorkerLineage{
		ParentSessionID: parentSessionID,
		TeamID:          target.TeamID, MemberID: target.MemberID,
		TaskID: target.TaskID, AttemptID: target.AttemptID,
		ContinuationID: attempt.ContinuationID,
	}
	for _, value := range values {
		if value.ID == attempt.Session.SessionID &&
			value.WorkspaceID == attempt.Session.WorkspaceID &&
			value.TeamWorker == want {
			return value, nil
		}
	}

	return session.Metadata{}, ErrTeamWorkerStale
}

func (r *Runtime) bootstrapTeamWorkerState(
	ctx context.Context,
	handle *session.Handle,
) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}

	pending, err := handle.Session().Pending()
	if err != nil {
		return State{}, err
	}

	tree, err := handle.Session().Tree(harness.TreeLimits{})
	if err != nil {
		return State{}, err
	}

	result, err := BootstrapState(BootstrapOptions{
		SessionID: handle.Metadata().ID,
		Provider:  r.model.Provider(), ModelID: r.model.ModelID(),
		ContextWindow: r.resolved.Limits.ContextWindow,
		Mode:          r.currentOperatingMode(), Path: handle.Session().Path(),
		HasPendingToolCalls: len(pending) != 0, Tree: tree,
	})
	if err != nil {
		return State{}, err
	}

	return result.State.Clone(), nil
}
