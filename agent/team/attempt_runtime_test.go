//nolint:wsl_v5 // Tests keep setup and assertions grouped by lifecycle phase.
package team

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type attemptTestWorker func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (worker attemptTestWorker) Run(ctx context.Context, request continuation.WorkRequest) (continuation.WorkResult, error) {
	return worker(ctx, request)
}

func newAttemptTestRuntime(t *testing.T, worker continuation.Worker, projector AttemptResultProjector) (*AttemptRuntime, *testRuntime) {
	t.Helper()
	teamRuntime := newMemoryTestRuntime(t)
	continuationStore, err := continuation.NewMemoryStore()
	require.NoError(t, err)
	continuations, err := continuation.New(continuationStore)
	require.NoError(t, err)
	runtime, err := NewAttemptRuntime(
		teamRuntime.engine, continuations,
		AttemptWorkerFactoryFunc(func(context.Context, AttemptInput) (PreparedAttempt, error) {
			return PreparedAttempt{Worker: worker, Input: ai.JSON(`{"work":true}`)}, nil
		}),
		projector,
		WithAttemptCoordinator(Actor{Kind: ActorKindCoordinator, ID: "test-coordinator"}),
		WithAttemptHandlers(
			continuation.HandlerRef{Kind: "attempt-test-worker", Version: "v1"},
			DefaultAttemptControllerRef(), CompleteAfterWork{},
		),
	)
	require.NoError(t, err)
	return runtime, teamRuntime
}

func createAssignedAttemptTask(t *testing.T, teamRuntime *testRuntime) Team {
	t.Helper()
	team := registerWorker(t, teamRuntime, createTestTeam(t, teamRuntime))
	var err error
	team, err = teamRuntime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: teamRuntime.coordinator(team.Revision), TaskID: "task", Title: "Run task", AttemptLimit: 2,
	})
	require.NoError(t, err)
	team, err = teamRuntime.engine.AssignTask(t.Context(), team.ID, AssignTaskRequest{
		Command: teamRuntime.member("lead", team.Revision), TaskID: "task", MemberID: "worker",
	})
	require.NoError(t, err)
	return team
}

func TestAttemptRuntimeRunsDurableLifecycleInOrder(t *testing.T) {
	t.Parallel()
	worker := attemptTestWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
		return continuation.WorkResult{Value: ai.JSON(`{"answer":"done"}`), Progress: continuation.ProgressChanged}, nil
	})
	projector := AttemptResultProjectorFunc(func(_ context.Context, input AttemptInput, execution continuation.Execution) (AttemptCompletion, error) {
		assert.Equal(t, MemberID("worker"), input.Dispatch.MemberID)
		assert.Equal(t, continuation.StatusCompleted, execution.Status)
		return AttemptCompletion{
			Outcome: AttemptOutcomeCompleted, Result: execution.Output, AcknowledgeMailbox: true,
			Messages: []AttemptMessage{{ID: "result-message", RecipientID: "lead", Body: ai.JSON(`{"status":"done"}`)}},
		}, nil
	})
	runtime, teamRuntime := newAttemptTestRuntime(t, worker, projector)
	team := createAssignedAttemptTask(t, teamRuntime)
	_, err := teamRuntime.engine.SendMessage(t.Context(), team.ID, SendMessageRequest{
		Command: teamRuntime.member("lead", team.Revision), MessageID: "inbox-message", RecipientID: "worker", TaskID: "task", Body: ai.JSON(`{"request":"go"}`),
	})
	require.NoError(t, err)

	result, err := runtime.Run(t.Context(), AttemptRunRequest{TeamID: team.ID, TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1"})
	require.NoError(t, err)
	assert.True(t, result.Finished)
	assert.Equal(t, continuation.StatusCompleted, result.Execution.Status)
	assert.Equal(t, TaskStatusCompleted, result.Team.Tasks[0].Status)
	assert.Equal(t, uint64(1), result.Team.Members[1].MailboxAcknowledged)
	leadMailbox, err := teamRuntime.engine.Mailbox(
		t.Context(),
		team.ID,
		"lead",
		MailboxOptions{Limit: 10},
	)
	require.NoError(t, err)
	require.Len(t, leadMailbox.Messages, 1)
	assert.Equal(t, MemberID("worker"), leadMailbox.Messages[0].SenderID)
	assert.Equal(t, TaskID("task"), leadMailbox.Messages[0].TaskID)

	history, err := teamRuntime.engine.History(t.Context(), team.ID)
	require.NoError(t, err)
	causes := make([]Cause, 0, len(history))
	for _, record := range history {
		causes = append(causes, record.Transition.Cause)
	}
	assert.Equal(t, []Cause{
		CauseCreate, CauseMemberRegistered, CauseTaskCreated, CauseTaskAssigned,
		CauseMessageSent, CauseTaskClaimed, CauseTaskAttemptStarted,
		CauseMessagesAcknowledged, CauseMessageSent, CauseTaskAttemptCompleted,
	}, causes)
}

func TestAttemptRuntimeCreatesMissingChildAfterCommittedStart(t *testing.T) {
	t.Parallel()
	worker := attemptTestWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
		return continuation.WorkResult{Value: ai.JSON(`{"ok":true}`), Progress: continuation.ProgressChanged}, nil
	})
	runtime, teamRuntime := newAttemptTestRuntime(t, worker, AttemptResultProjectorFunc(func(_ context.Context, _ AttemptInput, execution continuation.Execution) (AttemptCompletion, error) {
		return AttemptCompletion{Outcome: AttemptOutcomeCompleted, Result: execution.Output}, nil
	}))
	team := createAssignedAttemptTask(t, teamRuntime)
	team, err := teamRuntime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{Command: teamRuntime.member("worker", team.Revision), TaskID: "task"})
	require.NoError(t, err)
	started, err := teamRuntime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: teamRuntime.coordinator(team.Revision), TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1",
	})
	require.NoError(t, err)

	result, err := runtime.Run(t.Context(), AttemptRunRequest{TeamID: team.ID, TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1"})
	require.NoError(t, err)
	assert.True(t, result.Finished)
	assert.Equal(t, started.Dispatch.ContinuationID, result.Execution.ID)
}

func TestAttemptRuntimeLeavesAttemptRunningWhenProjectionFails(t *testing.T) {
	t.Parallel()
	worker := attemptTestWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
		return continuation.WorkResult{Value: ai.JSON(`{"ok":true}`), Progress: continuation.ProgressChanged}, nil
	})
	runtime, teamRuntime := newAttemptTestRuntime(t, worker, AttemptResultProjectorFunc(func(context.Context, AttemptInput, continuation.Execution) (AttemptCompletion, error) {
		return AttemptCompletion{}, errors.New("project result")
	}))
	team := createAssignedAttemptTask(t, teamRuntime)

	_, err := runtime.Run(t.Context(), AttemptRunRequest{TeamID: team.ID, TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1"})
	require.EqualError(t, err, "project result")
	current, err := teamRuntime.engine.Get(t.Context(), team.ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusRunning, current.Tasks[0].Status)
	assert.Equal(t, AttemptStatusRunning, current.Tasks[0].Attempts[0].Status)
}

func TestAttemptRuntimeLeavesAttemptRunningWhenWorkerFails(t *testing.T) {
	t.Parallel()
	worker := attemptTestWorker(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
		return continuation.WorkResult{}, errors.New("worker failed")
	})
	runtime, teamRuntime := newAttemptTestRuntime(t, worker, AttemptResultProjectorFunc(func(context.Context, AttemptInput, continuation.Execution) (AttemptCompletion, error) {
		return AttemptCompletion{}, errors.New("must not project")
	}))
	team := createAssignedAttemptTask(t, teamRuntime)

	result, err := runtime.Run(t.Context(), AttemptRunRequest{TeamID: team.ID, TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1"})
	require.EqualError(t, err, "worker failed")
	assert.Equal(t, continuation.StatusInterrupted, result.Execution.Status)
	current, getErr := teamRuntime.engine.Get(t.Context(), team.ID)
	require.NoError(t, getErr)
	assert.Equal(t, TaskStatusRunning, current.Tasks[0].Status)
}

func TestAttemptRuntimeHonorsCancellationWithoutFinishing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := attemptTestWorker(func(ctx context.Context, _ continuation.WorkRequest) (continuation.WorkResult, error) {
		cancel()
		<-ctx.Done()
		return continuation.WorkResult{}, ctx.Err()
	})
	runtime, teamRuntime := newAttemptTestRuntime(t, worker, AttemptResultProjectorFunc(func(context.Context, AttemptInput, continuation.Execution) (AttemptCompletion, error) {
		return AttemptCompletion{}, errors.New("must not project")
	}))
	team := createAssignedAttemptTask(t, teamRuntime)

	_, err := runtime.Run(ctx, AttemptRunRequest{TeamID: team.ID, TaskID: "task", AttemptID: "attempt-1", ContinuationID: "execution-1"})
	require.ErrorIs(t, err, context.Canceled)
	current, getErr := teamRuntime.engine.Get(t.Context(), team.ID)
	require.NoError(t, getErr)
	assert.Equal(t, TaskStatusRunning, current.Tasks[0].Status)
}
