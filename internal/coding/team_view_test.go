package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadTeamReturnsFreshSnapshotThenBoundedChanges(t *testing.T) {
	t.Parallel()

	runtime, engine, aggregate := newTeamViewRuntime(t)
	view, err := runtime.ReadTeam(t.Context(), TeamReadRequest{TeamID: aggregate.ID})
	require.NoError(t, err)

	assert.Equal(t, aggregate.Revision, view.Revision)
	assert.Equal(t, aggregate.Revision, view.ChangeCursor)
	assert.Empty(t, view.Changes)
	assert.False(t, view.ChangesPending)
	assert.Len(t, view.Members, 2)
	require.Len(t, view.Tasks, 1)
	assert.Equal(t, team.TaskID("task-one"), view.Tasks[0].ID)
	assert.Equal(t, teamstate.StateActive, view.ResourceState)
	view.Tasks[0].DependencyIDs = []team.TaskID{"task-one"}
	cloned := view.Clone()
	cloned.Members[0].Name = "changed"
	cloned.Tasks[0].DependencyIDs[0] = "dependency-changed"
	assert.NotEqual(t, cloned.Members[0].Name, view.Members[0].Name)
	assert.Equal(t, team.TaskID("task-one"), view.Tasks[0].DependencyIDs[0])
	invalid := view
	invalid.ChangeCursor = invalid.Revision + 1
	require.ErrorIs(t, validateTeamView(invalid), ErrTeamReadUnavailable)

	updated, err := engine.AssignTask(t.Context(), aggregate.ID, team.AssignTaskRequest{
		Command:  teamViewCommand("assign-task", aggregate.Revision),
		TaskID:   "task-one",
		MemberID: "worker",
	})
	require.NoError(t, err)

	view, err = runtime.ReadTeam(t.Context(), TeamReadRequest{
		TeamID:        aggregate.ID,
		AfterRevision: aggregate.Revision,
	})
	require.NoError(t, err)
	require.Len(t, view.Changes, 1)
	assert.Equal(t, team.CauseTaskAssigned, view.Changes[0].Cause)
	assert.Equal(t, updated.Revision, view.ChangeCursor)
	assert.Equal(t, updated.Revision, view.Revision)
	assert.False(t, view.ChangesPending)
}

func TestTeamAttemptViewRequiresDurableStartTime(t *testing.T) {
	t.Parallel()

	value := TeamAttemptView{
		Target: TeamWorkerTarget{
			TeamID: "team-one", MemberID: "worker", TaskID: "task-one",
			AttemptID: "attempt-one",
		},
		Number: 1, StartedAt: time.Now().UTC(),
	}
	assert.True(t, validTeamAttemptView(value, "team-one"))

	value.StartedAt = time.Time{}
	assert.False(t, validTeamAttemptView(value, "team-one"))
}

func TestReadTeamControlProjectionOmitsPrivateContentAndExecutionIdentity(t *testing.T) {
	t.Parallel()

	runtime, _, aggregate := newTeamViewRuntime(t)
	control := runtime.team.control
	at := time.Date(2026, time.July, 29, 1, 2, 3, 0, time.UTC)

	_, err := control.Submit(t.Context(), teamcontrol.Command{
		ID:        "control-one",
		Action:    teamcontrol.ActionMessage,
		Target:    teamcontrol.Target{TeamID: aggregate.ID, MemberID: "worker"},
		Text:      "first-private-message",
		CreatedAt: at,
	}, 0)
	require.NoError(t, err)
	_, err = control.Submit(t.Context(), teamcontrol.Command{
		ID:        "control-two",
		Action:    teamcontrol.ActionMessage,
		Target:    teamcontrol.Target{TeamID: aggregate.ID, MemberID: "worker"},
		Text:      "second-private-message",
		CreatedAt: at.Add(time.Second),
	}, 1)
	require.NoError(t, err)
	_, err = control.Begin(
		t.Context(), aggregate.ID, "control-two",
		teamcontrol.Mutation{ID: "begin-control-two", ExpectedRevision: 2},
		teamcontrol.ResolvedTarget{
			TeamID: aggregate.ID, MemberID: "worker", TaskID: "task-one",
			AttemptID: "attempt-private", ContinuationID: "continuation-private",
			SessionID: "session-private", WorkspaceID: "workspace-private", OwnerGeneration: 7,
		},
	)
	require.NoError(t, err)

	view, err := runtime.ReadTeam(t.Context(), TeamReadRequest{
		TeamID: aggregate.ID, AfterRevision: aggregate.Revision, AfterControlRevision: 2,
	})
	require.NoError(t, err)
	require.Len(t, view.Controls, 1)
	assert.Equal(t, TeamControlApplying, view.Controls[0].State)
	assert.Equal(t, uint64(3), view.ControlCursor)
	assert.Equal(t, uint64(3), view.ControlRevision)

	encoded, err := json.Marshal(view)
	require.NoError(t, err)

	for _, private := range []string{
		"first-private-message", "second-private-message", "continuation-private",
		"session-private", "workspace-private",
	} {
		assert.NotContains(t, string(encoded), private)
	}
}

func TestReadTeamRejectsUnavailableAndFutureCursors(t *testing.T) {
	t.Parallel()

	runtime, _, aggregate := newTeamViewRuntime(t)
	_, err := runtime.ReadTeam(t.Context(), TeamReadRequest{TeamID: "team-other"})
	require.ErrorIs(t, err, ErrTeamReadUnavailable)

	_, err = runtime.ReadTeam(t.Context(), TeamReadRequest{
		TeamID: aggregate.ID, AfterRevision: aggregate.Revision + 1,
	})
	require.ErrorIs(t, err, ErrTeamReadStale)

	_, err = runtime.ReadTeam(t.Context(), TeamReadRequest{
		TeamID: aggregate.ID, AfterControlRevision: 1,
	})
	require.ErrorIs(t, err, ErrTeamReadStale)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = runtime.ReadTeam(ctx, TeamReadRequest{TeamID: aggregate.ID})
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadTeamNeverLoadsFullTeamHistory(t *testing.T) {
	t.Parallel()

	memory, err := team.NewMemoryStore()
	require.NoError(t, err)

	store := &historyRejectingTeamStore{Store: memory}
	runtime, _, aggregate := newTeamViewRuntimeWithStore(t, store)

	_, err = runtime.ReadTeam(t.Context(), TeamReadRequest{TeamID: aggregate.ID})
	require.NoError(t, err)
	assert.False(t, store.called.Load())
}

func TestReadTeamControlChangesAreBounded(t *testing.T) {
	t.Parallel()

	runtime, _, aggregate := newTeamViewRuntime(t)

	at := time.Date(2026, time.July, 29, 1, 2, 3, 0, time.UTC)
	for index := range 130 {
		_, err := runtime.team.control.Submit(t.Context(), teamcontrol.Command{
			ID:        team.CommandID(fmt.Sprintf("bounded-control-%d", index)),
			Action:    teamcontrol.ActionCancelTeam,
			Target:    teamcontrol.Target{TeamID: aggregate.ID},
			CreatedAt: at.Add(time.Duration(index) * time.Second),
		}, teamcontrol.Revision(index))
		require.NoError(t, err)
	}

	view, err := runtime.ReadTeam(t.Context(), TeamReadRequest{
		TeamID: aggregate.ID, AfterRevision: aggregate.Revision, AfterControlRevision: 1,
	})
	require.NoError(t, err)
	assert.Len(t, view.Controls, teamReadPageLimit)
	assert.Equal(t, uint64(130), view.ControlRevision)
	assert.Equal(t, uint64(129), view.ControlCursor)
	assert.True(t, view.ControlsPending)
}

func newTeamViewRuntime(t *testing.T) (*Runtime, *team.Engine, team.Team) {
	t.Helper()

	domainStore, err := team.NewMemoryStore()
	require.NoError(t, err)

	return newTeamViewRuntimeWithStore(t, domainStore)
}

func newTeamViewRuntimeWithStore(
	t *testing.T,
	domainStore team.Store,
) (*Runtime, *team.Engine, team.Team) {
	t.Helper()

	engine, err := team.New(domainStore)
	require.NoError(t, err)

	aggregate, err := engine.Create(t.Context(), team.CreateRequest{
		Command:   teamViewCommand("create-team", 0),
		ID:        "team-one",
		Objective: "deliver the bounded Team view",
		Lead:      team.MemberSpec{ID: "lead", Name: "Lead", Role: "lead"},
	})
	require.NoError(t, err)
	aggregate, err = engine.RegisterMember(t.Context(), aggregate.ID, team.RegisterMemberRequest{
		Command: teamViewCommand("register-worker", aggregate.Revision),
		Member:  team.MemberSpec{ID: "worker", Name: "Worker", Role: "implementation"},
	})
	require.NoError(t, err)
	aggregate, err = engine.CreateTask(t.Context(), aggregate.ID, team.CreateTaskRequest{
		Command:      teamViewCommand("create-task", aggregate.Revision),
		TaskID:       "task-one",
		Title:        "Implement read model",
		Description:  "Expose safe bounded Team state",
		AttemptLimit: 2,
	})
	require.NoError(t, err)

	resourceStore, err := teamstate.New(
		filepath.Join(t.TempDir(), "team-state"),
		teamstate.Limits{},
	)
	require.NoError(t, err)

	at := time.Date(2026, time.July, 29, 1, 2, 3, 0, time.UTC)
	resources := teamstate.Snapshot{
		TeamID: aggregate.ID, Revision: 1, State: teamstate.StateActive,
		Parent: teamstate.ParentResource{
			SessionID: "session-parent", WorkspaceID: "workspace-parent",
			Workspace: teamstate.FileIdentity{Path: "/workspace", Device: 1, Inode: 2},
		},
		Repository: teamstate.RepositoryResource{
			CommonDir: teamstate.FileIdentity{Path: "/repository/.git", Device: 1, Inode: 3},
			BaseOID:   strings.Repeat("a", 40), BranchRef: "refs/heads/main",
			Admission: teamstate.AdmissionClean,
		},
		Members: []teamstate.MemberResource{
			{MemberID: "lead", CapabilityProfileFingerprint: strings.Repeat("b", 64)},
			{MemberID: "worker", CapabilityProfileFingerprint: strings.Repeat("c", 64)},
		},
		Cleanup: teamstate.CleanupRetain, CreatedAt: at, UpdatedAt: at,
	}
	_, err = resourceStore.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create-resources", ExpectedRevision: 0, Snapshot: resources,
	})
	require.NoError(t, err)

	controlStore, err := teamcontrol.New(
		filepath.Join(t.TempDir(), "team-control"),
		teamcontrol.Limits{},
	)
	require.NoError(t, err)

	runtime := &Runtime{
		state: State{},
		team: &teamCoordinator{
			id: aggregate.ID, leadID: aggregate.LeadMemberID, engine: engine,
			state: resourceStore, control: controlStore,
		},
	}

	return runtime, engine, aggregate
}

type historyRejectingTeamStore struct {
	team.Store
	called atomic.Bool
}

func (s *historyRejectingTeamStore) History(
	context.Context,
	team.ID,
) ([]team.Record, error) {
	s.called.Store(true)

	return nil, errors.New("full Team History must not be read")
}

func teamViewCommand(id team.CommandID, revision team.Revision) team.CommandMetadata {
	return team.CommandMetadata{
		ID: id, ExpectedRevision: revision,
		Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "coordinator"},
	}
}
