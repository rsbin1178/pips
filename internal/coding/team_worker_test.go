package coding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerInitialPromptIncludesAuthoritativeMailboxInOrder(t *testing.T) {
	t.Parallel()

	prompt := workerInitialPrompt(team.Task{
		Title: "Fix the parser", Description: "Preserve compatibility.",
	}, []team.Message{
		{Sequence: 4, SenderID: "lead", Body: ai.JSON(`{"text":"Inspect the failing fixture."}`)},
		{Sequence: 5, SenderID: "reviewer", Body: ai.JSON(`{"priority":"high"}`)},
	})

	assert.Equal(t, `Fix the parser

Preserve compatibility.

Team mailbox messages received before this attempt (oldest first):

Message 4 from lead:
Inspect the failing fixture.

Message 5 from reviewer:
{"priority":"high"}`, prompt)
}

func TestCloneWorkerMailboxDoesNotAliasDurableBody(t *testing.T) {
	t.Parallel()

	body := ai.JSON(`{"text":"original"}`)
	cloned := cloneWorkerMailbox([]team.Message{{Body: body}})
	body[0] = '['

	assert.JSONEq(t, `{"text":"original"}`, string(cloned[0].Body))
}

func TestAttemptOwnerInboxAppliesBackpressure(t *testing.T) {
	t.Parallel()

	owner := &attemptOwner{
		inbox: make(chan ownerCommand, attemptOwnerInboxCapacity),
		done:  make(chan struct{}),
	}
	for range attemptOwnerInboxCapacity {
		owner.inbox <- ownerCommand{kind: ownerCommandFollowUp}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := owner.deliver(ctx, ownerCommand{kind: ownerCommandInterrupt})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Len(t, owner.inbox, attemptOwnerInboxCapacity)
}

func TestAttemptOwnerReportsUncertainDeliveryWhenOwnerExitsAfterReceive(t *testing.T) {
	t.Parallel()

	owner := &attemptOwner{
		inbox: make(chan ownerCommand, 1),
		done:  make(chan struct{}),
	}
	received := make(chan struct{})
	go func() {
		<-owner.inbox
		close(received)
		close(owner.done)
	}()

	err := owner.deliver(t.Context(), ownerCommand{kind: ownerCommandFollowUp})
	<-received

	var uncertain *ownerDeliveryUncertainError
	require.ErrorAs(t, err, &uncertain)
	assert.True(t, errors.Is(err, ErrTeamAdmission))
}

func TestCapturedDependencySelectionRequiresVerifiedResourceIdentity(t *testing.T) {
	t.Parallel()

	baseOID := strings.Repeat("1", 40)
	resultOID := strings.Repeat("2", 40)
	aggregate := team.Team{Tasks: []team.Task{{
		ID: "dependency", Status: team.TaskStatusCompleted,
		Attempts: []team.Attempt{{
			ID: "attempt-dependency", MemberID: "worker-a",
			ContinuationID: "continuation-dependency", Status: team.AttemptStatusCompleted,
		}},
	}}}
	resources := teamstate.Snapshot{
		Repository: teamstate.RepositoryResource{BaseOID: baseOID},
		Attempts: []teamstate.AttemptResource{{
			TaskID: "dependency", AttemptID: "attempt-dependency", MemberID: "worker-a",
			ContinuationID: "continuation-dependency", State: teamstate.AttemptCaptured,
			Worktree: teamstate.WorktreeResource{
				ResultRef: "refs/pips/team/result", ResultCommitOID: resultOID,
			},
		}},
	}
	request := AttemptBaseRequest{
		Team: aggregate, Resources: resources,
		Task: team.Task{ID: "consumer", DependencyIDs: []team.TaskID{"dependency"}},
	}

	require.NoError(t, validateCapturedDependencyInputs(request))
	request.Resources.Attempts[0].Worktree.ResultCommitOID = ""
	assert.ErrorIs(t, validateCapturedDependencyInputs(request), errDependencyBaseUnavailable)
}

func TestAttemptOwnerProjectsDependencyConflictWithoutOpeningRuntime(t *testing.T) {
	t.Parallel()

	engine, state, aggregate := newSchedulerTestState(t, []schedulerMemberTask{{
		member: "worker-a", profile: strings.Repeat("a", 64), task: "task-a",
	}})
	claimed, err := engine.ClaimTask(t.Context(), aggregate.ID, team.ClaimTaskRequest{
		Command: team.CommandMetadata{
			ID: "claim-conflict", ExpectedRevision: aggregate.Revision,
			Actor: team.Actor{Kind: team.ActorKindMember, ID: "worker-a"},
		},
		TaskID: "task-a",
	})
	require.NoError(t, err)
	started, err := engine.StartTaskAttempt(t.Context(), aggregate.ID, team.StartTaskAttemptRequest{
		Command: team.CommandMetadata{
			ID: "start-conflict", ExpectedRevision: claimed.Revision, Actor: attemptActor,
		},
		TaskID: "task-a", AttemptID: "attempt-conflict",
		ContinuationID: "continuation-conflict",
	})
	require.NoError(t, err)
	candidate := workerCandidate{
		key: attemptKey{
			teamID: aggregate.ID, taskID: "task-a", attemptID: "attempt-conflict",
		},
		memberID: "worker-a", continuationID: "continuation-conflict",
		task: started.Team.Tasks[0],
	}
	_, err = ensureAttemptResource(t.Context(), state, candidate)
	require.NoError(t, err)
	owner := &attemptOwner{
		factory: &attemptOwnerFactory{team: engine, state: state}, candidate: candidate,
	}

	require.NoError(t, owner.blockDependencyConflict(t.Context(), ErrAttemptBaseConflict))

	latest, err := engine.Get(t.Context(), aggregate.ID)
	require.NoError(t, err)
	require.Len(t, latest.Tasks, 1)
	assert.Equal(t, team.TaskStatusFailed, latest.Tasks[0].Status)
	assert.Equal(t, team.AttemptStatusFailed, latest.Tasks[0].Attempts[0].Status)
	snapshot, err := state.Load(t.Context(), aggregate.ID)
	require.NoError(t, err)
	assert.Equal(t, teamstate.StateBlockedConflict, snapshot.State)
	require.Len(t, snapshot.Attempts, 1)
	assert.Equal(t, teamstate.AttemptConflicted, snapshot.Attempts[0].State)
	assert.Empty(t, snapshot.Attempts[0].Session.SessionID)
	assert.Empty(t, snapshot.Attempts[0].Worktree.ID)
}
