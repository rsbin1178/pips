package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/teamcontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamControlLifecycleCodecAndTelemetryOmitPrivateControlContent(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.July, 29, 1, 2, 3, 0, time.UTC)
	record := teamcontrol.Record{
		Revision: 2,
		Entry: teamcontrol.Entry{
			Command: teamcontrol.Command{
				ID: "control-secret", Action: teamcontrol.ActionResolveQuestion,
				Target: teamcontrol.Target{
					TeamID: "team-secret", MemberID: "member-secret", TaskID: "task-secret",
					ExpectedAttemptID: "attempt-secret", OwnerGeneration: 9,
				},
				Payload: ai.JSON(`{"private":"answer-secret"}`), CreatedAt: at,
			},
			State: teamcontrol.StateApplying,
			Resolved: &teamcontrol.ResolvedTarget{
				TeamID: "team-secret", MemberID: "member-secret", TaskID: "task-secret",
				AttemptID: "attempt-secret", ContinuationID: "continuation-secret",
				SessionID: "session-secret", WorkspaceID: "workspace-secret", OwnerGeneration: 9,
			},
			UpdatedAt: at,
		},
	}
	payload := teamControlLifecycle(record)
	event := newSessionEvent(EventTeamControlLifecycle, payload)
	require.NoError(t, ValidateEvent(event))

	encoded, err := MarshalEvent(event)
	require.NoError(t, err)
	decoded, err := UnmarshalEvent(encoded)
	require.NoError(t, err)
	assert.Equal(t, payload, decoded.Payload)

	safe, err := Project(event, DisclosureSafe)
	require.NoError(t, err)
	encodedSafe, err := json.Marshal(safe)
	require.NoError(t, err)

	for _, private := range []string{
		"answer-secret", "continuation-secret", "session-secret", "workspace-secret",
	} {
		assert.NotContains(t, string(encoded), private)
		assert.NotContains(t, string(encodedSafe), private)
	}

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encodedTelemetry, err := json.Marshal(telemetry)
	require.NoError(t, err)

	for _, routingID := range []string{
		"team-secret", "control-secret", "member-secret", "task-secret", "attempt-secret",
	} {
		assert.NotContains(t, string(encodedTelemetry), routingID)
	}

	assert.Equal(t, "team_control", telemetry.Agent)
	assert.Equal(t, string(TeamControlResolveQuestion), telemetry.TeamControlAction)
	assert.Equal(t, string(TeamControlApplying), telemetry.TeamControlState)
}

func TestValidateTeamControlLifecycleRejectsMalformedState(t *testing.T) {
	t.Parallel()

	tests := []TeamControlLifecycle{
		{TeamID: "/private/team", Revision: 1, CommandID: "control-1", Action: TeamControlCancelTeam, State: TeamControlPending},
		{TeamID: "team-1", CommandID: "control-1", Action: TeamControlCancelTeam, State: TeamControlPending},
		{TeamID: "team-1", Revision: 1, CommandID: "control-1", Action: "unknown", State: TeamControlPending},
		{TeamID: "team-1", Revision: 1, CommandID: "control-1", Action: TeamControlCancelTeam, State: TeamControlRejected},
		{TeamID: "team-1", Revision: 1, CommandID: "control-1", Action: TeamControlCancelTeam, State: TeamControlPending, Code: "unexpected"},
		{TeamID: "team-1", Revision: 1, CommandID: "control-1", Action: TeamControlMessage, MemberID: "worker-1", OwnerGeneration: 1, State: TeamControlPending},
	}
	for _, payload := range tests {
		event := newSessionEvent(EventTeamControlLifecycle, payload)
		require.ErrorIs(t, ValidateEvent(event), ErrInvalidEvent)
	}
}

func TestReduceTeamControlLifecycleIsLatestBoundedAndStrict(t *testing.T) {
	t.Parallel()

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newSessionEvent(EventTeamControlLifecycle, TeamControlLifecycle{
			TeamID: "team-1", Revision: 1, CommandID: "control-1",
			Action: TeamControlMessage, MemberID: "worker-1", State: TeamControlPending,
		}),
		newSessionEvent(EventTeamControlLifecycle, TeamControlLifecycle{
			TeamID: "team-1", Revision: 2, CommandID: "control-1",
			Action: TeamControlMessage, MemberID: "worker-1", TaskID: "task-1",
			AttemptID: "attempt-1", OwnerGeneration: 3, State: TeamControlApplying,
		}),
		newSessionEvent(EventTeamControlLifecycle, TeamControlLifecycle{
			TeamID: "team-1", Revision: 3, CommandID: "control-1",
			Action: TeamControlMessage, MemberID: "worker-1", TaskID: "task-1",
			AttemptID: "attempt-1", OwnerGeneration: 3, State: TeamControlApplied,
		}),
	}

	var state State

	for index := range events {
		events[index].Sequence = uint64(index + 1)

		var err error

		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}

	require.Len(t, state.TeamControls, 1)
	assert.Equal(t, TeamControlApplied, state.TeamControls[0].State)
	assert.Equal(t, uint64(3), state.TeamControls[0].Revision)

	late := newSessionEvent(EventTeamControlLifecycle, TeamControlLifecycle{
		TeamID: "team-1", Revision: 4, CommandID: "control-1",
		Action: TeamControlMessage, MemberID: "worker-1", TaskID: "task-1",
		AttemptID: "attempt-1", OwnerGeneration: 3, State: TeamControlApplying,
	})
	late.Sequence = state.Sequence + 1
	_, err := Reduce(state, late)
	require.ErrorIs(t, err, ErrEventProtocol)

	for index := range maxRecentTeamControls + 1 {
		event := newSessionEvent(EventTeamControlLifecycle, TeamControlLifecycle{
			TeamID: "team-1", Revision: uint64(index + 4),
			CommandID: team.CommandID(fmt.Sprintf("bulk-control-%d", index)),
			Action:    TeamControlCancelTeam, State: TeamControlPending,
		})
		event.Sequence = state.Sequence + 1
		state, err = Reduce(state, event)
		require.NoError(t, err)
	}

	assert.Len(t, state.TeamControls, maxRecentTeamControls)
}

func TestTeamControlTransitionsPublishOnlyAfterDurableRecords(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "controls"), teamcontrol.Limits{})
	require.NoError(t, err)

	var observed []TeamControlLifecycle

	coordinator := &teamCoordinator{
		id: "team-1", control: store, wake: make(chan struct{}, 1),
	}
	coordinator.controlLifecycle = func(_ context.Context, value TeamControlLifecycle) {
		revision, revisionErr := store.Revision(t.Context(), coordinator.id)
		require.NoError(t, revisionErr)
		assert.Equal(t, teamcontrol.Revision(value.Revision), revision)
		observed = append(observed, value)
	}

	reference, err := coordinator.submitControlCommand(
		t.Context(), teamcontrol.ActionMessage,
		teamcontrol.Target{TeamID: coordinator.id, MemberID: "worker-1"},
		"private operator message", nil,
	)
	require.NoError(t, err)
	require.Len(t, observed, 1)
	assert.Equal(t, reference.CommandID, observed[0].CommandID)
	assert.Equal(t, TeamControlPending, observed[0].State)
	assert.Equal(t, uint64(1), observed[0].Revision)

	resolved := teamcontrol.ResolvedTarget{
		TeamID: coordinator.id, MemberID: "worker-1", TaskID: "task-1",
		AttemptID: "attempt-1", ContinuationID: "continuation-1",
		SessionID: "session-1", WorkspaceID: "workspace-1", OwnerGeneration: 2,
	}
	require.NoError(t, coordinator.beginControl(t.Context(), reference.CommandID, resolved))
	require.Len(t, observed, 2)
	assert.Equal(t, TeamControlApplying, observed[1].State)
	assert.Equal(t, uint64(2), observed[1].Revision)

	require.NoError(t, coordinator.completeControl(
		t.Context(), reference.CommandID, "applied", teamcontrol.StateApplied, "",
	))
	require.Len(t, observed, 3)
	assert.Equal(t, TeamControlApplied, observed[2].State)
	assert.Equal(t, uint64(3), observed[2].Revision)
}
