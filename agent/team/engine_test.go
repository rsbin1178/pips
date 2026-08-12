package team

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testRuntime struct {
	engine   *Engine
	commands atomic.Int64
}

func newTestRuntime(t *testing.T, store Store) *testRuntime {
	t.Helper()

	var events atomic.Int64

	engine, err := New(
		store,
		WithClock(ClockFunc(func() time.Time {
			return time.Date(2026, time.July, 19, 9, 0, 0, 0, time.UTC)
		})),
		WithEventIDSource(func(time.Time) (EventID, error) {
			return EventID(fmt.Sprintf("event-%d", events.Add(1))), nil
		}),
	)
	require.NoError(t, err)

	return &testRuntime{engine: engine}
}

func newMemoryTestRuntime(t *testing.T) *testRuntime {
	t.Helper()

	store, err := NewMemoryStore()
	require.NoError(t, err)

	return newTestRuntime(t, store)
}

func (runtime *testRuntime) coordinator(revision Revision) CommandMetadata {
	return CommandMetadata{
		ID:               CommandID(fmt.Sprintf("command-%d", runtime.commands.Add(1))),
		ExpectedRevision: revision,
		Actor:            Actor{Kind: ActorKindCoordinator, ID: "test-coordinator"},
	}
}

func (runtime *testRuntime) member(id MemberID, revision Revision) CommandMetadata {
	return CommandMetadata{
		ID:               CommandID(fmt.Sprintf("command-%d", runtime.commands.Add(1))),
		ExpectedRevision: revision,
		Actor:            Actor{Kind: ActorKindMember, ID: string(id)},
	}
}

func createTestTeam(t *testing.T, runtime *testRuntime) Team {
	t.Helper()

	team, err := runtime.engine.Create(t.Context(), CreateRequest{
		Command: runtime.coordinator(0), ID: "team-1", Objective: "Ship a reliable change",
		Lead: MemberSpec{
			ID: "lead", Name: "Lead", Role: "coordinate and implement",
		},
	})
	require.NoError(t, err)

	return team
}

func registerWorker(t *testing.T, runtime *testRuntime, team Team) Team {
	t.Helper()

	team, err := runtime.engine.RegisterMember(t.Context(), team.ID, RegisterMemberRequest{
		Command: runtime.coordinator(team.Revision),
		Member: MemberSpec{
			ID: "worker", Name: "Worker", Role: "implement",
		},
	})
	require.NoError(t, err)

	return team
}

func TestCreateIsIdempotentAndRejectsCommandReuse(t *testing.T) {
	t.Parallel()

	store, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)

	request := CreateRequest{
		Command: runtime.coordinator(0), ID: "team-idempotent", Objective: "objective",
		Lead: MemberSpec{ID: "lead", Name: "Lead", Role: "lead"},
	}
	created, err := runtime.engine.Create(t.Context(), request)
	require.NoError(t, err)

	replayed, err := runtime.engine.Create(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, created, replayed)

	request.Objective = "different"
	_, err = runtime.engine.Create(t.Context(), request)
	require.ErrorIs(t, err, ErrCommandConflict)

	history, err := store.History(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Len(t, history, 1)
}

func TestTaskAttemptLifecyclePromotesDependenciesAndReplaysExactRecord(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	var err error

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "foundation",
		Title: "Build foundation", Payload: ai.JSON(`{"scope":"core"}`), AttemptLimit: 2,
	})
	require.NoError(t, err)

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "integration",
		Title: "Integrate", Dependencies: []TaskID{"foundation"}, AttemptLimit: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, TaskStatusPending, team.Tasks[1].Status)

	team, err = runtime.engine.AssignTask(t.Context(), team.ID, AssignTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "foundation", MemberID: "worker",
	})
	require.NoError(t, err)

	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "foundation",
	})
	require.NoError(t, err)

	started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "foundation",
		AttemptID: "attempt-1", ContinuationID: "execution-1",
	})
	require.NoError(t, err)
	assert.Equal(t, started.Team.Revision, started.Dispatch.TeamRevision)
	assert.Equal(t, MemberID("worker"), started.Dispatch.MemberID)

	finishCommand := runtime.member("worker", started.Team.Revision)
	finish := FinishTaskAttemptRequest{
		Command: finishCommand, TaskID: "foundation", AttemptID: "attempt-1",
		ContinuationID: "execution-1", Outcome: AttemptOutcomeCompleted,
		Result: ai.JSON(`{"ok":true}`),
	}
	completed, err := runtime.engine.FinishTaskAttempt(t.Context(), team.ID, finish)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusCompleted, completed.Tasks[0].Status)
	assert.Equal(t, TaskStatusReady, completed.Tasks[1].Status)

	finish.Command.ExpectedRevision = completed.Revision + 20
	replayed, err := runtime.engine.FinishTaskAttempt(t.Context(), team.ID, finish)
	require.NoError(t, err)
	assert.Equal(t, completed, replayed)

	_, err = runtime.engine.FinishTaskAttempt(t.Context(), team.ID, FinishTaskAttemptRequest{
		Command: runtime.coordinator(completed.Revision), TaskID: "foundation",
		AttemptID: "attempt-1", ContinuationID: "execution-1",
		Outcome: AttemptOutcomeCompleted,
	})
	require.ErrorIs(t, err, ErrStaleAttempt)
}

func TestFailedTaskMustBeRetriedOrCancelledBeforeCompletion(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	var err error

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task", Title: "Task", AttemptLimit: 1,
	})
	require.NoError(t, err)
	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		AttemptID: "attempt", ContinuationID: "execution",
	})
	require.NoError(t, err)
	team, err = runtime.engine.FinishTaskAttempt(t.Context(), team.ID, FinishTaskAttemptRequest{
		Command: runtime.member("worker", started.Team.Revision), TaskID: "task",
		AttemptID: "attempt", ContinuationID: "execution",
		Outcome: AttemptOutcomeFailed, Reason: "test failed",
	})
	require.NoError(t, err)

	_, err = runtime.engine.CompleteTeam(t.Context(), team.ID, CompleteTeamRequest{
		Command: runtime.member("lead", team.Revision),
	})
	require.ErrorIs(t, err, ErrInvalidState)

	_, err = runtime.engine.RetryTask(t.Context(), team.ID, RetryTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task",
	})
	require.ErrorIs(t, err, ErrAttemptLimit)

	team, err = runtime.engine.CancelTask(t.Context(), team.ID, CancelTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task", Reason: "accepted failure",
	})
	require.NoError(t, err)
	team, err = runtime.engine.CompleteTeam(t.Context(), team.ID, CompleteTeamRequest{
		Command: runtime.member("lead", team.Revision), Output: ai.JSON(`{"done":true}`),
	})
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, team.Status)
}

func TestMailboxOrderingAcknowledgementAndScopedSender(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	sent, err := runtime.engine.SendMessage(t.Context(), team.ID, SendMessageRequest{
		Command: runtime.member("lead", team.Revision), MessageID: "message-1",
		RecipientID: "worker", Body: ai.JSON(`{"text":"begin"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, MemberID("lead"), sent.Message.SenderID)

	page, err := runtime.engine.Mailbox(t.Context(), team.ID, "worker", MailboxOptions{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, uint64(1), page.NextAfter)

	team, err = runtime.engine.AcknowledgeMessages(t.Context(), team.ID, AcknowledgeMessagesRequest{
		Command: runtime.member("worker", sent.Team.Revision), ThroughSequence: page.NextAfter,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), team.Members[1].MailboxAcknowledged)

	_, err = runtime.engine.AcknowledgeMessages(t.Context(), team.ID, AcknowledgeMessagesRequest{
		Command: runtime.member("worker", team.Revision), ThroughSequence: 1,
	})
	require.ErrorIs(t, err, ErrInvalidState)

	_, err = runtime.engine.SendMessage(t.Context(), team.ID, SendMessageRequest{
		Command: runtime.coordinator(team.Revision), MessageID: "message-2",
		RecipientID: "worker", Body: ai.JSON(`{"text":"spoof"}`),
	})
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestMessageReplayConflictAndConcurrentSequence(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	group := registerWorker(t, runtime, createTestTeam(t, runtime))
	request := SendMessageRequest{
		Command: runtime.member("lead", group.Revision), MessageID: "message-replay",
		RecipientID: "worker", Body: ai.JSON(`{"text":"stable"}`),
	}
	sent, err := runtime.engine.SendMessage(t.Context(), group.ID, request)
	require.NoError(t, err)
	replayed, err := runtime.engine.SendMessage(t.Context(), group.ID, request)
	require.NoError(t, err)
	assert.Equal(t, sent, replayed)

	request.Body = ai.JSON(`{"text":"changed"}`)
	_, err = runtime.engine.SendMessage(t.Context(), group.ID, request)
	require.ErrorIs(t, err, ErrCommandConflict)

	requests := []SendMessageRequest{
		{
			Command: CommandMetadata{
				ID: "concurrent-message-a", ExpectedRevision: sent.Team.Revision,
				Actor: Actor{Kind: ActorKindMember, ID: "lead"},
			},
			MessageID: "message-a", RecipientID: "worker", Body: ai.JSON(`{"text":"a"}`),
		},
		{
			Command: CommandMetadata{
				ID: "concurrent-message-b", ExpectedRevision: sent.Team.Revision,
				Actor: Actor{Kind: ActorKindMember, ID: "lead"},
			},
			MessageID: "message-b", RecipientID: "worker", Body: ai.JSON(`{"text":"b"}`),
		},
	}
	errorsByRequest := make([]error, len(requests))

	var wait sync.WaitGroup
	wait.Add(len(requests))

	for index := range requests {
		go func() {
			defer wait.Done()

			_, errorsByRequest[index] = runtime.engine.SendMessage(t.Context(), group.ID, requests[index])
		}()
	}

	wait.Wait()

	successes := 0
	conflicts := 0

	for _, sendErr := range errorsByRequest {
		switch {
		case sendErr == nil:
			successes++
		case errors.Is(sendErr, ErrConflict):
			conflicts++
		default:
			require.NoError(t, sendErr)
		}
	}

	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)

	mailbox, err := runtime.engine.Mailbox(t.Context(), group.ID, "worker", MailboxOptions{})
	require.NoError(t, err)
	require.Len(t, mailbox.Messages, 2)
	assert.Equal(t, uint64(1), mailbox.Messages[0].Sequence)
	assert.Equal(t, uint64(2), mailbox.Messages[1].Sequence)
}

type executionReaderFunc func(context.Context, continuation.ID) (continuation.Execution, error)

func (function executionReaderFunc) Get(
	ctx context.Context,
	id continuation.ID,
) (continuation.Execution, error) {
	return function(ctx, id)
}

func TestCancellationDispatchAndFiniteInspection(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	var err error

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task", Title: "Task", AttemptLimit: 2,
	})
	require.NoError(t, err)
	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		AttemptID: "attempt", ContinuationID: "execution",
	})
	require.NoError(t, err)

	inspections, err := runtime.engine.InspectActiveAttempts(
		t.Context(), team.ID,
		executionReaderFunc(func(context.Context, continuation.ID) (continuation.Execution, error) {
			return continuation.Execution{}, continuation.ErrNotFound
		}),
	)
	require.NoError(t, err)
	require.Len(t, inspections, 1)
	assert.Equal(t, AttemptInspectionMissing, inspections[0].State)

	team, err = runtime.engine.CancelTeam(t.Context(), team.ID, CancelTeamRequest{
		Command: runtime.coordinator(started.Team.Revision), Reason: "coordinator shutdown",
	})
	require.NoError(t, err)
	cancellations, err := runtime.engine.CancellationDispatches(t.Context(), team.ID)
	require.NoError(t, err)
	require.Len(t, cancellations, 1)
	assert.Equal(t, continuation.ID("execution"), cancellations[0].ContinuationID)

	_, err = runtime.engine.FinishTaskAttempt(t.Context(), team.ID, FinishTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task", AttemptID: "attempt",
		ContinuationID: "execution", Outcome: AttemptOutcomeCompleted,
	})
	require.ErrorIs(t, err, ErrTerminal)
}

func TestConcurrentTaskClaimHasOneWinner(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	var err error

	team, err = runtime.engine.RegisterMember(t.Context(), team.ID, RegisterMemberRequest{
		Command: runtime.coordinator(team.Revision),
		Member: MemberSpec{
			ID: "worker-2", Name: "Worker Two", Role: "implement",
		},
	})
	require.NoError(t, err)
	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "race-task",
		Title: "Claim once", AttemptLimit: 1,
	})
	require.NoError(t, err)

	requests := []ClaimTaskRequest{
		{Command: runtime.member("worker", team.Revision), TaskID: "race-task"},
		{Command: runtime.member("worker-2", team.Revision), TaskID: "race-task"},
	}
	errorsByClaim := make([]error, len(requests))

	var wait sync.WaitGroup
	wait.Add(len(requests))

	for index := range requests {
		go func() {
			defer wait.Done()

			_, errorsByClaim[index] = runtime.engine.ClaimTask(t.Context(), team.ID, requests[index])
		}()
	}

	wait.Wait()

	successes := 0
	conflicts := 0

	for _, claimErr := range errorsByClaim {
		switch {
		case claimErr == nil:
			successes++
		case errors.Is(claimErr, ErrConflict):
			conflicts++
		default:
			require.NoError(t, claimErr)
		}
	}

	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	loaded, err := runtime.engine.Get(t.Context(), team.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, loaded.Tasks[0].ClaimedMemberID)
}

func TestMemberDisableRequiresCoordinatorAndIdleMember(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	_, err := runtime.engine.DisableMember(t.Context(), team.ID, DisableMemberRequest{
		Command: runtime.coordinator(team.Revision), MemberID: "lead", Reason: "not allowed",
	})
	require.ErrorIs(t, err, ErrUnauthorized)

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task", Title: "Task", AttemptLimit: 1,
	})
	require.NoError(t, err)
	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)

	_, err = runtime.engine.DisableMember(t.Context(), team.ID, DisableMemberRequest{
		Command: runtime.coordinator(team.Revision), MemberID: "worker", Reason: "maintenance",
	})
	require.ErrorIs(t, err, ErrMemberBusy)

	team, err = runtime.engine.ReleaseTask(t.Context(), team.ID, ReleaseTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	team, err = runtime.engine.DisableMember(t.Context(), team.ID, DisableMemberRequest{
		Command: runtime.coordinator(team.Revision), MemberID: "worker", Reason: "maintenance",
	})
	require.NoError(t, err)

	_, err = runtime.engine.SendMessage(t.Context(), team.ID, SendMessageRequest{
		Command: runtime.member("worker", team.Revision), MessageID: "disabled-message",
		RecipientID: "lead", Body: ai.JSON(`{"text":"not sent"}`),
	})
	require.ErrorIs(t, err, ErrUnauthorized)

	team, err = runtime.engine.EnableMember(t.Context(), team.ID, EnableMemberRequest{
		Command: runtime.coordinator(team.Revision), MemberID: "worker",
	})
	require.NoError(t, err)
	assert.Equal(t, MemberStatusActive, team.Members[1].Status)
}

func TestTaskAssignmentRetryAndSecondAttemptCompletion(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	var err error

	team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task", Title: "Task", AttemptLimit: 2,
	})
	require.NoError(t, err)
	team, err = runtime.engine.AssignTask(t.Context(), team.ID, AssignTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task", MemberID: "lead",
	})
	require.NoError(t, err)
	team, err = runtime.engine.AssignTask(t.Context(), team.ID, AssignTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task", MemberID: "worker",
	})
	require.NoError(t, err)
	team, err = runtime.engine.UnassignTask(t.Context(), team.ID, UnassignTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	team, err = runtime.engine.AssignTask(t.Context(), team.ID, AssignTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task", MemberID: "worker",
	})
	require.NoError(t, err)

	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		AttemptID: "attempt-1", ContinuationID: "execution-1",
	})
	require.NoError(t, err)
	team, err = runtime.engine.FinishTaskAttempt(t.Context(), team.ID, FinishTaskAttemptRequest{
		Command: runtime.member("worker", started.Team.Revision), TaskID: "task",
		AttemptID: "attempt-1", ContinuationID: "execution-1",
		Outcome: AttemptOutcomeFailed, Reason: "retryable",
	})
	require.NoError(t, err)
	team, err = runtime.engine.RetryTask(t.Context(), team.ID, RetryTaskRequest{
		Command: runtime.member("lead", team.Revision), TaskID: "task", Reason: "try again",
	})
	require.NoError(t, err)
	assert.Equal(t, TaskStatusReady, team.Tasks[0].Status)

	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	started, err = runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		AttemptID: "attempt-2", ContinuationID: "execution-2",
	})
	require.NoError(t, err)
	team, err = runtime.engine.FinishTaskAttempt(t.Context(), team.ID, FinishTaskAttemptRequest{
		Command: runtime.member("worker", started.Team.Revision), TaskID: "task",
		AttemptID: "attempt-2", ContinuationID: "execution-2",
		Outcome: AttemptOutcomeCompleted,
	})
	require.NoError(t, err)
	require.Len(t, team.Tasks[0].Attempts, 2)

	team, err = runtime.engine.CompleteTeam(t.Context(), team.ID, CompleteTeamRequest{
		Command: runtime.member("lead", team.Revision),
	})
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, team.Status)
}

func TestFailTeamIsExplicitAndTerminal(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := createTestTeam(t, runtime)
	failed, err := runtime.engine.FailTeam(t.Context(), team.ID, FailTeamRequest{
		Command: runtime.member("lead", team.Revision), Reason: "objective cannot be met",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, failed.Status)

	_, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(failed.Revision), TaskID: "late", Title: "Late", AttemptLimit: 1,
	})
	require.ErrorIs(t, err, ErrTerminal)
}
