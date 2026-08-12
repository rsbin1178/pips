package coding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
)

var (
	// ErrTeamRecoveryNotFound means the requested Team is not a retained
	// candidate owned by the current Lead Session.
	ErrTeamRecoveryNotFound = errors.New("coding Team recovery candidate not found")
	// ErrTeamRecoveryStale means durable state changed after the candidate was
	// presented and the user must review a fresh candidate.
	ErrTeamRecoveryStale = errors.New("coding Team recovery candidate is stale")
	// ErrTeamRecoveryBlocked means exact identity or lifecycle validation does
	// not permit automatic recovery.
	ErrTeamRecoveryBlocked = errors.New("coding Team recovery is blocked")
	// ErrTeamRecoveryDecisionRequired prevents replaying interrupted Work
	// without an explicit user decision.
	ErrTeamRecoveryDecisionRequired = errors.New("coding Team recovery decision is required")
)

// TeamResumeDecision binds explicit recovery to the candidate the user
// reviewed. RetryInterruptedWork authorizes a new continuation Work attempt
// only when the previous process died during that Work stage.
type TeamResumeDecision struct {
	ExpectedResourceRevision team.Revision
	RetryInterruptedWork     bool
}

// ResumeTeam explicitly reacquires one retained Team and starts only its exact
// recoverable owners. Startup discovery itself never calls this method.
func (r *Runtime) ResumeTeam(
	ctx context.Context,
	teamID team.ID,
	decision TeamResumeDecision,
) (TeamReference, error) {
	if r == nil {
		return TeamReference{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return TeamReference{}, fmt.Errorf("%w: Team Workers cannot resume parent Teams", ErrTeamAdmission)
	}
	if teamID == "" || decision.ExpectedResourceRevision == 0 {
		return TeamReference{}, ErrTeamRecoveryStale
	}

	operationCtx, operation, err := r.beginOperation(
		ctx,
		operationTeamResume,
		runtimeResolution{},
		nil,
	)
	if err != nil {
		return TeamReference{}, err
	}
	defer r.endOperation(operation)

	r.mu.Lock()
	if r.team != nil || r.teamGuard.active() || r.admission.hasProposals() {
		r.mu.Unlock()

		return TeamReference{}, ErrTeamActive
	}
	r.mu.Unlock()

	candidates, err := r.discoverTeamRecovery(operationCtx)
	if err != nil {
		return TeamReference{}, err
	}
	candidate, found := recoveryCandidateByID(candidates, teamID)
	if !found {
		return TeamReference{}, ErrTeamRecoveryNotFound
	}
	if candidate.ResourceRevision != decision.ExpectedResourceRevision {
		return TeamReference{}, ErrTeamRecoveryStale
	}
	if candidate.Disposition != TeamRecoveryResume {
		return TeamReference{}, fmt.Errorf(
			"%w: %s",
			ErrTeamRecoveryBlocked,
			candidate.Disposition,
		)
	}
	if recoveryCandidateMayReplayWork(candidate) && !decision.RetryInterruptedWork {
		return TeamReference{}, ErrTeamRecoveryDecisionRequired
	}

	stateStore, err := teamstate.New(r.paths.TeamResourcesDir(), teamstate.Limits{})
	if err != nil {
		return TeamReference{}, err
	}
	snapshot, err := stateStore.Load(operationCtx, teamID)
	if err != nil {
		return TeamReference{}, err
	}
	if snapshot.Revision != decision.ExpectedResourceRevision ||
		!r.parentRecoveryIdentityMatches(operationCtx, snapshot) {
		return TeamReference{}, ErrTeamRecoveryStale
	}

	aggregateStore, err := team.NewJSONLStore(r.paths.TeamAggregatesDir())
	if err != nil {
		return TeamReference{}, err
	}
	teamEngine, err := team.New(aggregateStore)
	if err != nil {
		return TeamReference{}, err
	}
	aggregate, err := teamEngine.Get(operationCtx, teamID)
	if err != nil {
		return TeamReference{}, err
	}
	if aggregate.Status != team.StatusActive ||
		!recoveryAggregateMatchesResources(aggregate, snapshot) {
		return TeamReference{}, ErrTeamRecoveryBlocked
	}

	manager, err := teamworktree.New(teamworktree.Options{
		GitPath: r.opts.GitPath, ProductRoot: r.paths.Root(),
		WorktreesRoot: r.paths.WorktreesRoot(), LeasesRoot: r.paths.TeamLeasesDir(),
		Limits: teamworktree.DefaultLimits(),
	})
	if err != nil {
		return TeamReference{}, err
	}
	generation := uint64(snapshot.Revision) + 1
	lease, err := manager.Acquire(operationCtx, teamID, generation)
	if err != nil {
		return TeamReference{}, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			_ = lease.Close()
		}
	}()

	snapshot, err = takeoverRecoveryResources(
		operationCtx,
		stateStore,
		manager,
		lease,
		snapshot,
		generation,
	)
	if err != nil {
		return TeamReference{}, err
	}

	controlStore, err := teamcontrol.New(r.paths.TeamControlDir(), teamcontrol.Limits{})
	if err != nil {
		return TeamReference{}, err
	}
	coordinator, err := newTeamCoordinator(
		teamID,
		aggregate.LeadMemberID,
		teamEngine,
		stateStore,
		nil,
		min(defaultWorkerLimit, max(1, aggregate.Limits.MaxActiveTasks)),
		min(defaultWorkerKeyCapacity, max(1, aggregate.Limits.MaxActiveTasks)),
	)
	if err != nil {
		return TeamReference{}, err
	}
	coordinator.control = controlStore
	coordinator.worktree = manager
	coordinator.lease = lease
	coordinator.lifecycle = r.publishTeamLifecycle
	coordinator.controlLifecycle = r.publishTeamControlLifecycle

	continuationStore, err := continuation.NewJSONLStore(r.paths.TeamContinuationsDir())
	if err != nil {
		return TeamReference{}, err
	}
	continuationEngine, err := continuation.New(continuationStore)
	if err != nil {
		return TeamReference{}, err
	}
	recovered, err := prepareRecoveredCandidates(
		operationCtx,
		aggregate,
		snapshot,
		continuationStore,
		continuationEngine,
		decision,
	)
	if err != nil {
		return TeamReference{}, err
	}
	ownerFactory, err := newAttemptOwnerFactory(r, coordinator, generation, nil)
	if err != nil {
		return TeamReference{}, err
	}
	coordinator.factory = ownerFactory
	if err := reconcileInterruptedLiveControls(operationCtx, coordinator); err != nil {
		return TeamReference{}, err
	}

	if err := r.teamGuard.resume(teamID); err != nil {
		return TeamReference{}, err
	}
	guardOwned := true
	defer func() {
		if guardOwned {
			r.teamGuard.deactivate(teamID)
		}
	}()

	r.mu.Lock()
	if r.team != nil {
		r.mu.Unlock()

		return TeamReference{}, ErrTeamActive
	}
	r.team = coordinator
	r.mu.Unlock()
	coordinatorOwned := true
	defer func() {
		if coordinatorOwned {
			r.mu.Lock()
			if r.team == coordinator {
				r.team = nil
			}
			r.mu.Unlock()
		}
	}()

	if err := coordinator.startRecovered(operationCtx, recovered); err != nil {
		return TeamReference{}, err
	}
	r.publishTeamLifecycle(operationCtx, TeamLifecycle{
		TeamID: teamID, State: TeamLifecycleAdmitted,
	})

	leaseOwned = false
	guardOwned = false
	coordinatorOwned = false
	r.mu.Lock()
	r.teamRecovery = nil
	r.teamRecoveryErr = nil
	r.mu.Unlock()

	return TeamReference{
		TeamID: teamID, Admission: TeamAdmissionMode(snapshot.Repository.Admission),
		BaseOID: snapshot.Repository.BaseOID, LeadMember: aggregate.LeadMemberID,
	}, nil
}

func recoveryCandidateByID(
	values []TeamRecoveryCandidate,
	id team.ID,
) (TeamRecoveryCandidate, bool) {
	for _, value := range values {
		if value.TeamID == id {
			return value, true
		}
	}

	return TeamRecoveryCandidate{}, false
}

func recoveryCandidateMayReplayWork(candidate TeamRecoveryCandidate) bool {
	for _, attempt := range candidate.Attempts {
		if attempt.RetryWork {
			return true
		}
		switch attempt.ContinuationState {
		case continuation.StatusRunning, continuation.StatusPauseRequested:
			if attempt.ContinuationPhase != continuation.PhaseWork {
				continue
			}
			return true
		}
	}

	return false
}

func recoveryAggregateMatchesResources(
	aggregate team.Team,
	snapshot teamstate.Snapshot,
) bool {
	if aggregate.ID != snapshot.TeamID || aggregate.LeadMemberID == "" ||
		len(aggregate.Members) != len(snapshot.Members) {
		return false
	}
	resources := make(map[team.MemberID]string, len(snapshot.Members))
	for _, member := range snapshot.Members {
		resources[member.MemberID] = member.CapabilityProfileFingerprint
	}
	for _, member := range aggregate.Members {
		fingerprint, found := resources[member.ID]
		if !found {
			return false
		}
		if member.ID != aggregate.LeadMemberID && fingerprint != member.CapabilityProfileRef {
			return false
		}
	}

	return true
}

func takeoverRecoveryResources(
	ctx context.Context,
	store *teamstate.Store,
	manager *teamworktree.Manager,
	lease *teamworktree.Lease,
	current teamstate.Snapshot,
	generation uint64,
) (teamstate.Snapshot, error) {
	next := cloneTeamResourceSnapshot(current)
	for index, attempt := range next.Attempts {
		if attempt.Worktree == (teamstate.WorktreeResource{}) {
			continue
		}
		owner := teamworktree.Owner{
			TeamID: next.TeamID, MemberID: attempt.MemberID,
			AttemptID:       attempt.AttemptID,
			LeaseGeneration: attempt.Worktree.LeaseGeneration,
		}
		resource := worktreeResourceFromState(owner, attempt.Worktree)
		taken, err := manager.Takeover(ctx, lease, resource)
		if err != nil {
			return teamstate.Snapshot{}, err
		}
		next.Attempts[index].Worktree = taken.TeamState()
	}
	next.Revision = current.Revision + 1
	next.State = teamstate.StateActive
	next.UpdatedAt = time.Now().UTC()

	return store.Commit(ctx, teamstate.Mutation{
		CommandID: admissionCommandID(
			current.TeamID,
			"resource-resume",
			fmt.Sprintf("%d", generation),
		),
		ExpectedRevision: current.Revision,
		Snapshot:         next,
	})
}

func prepareRecoveredCandidates(
	ctx context.Context,
	aggregate team.Team,
	snapshot teamstate.Snapshot,
	store *continuation.JSONLStore,
	engine *continuation.Engine,
	decision TeamResumeDecision,
) ([]workerCandidate, error) {
	resources := make(map[team.MemberID]teamstate.MemberResource, len(snapshot.Members))
	for _, member := range snapshot.Members {
		resources[member.MemberID] = member
	}
	values := make([]workerCandidate, 0)
	for _, taskValue := range aggregate.Tasks {
		if taskValue.Status != team.TaskStatusRunning || len(taskValue.Attempts) == 0 {
			continue
		}
		attempt := taskValue.Attempts[len(taskValue.Attempts)-1]
		if attempt.Status != team.AttemptStatusRunning || attempt.ContinuationID == "" {
			return nil, ErrTeamRecoveryBlocked
		}
		_, err := store.Load(ctx, attempt.ContinuationID)
		if err == nil {
			execution, recoverErr := engine.Get(ctx, attempt.ContinuationID)
			if recoverErr != nil {
				return nil, recoverErr
			}
			if recoveryNeedsWorkRetry(execution) {
				if !decision.RetryInterruptedWork {
					return nil, ErrTeamRecoveryDecisionRequired
				}
				execution, recoverErr = engine.RetryWork(
					ctx,
					execution.ID,
					execution.Revision,
					"explicit Team recovery",
				)
				if recoverErr != nil {
					return nil, recoverErr
				}
			} else if recoveryNeedsDecisionRetry(execution) {
				execution, recoverErr = engine.RetryDecision(
					ctx,
					execution.ID,
					execution.Revision,
					"explicit Team recovery",
				)
				if recoverErr != nil {
					return nil, recoverErr
				}
			} else if execution.Status == continuation.StatusPaused {
				// A paused execution with no retry requirement already has a
				// durable safe resumption point. Resume restores that point; it
				// does not authorize a second Work invocation.
				execution, recoverErr = engine.Resume(
					ctx,
					execution.ID,
					execution.Revision,
					"explicit Team recovery",
				)
				if recoverErr != nil {
					return nil, recoverErr
				}
			}
			if !execution.Status.Terminal() && execution.Status != continuation.StatusReady {
				return nil, ErrTeamRecoveryBlocked
			}
		} else if !errors.Is(err, continuation.ErrNotFound) {
			return nil, err
		}

		member, found := resources[attempt.MemberID]
		if !found || member.CapabilityProfileFingerprint == "" {
			return nil, ErrTeamRecoveryBlocked
		}
		values = append(values, workerCandidate{
			key: attemptKey{
				teamID: aggregate.ID, taskID: taskValue.ID, attemptID: attempt.ID,
			},
			continuationID: attempt.ContinuationID,
			memberID:       attempt.MemberID,
			identityKey:    member.CapabilityProfileFingerprint,
			task:           taskValue,
			teamRevision:   aggregate.Revision,
		})
	}
	if len(values) > defaultWorkerLimit {
		return nil, ErrTeamRecoveryBlocked
	}

	return values, nil
}

func reconcileInterruptedLiveControls(
	ctx context.Context,
	coordinator *teamCoordinator,
) error {
	pending, err := coordinator.control.Pending(ctx, coordinator.id, teamControlPendingLimit)
	if errors.Is(err, teamcontrol.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, record := range pending {
		if record.Entry.State != teamcontrol.StateApplying ||
			teamcontrol.ClassifyInterrupted(record.Entry) != teamcontrol.DispositionDeliveryUnknown {
			continue
		}
		if err := coordinator.completeControl(
			ctx,
			record.Entry.Command.ID,
			"resume-delivery-unknown",
			teamcontrol.StateDeliveryUnknown,
			"interrupted_delivery",
		); err != nil {
			return err
		}
	}

	return nil
}
