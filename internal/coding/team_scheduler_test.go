package coding

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerAdmissionIsFIFOAndWorkConservingAcrossIdentityKeys(t *testing.T) {
	t.Parallel()

	admission, err := newWorkerAdmission(3, 1)
	require.NoError(t, err)
	a1 := schedulerTestCandidate("task-a1", "key-a")
	a2 := schedulerTestCandidate("task-a2", "key-a")
	b1 := schedulerTestCandidate("task-b1", "key-b")
	assert.True(t, admission.enqueue(a1))
	assert.True(t, admission.enqueue(a2))
	assert.True(t, admission.enqueue(b1))
	assert.False(t, admission.enqueue(a1))

	first, firstPermit, granted := admission.grant()
	require.True(t, granted)
	assert.Equal(t, a1.key, first.key)
	second, secondPermit, granted := admission.grant()
	require.True(t, granted)
	assert.Equal(t, b1.key, second.key)
	_, _, granted = admission.grant()
	assert.False(t, granted)

	firstPermit.release()
	third, thirdPermit, granted := admission.grant()
	require.True(t, granted)
	assert.Equal(t, a2.key, third.key)
	secondPermit.release()
	thirdPermit.release()
	active, queued, byKey := admission.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, queued)
	assert.Empty(t, byKey)
}

func TestWorkerAdmissionCancellationCreatesNoPermit(t *testing.T) {
	t.Parallel()

	admission, err := newWorkerAdmission(1, 1)
	require.NoError(t, err)
	candidate := schedulerTestCandidate("task-cancel", "key-a")
	require.True(t, admission.enqueue(candidate))
	assert.True(t, admission.cancel(candidate.key))
	assert.False(t, admission.cancel(candidate.key))
	_, _, granted := admission.grant()
	assert.False(t, granted)
	active, queued, _ := admission.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, queued)
}

func TestTeamSchedulerBoundsConcurrencyAndDoesNotStarveFIFO(t *testing.T) {
	t.Parallel()

	engine, state, aggregate := newSchedulerTestState(t, []schedulerMemberTask{
		{member: "worker-a", profile: strings.Repeat("a", 64), task: "task-a"},
		{member: "worker-b", profile: strings.Repeat("b", 64), task: "task-b"},
		{member: "worker-c", profile: strings.Repeat("c", 64), task: "task-c"},
	})
	factory := newScriptedCoordinatorFactory()
	coordinator, err := newTeamCoordinator(
		aggregate.ID,
		aggregate.LeadMemberID,
		engine,
		state,
		factory,
		2,
		1,
	)
	require.NoError(t, err)
	require.NoError(t, coordinator.start(t.Context()))
	t.Cleanup(func() { _ = coordinator.close(context.Background()) })

	assert.Equal(t, team.TaskID("task-a"), factory.nextStart(t))
	assert.Equal(t, team.TaskID("task-b"), factory.nextStart(t))
	active, queued, _ := coordinator.admission.snapshot()
	assert.Equal(t, 2, active)
	assert.Equal(t, 1, queued)

	factory.release("task-a")
	assert.Equal(t, team.TaskID("task-c"), factory.nextStart(t))
	require.Eventually(t, func() bool {
		active, queued, _ := coordinator.admission.snapshot()

		return active == 2 && queued == 0
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		latest, getErr := engine.Get(t.Context(), aggregate.ID)
		if getErr != nil {
			return false
		}
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()

		return coordinator.revision == latest.Revision
	}, time.Second, 10*time.Millisecond)
	factory.release("task-b")
	factory.release("task-c")
	require.Eventually(t, func() bool {
		active, _, _ := coordinator.admission.snapshot()

		return active == 0
	}, time.Second, 10*time.Millisecond)
}

func TestTeamSchedulerCancelRemovesWaitersAndStopsOwners(t *testing.T) {
	t.Parallel()

	engine, state, aggregate := newSchedulerTestState(t, []schedulerMemberTask{
		{member: "worker-a", profile: strings.Repeat("a", 64), task: "task-a"},
		{member: "worker-b", profile: strings.Repeat("b", 64), task: "task-b"},
	})
	factory := newScriptedCoordinatorFactory()
	coordinator, err := newTeamCoordinator(
		aggregate.ID,
		aggregate.LeadMemberID,
		engine,
		state,
		factory,
		1,
		1,
	)
	require.NoError(t, err)
	require.NoError(t, coordinator.start(t.Context()))
	t.Cleanup(func() { _ = coordinator.close(context.Background()) })
	assert.Equal(t, team.TaskID("task-a"), factory.nextStart(t))
	require.Eventually(t, func() bool {
		current, getErr := engine.Get(t.Context(), aggregate.ID)
		if getErr != nil {
			return false
		}
		task, found := taskByID(current.Tasks, "task-a")

		return found && task.Status == team.TaskStatusRunning
	}, time.Second, 10*time.Millisecond)

	latest, err := engine.Get(t.Context(), aggregate.ID)
	require.NoError(t, err)
	_, err = engine.CancelTeam(t.Context(), aggregate.ID, team.CancelTeamRequest{
		Command: team.CommandMetadata{
			ID: "cancel-team", ExpectedRevision: latest.Revision,
			Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
		},
		Reason: "test cancellation",
	})
	require.NoError(t, err)
	coordinator.signal()

	require.Eventually(t, func() bool {
		active, queued, _ := coordinator.admission.snapshot()

		return active == 0 && queued == 0 && coordinator.ownerCount() == 0
	}, time.Second, 10*time.Millisecond)
	select {
	case unexpected := <-factory.started:
		require.Fail(t, "queued Worker started after Team cancellation", string(unexpected))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTeamSchedulerOrdersByTopologyThenTaskID(t *testing.T) {
	t.Parallel()

	completedAttempt, completedContinuation := scheduledAttemptIdentities("team-1", "task-base", 1)
	aggregate := team.Team{
		ID: "team-1", Status: team.StatusActive,
		Members: []team.Member{
			{ID: "worker-a", Status: team.MemberStatusActive, CapabilityProfileRef: strings.Repeat("a", 64)},
			{ID: "worker-b", Status: team.MemberStatusActive, CapabilityProfileRef: strings.Repeat("b", 64)},
		},
		Tasks: []team.Task{
			{
				ID: "task-base", Status: team.TaskStatusCompleted,
				Attempts: []team.Attempt{{
					ID: completedAttempt, ContinuationID: completedContinuation,
					Status: team.AttemptStatusCompleted,
				}},
			},
			{ID: "task-z", Status: team.TaskStatusReady, AssignedMemberID: "worker-a"},
			{
				ID: "task-dependent", Status: team.TaskStatusReady,
				AssignedMemberID: "worker-b", DependencyIDs: []team.TaskID{"task-base"},
			},
		},
	}
	resources := teamstate.Snapshot{
		State: teamstate.StateActive,
		Attempts: []teamstate.AttemptResource{{
			TaskID: "task-base", AttemptID: completedAttempt,
			ContinuationID: completedContinuation, MemberID: "worker-a",
			State: teamstate.AttemptCaptured,
		}},
	}

	candidates := readyWorkerCandidates(aggregate, resources)
	require.Len(t, candidates, 2)
	assert.Equal(t, team.TaskID("task-z"), candidates[0].key.taskID)
	assert.Equal(t, team.TaskID("task-dependent"), candidates[1].key.taskID)
	assert.Less(t, candidates[0].topologyIndex, candidates[1].topologyIndex)
}

type schedulerMemberTask struct {
	member  team.MemberID
	profile string
	task    team.TaskID
}

func newSchedulerTestState(
	t *testing.T,
	values []schedulerMemberTask,
) (*team.Engine, *teamstate.Store, team.Team) {
	t.Helper()

	memory, err := team.NewMemoryStore()
	require.NoError(t, err)
	engine, err := team.New(memory)
	require.NoError(t, err)
	aggregate, err := engine.Create(t.Context(), team.CreateRequest{
		Command: team.CommandMetadata{
			ID: "create-team", Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
		},
		ID: "team-1", Objective: "Test deterministic scheduler behavior.",
		Lead:   team.MemberSpec{ID: "lead", Name: "Lead", Role: "coordinate"},
		Limits: team.Limits{MaxMembers: len(values) + 1, MaxActiveTasks: len(values)},
	})
	require.NoError(t, err)
	for index, value := range values {
		aggregate, err = engine.RegisterMember(t.Context(), aggregate.ID, team.RegisterMemberRequest{
			Command: team.CommandMetadata{
				ID: team.CommandID("register-" + value.member), ExpectedRevision: aggregate.Revision,
				Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
			},
			Member: team.MemberSpec{
				ID: value.member, Name: string(value.member), Role: "worker",
				CapabilityProfileRef: value.profile,
			},
		})
		require.NoError(t, err)
		aggregate, err = engine.CreateTask(t.Context(), aggregate.ID, team.CreateTaskRequest{
			Command: team.CommandMetadata{
				ID: team.CommandID("create-" + value.task), ExpectedRevision: aggregate.Revision,
				Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
			},
			TaskID: value.task, Title: "Task " + string(value.task), AttemptLimit: 3,
		})
		require.NoError(t, err, index)
		aggregate, err = engine.AssignTask(t.Context(), aggregate.ID, team.AssignTaskRequest{
			Command: team.CommandMetadata{
				ID: team.CommandID("assign-" + value.task), ExpectedRevision: aggregate.Revision,
				Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
			},
			TaskID: value.task, MemberID: value.member,
		})
		require.NoError(t, err, index)
	}

	stateDirectory := filepath.Join(t.TempDir(), "state")
	require.NoError(t, mkdirPrivate(stateDirectory))
	store, err := teamstate.New(stateDirectory, teamstate.Limits{})
	require.NoError(t, err)
	now := time.Now().UTC()
	members := []teamstate.MemberResource{{
		MemberID: "lead", CapabilityProfileFingerprint: strings.Repeat("0", 64),
	}}
	for _, value := range values {
		members = append(members, teamstate.MemberResource{
			MemberID: value.member, CapabilityProfileFingerprint: value.profile,
		})
	}
	_, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "state-active",
		Snapshot: teamstate.Snapshot{
			TeamID: aggregate.ID, Revision: 1, State: teamstate.StateActive,
			Parent: teamstate.ParentResource{
				SessionID: "s-parent", WorkspaceID: "workspace-1",
				Workspace: teamstate.FileIdentity{Path: t.TempDir()},
			},
			Repository: teamstate.RepositoryResource{
				CommonDir: teamstate.FileIdentity{Path: t.TempDir()},
				BaseOID:   strings.Repeat("1", 40), Admission: teamstate.AdmissionClean,
			},
			Members: members, Cleanup: teamstate.CleanupRetain,
			CreatedAt: now, UpdatedAt: now,
		},
	})
	require.NoError(t, err)

	return engine, store, aggregate
}

type scriptedCoordinatorFactory struct {
	mu       sync.Mutex
	started  chan team.TaskID
	releases map[team.TaskID]chan struct{}
}

func newScriptedCoordinatorFactory() *scriptedCoordinatorFactory {
	return &scriptedCoordinatorFactory{
		started: make(chan team.TaskID, 16), releases: make(map[team.TaskID]chan struct{}),
	}
}

func (f *scriptedCoordinatorFactory) NewOwner(
	_ context.Context,
	candidate workerCandidate,
) (coordinatorOwner, error) {
	release := make(chan struct{})
	f.mu.Lock()
	f.releases[candidate.key.taskID] = release
	f.mu.Unlock()
	f.started <- candidate.key.taskID

	return scriptedCoordinatorOwner{key: candidate.key, release: release}, nil
}

func (f *scriptedCoordinatorFactory) nextStart(t *testing.T) team.TaskID {
	t.Helper()

	select {
	case value := <-f.started:
		return value
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for Worker start")

		return ""
	}
}

func (f *scriptedCoordinatorFactory) release(id team.TaskID) {
	f.mu.Lock()
	value := f.releases[id]
	delete(f.releases, id)
	f.mu.Unlock()
	if value != nil {
		close(value)
	}
}

type scriptedCoordinatorOwner struct {
	key     attemptKey
	release <-chan struct{}
}

func (o scriptedCoordinatorOwner) Key() attemptKey { return o.key }

func (o scriptedCoordinatorOwner) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.release:
		return nil
	}
}

func schedulerTestCandidate(taskID team.TaskID, identity string) workerCandidate {
	attemptID, continuationID := scheduledAttemptIdentities("team-1", taskID, 1)

	return workerCandidate{
		key:            attemptKey{teamID: "team-1", taskID: taskID, attemptID: attemptID},
		continuationID: continuationID, memberID: team.MemberID("worker-" + taskID),
		identityKey: identity, task: team.Task{ID: taskID},
	}
}
