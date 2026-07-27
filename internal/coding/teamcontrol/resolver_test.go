package teamcontrol_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

func TestResolveCommandBindsExactlyOneRunningAttempt(t *testing.T) {
	t.Parallel()

	group, resources := runningTeamFixture()
	command := newCommand("operator-resolve", group.ID, "worker-1")
	resolved, err := teamcontrol.ResolveCommand(group, resources, command)
	require.NoError(t, err)
	assert.Equal(t, team.AttemptID("attempt-1"), resolved.AttemptID)
	assert.Equal(t, continuation.ID("continuation-1"), resolved.ContinuationID)
	assert.Equal(t, "session-1", resolved.SessionID)
	assert.Equal(t, "workspace-1", resolved.WorkspaceID)

	resources.Attempts = append(resources.Attempts, teamstate.AttemptResource{
		TaskID: "task-2", AttemptID: "attempt-2", MemberID: "worker-1",
		ContinuationID: "continuation-2",
		Session:        teamstate.WorkerSessionResource{SessionID: "session-2", WorkspaceID: "workspace-2"},
		State:          teamstate.AttemptRunning,
	})
	group.Tasks = append(group.Tasks, team.Task{
		ID: "task-2", Status: team.TaskStatusRunning,
		Attempts: []team.Attempt{{
			ID: "attempt-2", MemberID: "worker-1", ContinuationID: "continuation-2",
			Status: team.AttemptStatusRunning,
		}},
	})
	_, err = teamcontrol.ResolveCommand(group, resources, command)
	require.ErrorIs(t, err, teamcontrol.ErrAmbiguous)
}

func TestResolveCommandFailsClosedOnStaleIdentity(t *testing.T) {
	t.Parallel()

	group, resources := runningTeamFixture()
	command := newCommand("operator-stale", group.ID, "worker-1")
	command.Target.ExpectedAttemptID = "attempt-old"
	_, err := teamcontrol.ResolveCommand(group, resources, command)
	require.ErrorIs(t, err, teamcontrol.ErrAmbiguous)

	command.Target.ExpectedAttemptID = "attempt-1"
	resources.Attempts[0].Session.SessionID = ""
	_, err = teamcontrol.ResolveCommand(group, resources, command)
	assert.ErrorIs(t, err, teamcontrol.ErrInvalid)
}

func TestResolveCommandDomainActionsDoNotRequireLiveSession(t *testing.T) {
	t.Parallel()

	group, resources := runningTeamFixture()
	group.Tasks[0].Status = team.TaskStatusFailed
	group.Tasks[0].Attempts[0].Status = team.AttemptStatusFailed
	resources.Attempts[0].State = teamstate.AttemptFailed
	command := teamcontrol.Command{
		ID: "operator-retry", Action: teamcontrol.ActionRetryTask,
		Target:    teamcontrol.Target{TeamID: group.ID, TaskID: "task-1"},
		CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC),
	}
	resolved, err := teamcontrol.ResolveCommand(group, resources, command)
	require.NoError(t, err)
	assert.Equal(t, team.TaskID("task-1"), resolved.TaskID)
	assert.Empty(t, resolved.SessionID)
}

func TestClassifyInterrupted(t *testing.T) {
	t.Parallel()

	assert.Equal(t, teamcontrol.DispositionDeliveryUnknown, teamcontrol.ClassifyInterrupted(
		teamcontrol.Entry{Command: teamcontrol.Command{Action: teamcontrol.ActionFollowUp}, State: teamcontrol.StateApplying},
	))
	assert.Equal(t, teamcontrol.DispositionReconcile, teamcontrol.ClassifyInterrupted(
		teamcontrol.Entry{Command: teamcontrol.Command{Action: teamcontrol.ActionCancelTask}, State: teamcontrol.StateApplying},
	))
	assert.Equal(t, teamcontrol.DispositionNone, teamcontrol.ClassifyInterrupted(
		teamcontrol.Entry{Command: teamcontrol.Command{Action: teamcontrol.ActionMessage}, State: teamcontrol.StatePending},
	))
}

func runningTeamFixture() (team.Team, teamstate.Snapshot) {
	group := team.Team{
		ID: "team-1", Status: team.StatusActive,
		Members: []team.Member{{ID: "worker-1", Status: team.MemberStatusActive}},
		Tasks: []team.Task{{
			ID: "task-1", Status: team.TaskStatusRunning,
			Attempts: []team.Attempt{{
				ID: "attempt-1", MemberID: "worker-1", ContinuationID: "continuation-1",
				Status: team.AttemptStatusRunning,
			}},
		}},
	}
	resources := teamstate.Snapshot{
		TeamID: group.ID, State: teamstate.StateActive,
		Members: []teamstate.MemberResource{{MemberID: "worker-1"}},
		Attempts: []teamstate.AttemptResource{{
			TaskID: "task-1", AttemptID: "attempt-1", MemberID: "worker-1",
			ContinuationID: "continuation-1",
			Session:        teamstate.WorkerSessionResource{SessionID: "session-1", WorkspaceID: "workspace-1"},
			State:          teamstate.AttemptRunning,
		}},
	}

	return group, resources
}
