package coding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

const teamStateConflictRetries = 32

type attemptResourceMutation func(*teamstate.AttemptResource) error
type attemptSnapshotMutation func(*teamstate.Snapshot, *teamstate.AttemptResource) error

func ensureAttemptResource(
	ctx context.Context,
	store *teamstate.Store,
	candidate workerCandidate,
) (teamstate.AttemptResource, error) {
	return mutateAttemptResource(ctx, store, candidate.key, "planned", func(
		resource *teamstate.AttemptResource,
	) error {
		if resource.AttemptID != "" {
			return validateAttemptResourceIdentity(*resource, candidate)
		}

		*resource = teamstate.AttemptResource{
			TaskID: candidate.key.taskID, AttemptID: candidate.key.attemptID,
			MemberID: candidate.memberID, ContinuationID: candidate.continuationID,
			State: teamstate.AttemptPlanned, Cleanup: teamstate.CleanupRetain,
		}

		return nil
	})
}

func transitionAttemptResource(
	ctx context.Context,
	store *teamstate.Store,
	candidate workerCandidate,
	action string,
	state teamstate.AttemptState,
	change attemptResourceMutation,
) (teamstate.AttemptResource, error) {
	return mutateAttemptResource(ctx, store, candidate.key, action, func(
		resource *teamstate.AttemptResource,
	) error {
		if err := validateAttemptResourceIdentity(*resource, candidate); err != nil {
			return err
		}
		if change != nil {
			if err := change(resource); err != nil {
				return err
			}
		}
		resource.State = state

		return nil
	})
}

func mutateAttemptResource(
	ctx context.Context,
	store *teamstate.Store,
	key attemptKey,
	action string,
	change attemptResourceMutation,
) (teamstate.AttemptResource, error) {
	if change == nil {
		return teamstate.AttemptResource{}, fmt.Errorf(
			"%w: invalid Attempt resource mutation",
			ErrTeamAdmission,
		)
	}

	return mutateAttemptSnapshot(ctx, store, key, action, func(
		_ *teamstate.Snapshot,
		resource *teamstate.AttemptResource,
	) error {
		return change(resource)
	})
}

func mutateAttemptSnapshot(
	ctx context.Context,
	store *teamstate.Store,
	key attemptKey,
	action string,
	change attemptSnapshotMutation,
) (teamstate.AttemptResource, error) {
	if store == nil || key.teamID == "" || key.taskID == "" || key.attemptID == "" || change == nil {
		return teamstate.AttemptResource{}, fmt.Errorf("%w: invalid Attempt resource mutation", ErrTeamAdmission)
	}

	for range teamStateConflictRetries {
		if err := ctx.Err(); err != nil {
			return teamstate.AttemptResource{}, err
		}
		snapshot, err := store.Load(ctx, key.teamID)
		if err != nil {
			return teamstate.AttemptResource{}, err
		}
		next := cloneTeamResourceSnapshot(snapshot)
		index := attemptResourceIndex(next.Attempts, key.attemptID)
		if index < 0 {
			next.Attempts = append(next.Attempts, teamstate.AttemptResource{})
			index = len(next.Attempts) - 1
		}
		before := next.Attempts[index]
		if err := change(&next, &next.Attempts[index]); err != nil {
			return teamstate.AttemptResource{}, err
		}
		if next.Attempts[index] == before {
			return next.Attempts[index], nil
		}

		next.Revision = snapshot.Revision + 1
		next.UpdatedAt = time.Now().UTC()
		commandID := admissionCommandID(
			key.teamID,
			fmt.Sprintf("attempt-%s-%d", action, snapshot.Revision),
			string(key.attemptID),
		)
		committed, err := store.Commit(ctx, teamstate.Mutation{
			CommandID: commandID, ExpectedRevision: snapshot.Revision, Snapshot: next,
		})
		if errors.Is(err, teamstate.ErrConflict) {
			continue
		}
		if err != nil {
			return teamstate.AttemptResource{}, err
		}

		index = attemptResourceIndex(committed.Attempts, key.attemptID)
		if index < 0 {
			return teamstate.AttemptResource{}, fmt.Errorf("%w: committed Attempt resource is missing", ErrTeamAdmission)
		}

		return committed.Attempts[index], nil
	}

	return teamstate.AttemptResource{}, fmt.Errorf("%w: Attempt resource conflict retry limit", ErrTeamAdmission)
}

func loadAttemptResource(
	ctx context.Context,
	store *teamstate.Store,
	key attemptKey,
) (teamstate.Snapshot, teamstate.AttemptResource, error) {
	snapshot, err := store.Load(ctx, key.teamID)
	if err != nil {
		return teamstate.Snapshot{}, teamstate.AttemptResource{}, err
	}
	index := attemptResourceIndex(snapshot.Attempts, key.attemptID)
	if index < 0 {
		return snapshot, teamstate.AttemptResource{}, teamstate.ErrNotFound
	}

	return snapshot, snapshot.Attempts[index], nil
}

func validateAttemptResourceIdentity(
	resource teamstate.AttemptResource,
	candidate workerCandidate,
) error {
	if resource.TaskID != candidate.key.taskID || resource.AttemptID != candidate.key.attemptID ||
		resource.MemberID != candidate.memberID || resource.ContinuationID != candidate.continuationID {
		return fmt.Errorf("%w: Attempt resource identity mismatch", ErrTeamAdmission)
	}

	return nil
}

func attemptResourceIndex(values []teamstate.AttemptResource, id team.AttemptID) int {
	for index := range values {
		if values[index].AttemptID == id {
			return index
		}
	}

	return -1
}

func cloneTeamResourceSnapshot(value teamstate.Snapshot) teamstate.Snapshot {
	value.Members = slices.Clone(value.Members)
	value.Attempts = slices.Clone(value.Attempts)
	integrations := value.Integrations
	value.Integrations = make([]teamstate.IntegrationResource, len(integrations))
	for index := range integrations {
		value.Integrations[index] = integrations[index]
		value.Integrations[index].AttemptIDs = slices.Clone(integrations[index].AttemptIDs)
	}

	return value
}
