package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
)

const (
	teamChangePageLimit    = 64
	teamChangePageBudget   = 4
	teamCandidateScanLimit = 64
)

func (c *teamCoordinator) run(ctx context.Context) {
	defer c.finishClose()

	for {
		select {
		case <-ctx.Done():
			c.stopOwners()
			for c.ownerCount() > 0 {
				c.handleOwnerCompletion(<-c.completions)
			}

			return
		case completion := <-c.completions:
			c.handleOwnerCompletion(completion)
			c.signal()
		case <-c.wake:
			if err := c.reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.mu.Lock()
				c.closeErr = errors.Join(c.closeErr, err)
				c.mu.Unlock()
			}
		}
	}
}

func (c *teamCoordinator) reconcile(ctx context.Context) error {
	if err := c.consumeControls(ctx); err != nil {
		return err
	}
	if err := c.consumeChanges(ctx); err != nil {
		return err
	}

	aggregate, err := c.engine.Get(ctx, c.id)
	if err != nil {
		return fmt.Errorf("coding Team scheduler: load Team: %w", err)
	}
	resources, err := c.state.Load(ctx, c.id)
	if err != nil {
		return fmt.Errorf("coding Team scheduler: load resources: %w", err)
	}
	if resources.TeamID != aggregate.ID || resources.Parent.SessionID == "" {
		return fmt.Errorf("%w: Team scheduler identity mismatch", ErrTeamAdmission)
	}
	if aggregate.Status != team.StatusActive || resources.State != teamstate.StateActive {
		c.emitTerminalLifecycle(ctx, aggregate.Status)
		c.stopOwners()

		return nil
	}

	candidates := readyWorkerCandidates(aggregate, resources)
	c.reconcileQueued(candidates)
	pending := make([]workerCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		c.mu.Lock()
		_, queued := c.queued[candidate.key]
		_, running := c.owners[candidate.key]
		_, seen := c.seen[candidate.key]
		c.mu.Unlock()
		if !queued && !running && !seen {
			pending = append(pending, candidate)
		}
	}
	if len(pending) > teamCandidateScanLimit {
		pending = pending[:teamCandidateScanLimit]
		defer c.signal()
	}
	for _, candidate := range pending {
		if !c.admission.enqueue(candidate) {
			continue
		}

		c.mu.Lock()
		c.queued[candidate.key] = candidate
		c.mu.Unlock()
		lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleWaiting)
		lifecycle.Activity = TeamActivityPreparing
		c.emitLifecycle(ctx, lifecycle)
	}

	if c.factory == nil {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, permit, granted := c.admission.grant()
		if !granted {
			return nil
		}
		c.mu.Lock()
		delete(c.queued, candidate.key)
		c.seen[candidate.key] = struct{}{}
		c.mu.Unlock()

		owner, createErr := c.factory.NewOwner(ctx, candidate)
		if createErr != nil {
			lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleInterrupted)
			lifecycle.Code = "attempt_owner_unavailable"
			c.emitLifecycle(ctx, lifecycle)
			permit.release()
			c.mu.Lock()
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf(
				"coding Team scheduler: prepare %s: %w",
				candidate.key.taskID,
				createErr,
			))
			c.mu.Unlock()
			continue
		}
		if owner == nil {
			lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleInterrupted)
			lifecycle.Code = "attempt_owner_unavailable"
			c.emitLifecycle(ctx, lifecycle)
			permit.release()

			return fmt.Errorf("%w: nil Worker owner", ErrTeamAdmission)
		}
		if owner.Key() != candidate.key {
			lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleInterrupted)
			lifecycle.Code = "attempt_owner_identity_mismatch"
			c.emitLifecycle(ctx, lifecycle)
			permit.release()

			return fmt.Errorf("%w: Worker owner identity mismatch", ErrTeamAdmission)
		}
		if err := c.startCandidateAttempt(ctx, candidate); err != nil {
			lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleInterrupted)
			lifecycle.Code = "attempt_start_failed"
			c.emitLifecycle(ctx, lifecycle)
			permit.release()
			c.mu.Lock()
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf(
				"coding Team scheduler: start %s: %w",
				candidate.key.taskID,
				err,
			))
			c.mu.Unlock()
			c.signal()

			return nil
		}
		// ClaimTask and StartTaskAttempt append Team changes after the cursor was
		// consumed at the beginning of this reconciliation pass. Schedule one
		// more bounded pass so the coordinator's durable cursor cannot remain
		// behind its own writes until an unrelated owner completion wakes it.
		c.signal()

		ownerCtx, cancel := context.WithCancel(ctx)
		slot := &coordinatorOwnerSlot{owner: owner, cancel: cancel, permit: permit}
		c.mu.Lock()
		if c.closing || c.closed {
			c.mu.Unlock()
			cancel()
			permit.release()

			return context.Canceled
		}
		if _, exists := c.owners[candidate.key]; exists {
			c.mu.Unlock()
			cancel()
			permit.release()

			return fmt.Errorf("%w: duplicate Worker owner", ErrTeamAdmission)
		}
		c.owners[candidate.key] = slot
		c.mu.Unlock()
		lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleRunning)
		lifecycle.Activity = TeamActivityPreparing
		c.emitLifecycle(ctx, lifecycle)

		go c.runOwner(ownerCtx, candidate.key, owner)
	}
}

func (c *teamCoordinator) startCandidateAttempt(
	ctx context.Context,
	candidate workerCandidate,
) error {
	claimID := admissionCommandID(
		candidate.key.teamID,
		"claim-attempt",
		string(candidate.key.attemptID),
	)
	startID := admissionCommandID(
		candidate.key.teamID,
		"start-attempt",
		string(candidate.key.attemptID),
	)
	for range teamStateConflictRetries {
		aggregate, err := c.engine.Get(ctx, candidate.key.teamID)
		if err != nil {
			return err
		}
		if aggregate.Status != team.StatusActive {
			return fmt.Errorf("%w: Team is %s", team.ErrInvalidState, aggregate.Status)
		}
		task, found := taskByID(aggregate.Tasks, candidate.key.taskID)
		if !found {
			return team.ErrNotFound
		}
		switch task.Status {
		case team.TaskStatusReady:
			aggregate, err = c.engine.ClaimTask(ctx, candidate.key.teamID, team.ClaimTaskRequest{
				Command: team.CommandMetadata{
					ID: claimID, ExpectedRevision: aggregate.Revision,
					Actor: team.Actor{Kind: team.ActorKindMember, ID: string(candidate.memberID)},
				},
				TaskID: candidate.key.taskID,
			})
			if errors.Is(err, team.ErrConflict) {
				continue
			}
			if err != nil {
				return err
			}
		case team.TaskStatusClaimed:
			if task.ClaimedMemberID != candidate.memberID {
				return team.ErrStaleAttempt
			}
		case team.TaskStatusRunning:
			if runningCandidateAttempt(task, candidate) {
				return nil
			}

			return team.ErrStaleAttempt
		default:
			return team.ErrStaleAttempt
		}

		_, err = c.engine.StartTaskAttempt(ctx, candidate.key.teamID, team.StartTaskAttemptRequest{
			Command: team.CommandMetadata{
				ID: startID, ExpectedRevision: aggregate.Revision, Actor: attemptActor,
			},
			TaskID: candidate.key.taskID, AttemptID: candidate.key.attemptID,
			ContinuationID: candidate.continuationID,
		})
		if errors.Is(err, team.ErrConflict) {
			continue
		}

		return err
	}

	return fmt.Errorf("%w: Team Attempt start conflict retry limit", ErrTeamAdmission)
}

func taskByID(tasks []team.Task, id team.TaskID) (team.Task, bool) {
	for _, task := range tasks {
		if task.ID == id {
			return task, true
		}
	}

	return team.Task{}, false
}

func runningCandidateAttempt(task team.Task, candidate workerCandidate) bool {
	if len(task.Attempts) == 0 || task.ClaimedMemberID != candidate.memberID {
		return false
	}
	attempt := task.Attempts[len(task.Attempts)-1]

	return attempt.Status == team.AttemptStatusRunning &&
		attempt.ID == candidate.key.attemptID &&
		attempt.MemberID == candidate.memberID &&
		attempt.ContinuationID == candidate.continuationID
}

func (c *teamCoordinator) consumeChanges(ctx context.Context) error {
	for pageIndex := 0; pageIndex < teamChangePageBudget; pageIndex++ {
		c.mu.Lock()
		after := c.revision
		c.mu.Unlock()
		page, err := c.engine.Changes(ctx, c.id, team.ChangeOptions{
			AfterRevision: after,
			Limit:         teamChangePageLimit,
		})
		if err != nil {
			return fmt.Errorf("coding Team scheduler: read changes: %w", err)
		}
		if len(page.Changes) == 0 {
			return nil
		}

		c.mu.Lock()
		if page.NextAfter > c.revision {
			c.revision = page.NextAfter
		}
		c.mu.Unlock()
		if len(page.Changes) < teamChangePageLimit {
			return nil
		}
	}

	c.signal()

	return nil
}

func (c *teamCoordinator) reconcileQueued(candidates []workerCandidate) {
	ready := make(map[attemptKey]struct{}, len(candidates))
	for _, candidate := range candidates {
		ready[candidate.key] = struct{}{}
	}

	c.mu.Lock()
	stale := make([]attemptKey, 0)
	for key := range c.queued {
		if _, found := ready[key]; !found {
			stale = append(stale, key)
		}
	}
	c.mu.Unlock()
	for _, key := range stale {
		if c.admission.cancel(key) {
			c.mu.Lock()
			delete(c.queued, key)
			c.mu.Unlock()
		}
	}
}

func (c *teamCoordinator) runOwner(
	ctx context.Context,
	key attemptKey,
	owner coordinatorOwner,
) {
	err := owner.Run(ctx)
	c.completions <- coordinatorOwnerCompletion{key: key, err: err}
}

func (c *teamCoordinator) handleOwnerCompletion(completion coordinatorOwnerCompletion) {
	c.mu.Lock()
	slot := c.owners[completion.key]
	delete(c.owners, completion.key)
	if completion.err != nil && !errors.Is(completion.err, context.Canceled) {
		c.closeErr = errors.Join(c.closeErr, fmt.Errorf(
			"coding Team Worker %s: %w",
			completion.key.taskID,
			completion.err,
		))
	}
	c.mu.Unlock()
	if slot != nil {
		slot.cancel()
		slot.permit.release()
	}
}

func (c *teamCoordinator) stopOwners() {
	c.admission.close()

	c.mu.Lock()
	queued := make([]workerCandidate, 0, len(c.queued))
	for _, candidate := range c.queued {
		queued = append(queued, candidate)
	}
	clear(c.queued)
	cancels := make([]context.CancelFunc, 0, len(c.owners))
	for _, slot := range c.owners {
		cancels = append(cancels, slot.cancel)
	}
	c.mu.Unlock()
	for _, candidate := range queued {
		lifecycle := teamAttemptLifecycle(candidate, TeamLifecycleInterrupted)
		lifecycle.Code = "attempt_not_started"
		c.emitLifecycle(context.Background(), lifecycle)
	}
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *teamCoordinator) ownerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.owners)
}

func readyWorkerCandidates(
	aggregate team.Team,
	resources teamstate.Snapshot,
) []workerCandidate {
	if aggregate.Status != team.StatusActive || resources.State != teamstate.StateActive {
		return nil
	}

	members := make(map[team.MemberID]team.Member, len(aggregate.Members))
	busy := make(map[team.MemberID]struct{})
	for _, member := range aggregate.Members {
		members[member.ID] = member
	}
	for _, task := range aggregate.Tasks {
		if task.Status == team.TaskStatusClaimed || task.Status == team.TaskStatusRunning {
			busy[task.ClaimedMemberID] = struct{}{}
		}
	}
	depths := teamTaskDepths(aggregate.Tasks)
	candidates := make([]workerCandidate, 0, len(aggregate.Tasks))
	for _, task := range aggregate.Tasks {
		if task.Status != team.TaskStatusReady || task.AssignedMemberID == "" ||
			resourceAttemptActive(resources.Attempts, task.ID) ||
			!dependencyResultsCaptured(task, aggregate.Tasks, resources.Attempts) {
			continue
		}
		member, found := members[task.AssignedMemberID]
		if !found || member.Status != team.MemberStatusActive ||
			member.CapabilityProfileRef == "" {
			continue
		}
		if _, memberBusy := busy[member.ID]; memberBusy {
			continue
		}

		attemptNumber := len(task.Attempts) + 1
		attemptID, continuationID := scheduledAttemptIdentities(
			aggregate.ID,
			task.ID,
			attemptNumber,
		)
		candidates = append(candidates, workerCandidate{
			key:            attemptKey{teamID: aggregate.ID, taskID: task.ID, attemptID: attemptID},
			continuationID: continuationID, memberID: member.ID,
			identityKey:   member.CapabilityProfileRef,
			topologyIndex: depths[task.ID], task: task, teamRevision: aggregate.Revision,
		})
	}
	slices.SortFunc(candidates, func(left, right workerCandidate) int {
		if left.topologyIndex != right.topologyIndex {
			return left.topologyIndex - right.topologyIndex
		}

		return strings.Compare(string(left.key.taskID), string(right.key.taskID))
	})
	selected := candidates[:0]
	selectedMembers := make(map[team.MemberID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, found := selectedMembers[candidate.memberID]; found {
			continue
		}
		selectedMembers[candidate.memberID] = struct{}{}
		selected = append(selected, candidate)
	}

	return selected
}

func teamTaskDepths(tasks []team.Task) map[team.TaskID]int {
	byID := make(map[team.TaskID]team.Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	depths := make(map[team.TaskID]int, len(tasks))
	var visit func(team.TaskID) int
	visit = func(id team.TaskID) int {
		if depth, found := depths[id]; found {
			return depth
		}
		depth := 0
		for _, dependency := range byID[id].DependencyIDs {
			depth = max(depth, visit(dependency)+1)
		}
		depths[id] = depth

		return depth
	}
	for id := range byID {
		visit(id)
	}

	return depths
}

func dependencyResultsCaptured(
	task team.Task,
	tasks []team.Task,
	resources []teamstate.AttemptResource,
) bool {
	if len(task.DependencyIDs) == 0 {
		return true
	}
	byID := make(map[team.TaskID]team.Task, len(tasks))
	for _, value := range tasks {
		byID[value.ID] = value
	}
	for _, dependencyID := range task.DependencyIDs {
		dependency, found := byID[dependencyID]
		if !found || dependency.Status != team.TaskStatusCompleted || len(dependency.Attempts) == 0 {
			return false
		}
		attempt := dependency.Attempts[len(dependency.Attempts)-1]
		captured := false
		for _, resource := range resources {
			if resource.TaskID == dependencyID && resource.AttemptID == attempt.ID &&
				(resource.State == teamstate.AttemptCaptured || resource.State == teamstate.AttemptTerminal) {
				captured = true
				break
			}
		}
		if !captured {
			return false
		}
	}

	return true
}

func resourceAttemptActive(resources []teamstate.AttemptResource, taskID team.TaskID) bool {
	for _, resource := range resources {
		if resource.TaskID != taskID {
			continue
		}
		switch resource.State {
		case teamstate.AttemptCaptured, teamstate.AttemptTerminal,
			teamstate.AttemptFailed, teamstate.AttemptCancelled,
			teamstate.AttemptConflicted, teamstate.AttemptCaptureFailed,
			teamstate.AttemptOrphaned:
		default:
			return true
		}
	}

	return false
}

func scheduledAttemptIdentities(
	teamID team.ID,
	taskID team.TaskID,
	number int,
) (team.AttemptID, continuation.ID) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", teamID, taskID, number)))
	token := hex.EncodeToString(sum[:16])

	return team.AttemptID("attempt-" + token), continuation.ID("continuation-" + token)
}
