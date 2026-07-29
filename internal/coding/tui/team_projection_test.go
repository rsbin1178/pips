//nolint:wsl_v5 // Projection cursor transitions stay adjacent to exact-once assertions.
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamAttemptsRemainMutableThenCommitOnceInTerminalOrder(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.width = 200
	model.storeTeamProjectionView(teamProjectionTestView())
	first := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	second := teamProjectionAttempt("worker-2", "task-2", "attempt-2")
	model.state.Teams = []coding.TeamLifecycleState{
		{TeamLifecycle: first},
		{TeamLifecycle: second},
	}

	active := model.activeTeamAttemptBlocks()
	require.Len(t, active, 2)
	assert.Contains(t, renderTimelineBlock(active[0], model.markdown, 120, themeDark, true), "Builder")

	model.state.Teams[0].Activity = coding.TeamActivityAwaitingApproval
	active = model.activeTeamAttemptBlocks()
	require.Len(t, active, 2, "repeated progress must replace the existing identity")
	assert.Contains(t, active[0].body, "Awaiting approval")

	first.State = coding.TeamLifecycleCompleted
	first.Activity = ""
	first.DurationMillis = 2_000
	first.Turns = 2
	first.ToolCalls = 3
	first.Usage = coding.TokenUsage{InputTokens: 900, OutputTokens: 600}
	second.State = coding.TeamLifecycleFailed
	second.Activity = ""
	second.DurationMillis = 3_000
	model.state.Teams = []coding.TeamLifecycleState{
		{TeamLifecycle: first},
		{TeamLifecycle: second},
	}
	model.trackTeamLifecycleEvent(coding.Event{Payload: second})
	model.trackTeamLifecycleEvent(coding.Event{Payload: first})

	committed := model.renderTimelineBlocks(model.takeStableTeamAttemptBlocks())
	require.NotEmpty(t, committed)
	assert.Less(t, strings.Index(committed, "Reviewer"), strings.Index(committed, "Builder"))
	assert.Contains(t, committed, "2 turns · 3 tools · 1.5k tokens")
	assert.Empty(t, model.takeStableTeamAttemptBlocks())
	assert.Empty(t, model.activeTeamAttemptBlocks())
	assert.Len(t, model.scrollback.teamAttempts, 2)
}

func TestTeamAttemptCursorFreezesForVisibleAndPendingRoutes(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	value := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	value.State = coding.TeamLifecycleCompleted
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: value}}
	model.trackTeamLifecycleEvent(coding.Event{Payload: value})

	model.route = routeState{
		kind: routeTeam,
		team: &teamRouteState{stage: teamRouteActive, teamID: value.TeamID},
	}
	assert.Nil(t, model.commitStableTimeline())
	assert.Empty(t, model.scrollback.teamAttempts)

	model.route = routeState{}
	model.presentation.pendingRoute = routeOpenRequest{kind: routeAgents}
	assert.Nil(t, model.commitStableTimeline())
	assert.Empty(t, model.scrollback.teamAttempts)

	model.presentation.pendingRoute = routeOpenRequest{}
	printed := commandOutput(model.commitStableTimeline())
	assert.Contains(t, printed, "Team Worker")
	assert.Len(t, model.scrollback.teamAttempts, 1)
	assert.Empty(t, commandOutput(model.commitStableTimeline()))
}

func TestTeamAttemptProjectionPreservesStreamingAdjacency(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{ai.UserText("start the Team")}
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: "parent answer"}}
	value := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	value.State = coding.TeamLifecycleCompleted
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: value}}
	model.trackTeamLifecycleEvent(coding.Event{Payload: value})

	first := model.takeStableTimeline()
	assert.Contains(t, first, "start the Team")
	assert.NotContains(t, first, "Team Worker")
	managed := model.renderTimelineBlocks(model.activeTimelineBlocks())
	assert.Contains(t, managed, "parent answer")
	assert.Contains(t, managed, "Team Worker")

	model.state.Transcript = append(model.state.Transcript, ai.AssistantText("parent answer"))
	model.state.Draft = nil
	second := model.takeStableTimeline()
	answer := strings.Index(second, "parent answer")
	teamBlock := strings.Index(second, "Team Worker")
	require.GreaterOrEqual(t, answer, 0)
	require.GreaterOrEqual(t, teamBlock, 0)
	assert.Less(t, answer, teamBlock)
	assert.Empty(t, model.takeStableTimeline())
}

func TestTeamAttemptProjectionUsesOnlySafeCachedLabels(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	value := teamProjectionAttempt("member-secret", "task-secret", "attempt-secret")
	value.ChildSessionID = "child-session-secret"
	value.Code = "/private/worktree/internal-ref"
	block := model.projectTeamAttemptBlock(value)
	fallback := renderTimelineBlock(block, model.markdown, 200, themeDark, true)
	assert.Contains(t, fallback, "Team Worker · Team task")
	for _, private := range []string{
		"team-1", "member-secret", "task-secret", "attempt-secret",
		"child-session-secret", "/private/worktree", "internal-ref",
	} {
		assert.NotContains(t, fallback, private)
	}

	model.storeTeamProjectionView(coding.TeamView{
		TeamID:  "team-1",
		Members: []coding.TeamMemberView{{ID: "member-secret", Name: "Safe\nBuilder"}},
		Tasks:   []coding.TeamTaskView{{ID: "task-secret", Title: "Implement\tUI"}},
	})
	cached := renderTimelineBlock(
		model.projectTeamAttemptBlock(value), model.markdown, 200, themeDark, true,
	)
	assert.Contains(t, cached, "Safe Builder · Implement UI")
	assert.NotContains(t, cached, "member-secret")
	assert.NotContains(t, cached, "child-session-secret")
}

func TestTeamProjectionCacheRefreshesOffRouteAndRejectsLateSessionResult(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	event := coding.Event{
		SessionID: model.state.SessionID,
		Payload:   teamProjectionAttempt("worker-1", "task-1", "attempt-1"),
	}

	command := model.invalidateTeamProjection(event)
	require.NotNil(t, command)
	message, ok := command().(teamProjectionRefreshMsg)
	require.True(t, ok)
	_, next := model.Update(message)
	assert.NotNil(t, next)
	view, cached := model.teamProjection.views[team.ID("team-1")]
	require.True(t, cached)
	assert.Equal(t, "Builder", view.Members[1].Name)
	assert.Equal(t, routeNone, model.route.kind)

	command = model.invalidateTeamProjection(event)
	require.NotNil(t, command)
	late, ok := command().(teamProjectionRefreshMsg)
	require.True(t, ok)
	controller.mu.Lock()
	reads := append([]coding.TeamReadRequest(nil), controller.reads...)
	controller.mu.Unlock()
	require.GreaterOrEqual(t, len(reads), 2)
	assert.Equal(t, team.Revision(5), reads[len(reads)-1].AfterRevision)
	terminal := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	terminal.State = coding.TeamLifecycleCompleted
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: terminal}}
	model.trackTeamLifecycleEvent(coding.Event{Payload: terminal})
	require.Len(t, model.takeStableTeamAttemptBlocks(), 1)
	require.Len(t, model.scrollback.teamAttempts, 1)
	model.state.SessionID = "session-2"
	model.resetTeamProjection(model.state.SessionID)
	model.Update(late)
	assert.Empty(t, model.teamProjection.views)
	assert.Empty(t, model.scrollback.teamAttempts)
}

func TestTeamStatusAppearsOnlyWhileWorkOrIntegrationIsPending(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Mode = coding.ModePlan
	baseline := model.statusLine()
	terminal := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	terminal.State = coding.TeamLifecycleCompleted
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: terminal}}
	model.state.TeamIntegrations = []coding.TeamIntegrationLifecycleState{{
		TeamIntegrationLifecycle: coding.TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: "integration-1",
			State: coding.TeamIntegrationApplied,
		},
	}}
	assert.Equal(t, baseline, model.statusLine(), "terminal Team state must be byte-identical")

	running := teamProjectionAttempt("worker-1", "task-1", "attempt-1")
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: running}}
	model.state.TeamIntegrations = nil
	model.width = 50
	status := ansi.Strip(model.statusLine())
	assert.True(t, strings.HasSuffix(status, "Plan mode · Team"))

	model.state.Mode = coding.ModeAgent
	model.width = 100
	assert.True(t, strings.HasSuffix(ansi.Strip(model.statusLine()), "Team · Running"))
	model.width = 16
	assert.True(t, strings.HasSuffix(ansi.Strip(model.statusLine()), "team"))

	model.width = 100
	model.state.TeamIntegrations = []coding.TeamIntegrationLifecycleState{{
		TeamIntegrationLifecycle: coding.TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: "integration-2",
			State: coding.TeamIntegrationApprovalRequired,
		},
	}}
	assert.True(t, strings.HasSuffix(
		ansi.Strip(model.statusLine()), "Integration · Approval required",
	))
}

func teamProjectionAttempt(
	memberID team.MemberID,
	taskID team.TaskID,
	attemptID team.AttemptID,
) coding.TeamLifecycle {
	return coding.TeamLifecycle{
		TeamID: "team-1", MemberID: memberID, TaskID: taskID, AttemptID: attemptID,
		ChildSessionID: "child-1", State: coding.TeamLifecycleRunning,
		Activity: coding.TeamActivityWorking,
	}
}

func teamProjectionTestView() coding.TeamView {
	return coding.TeamView{
		TeamID: "team-1",
		Members: []coding.TeamMemberView{
			{ID: "worker-1", Name: "Builder"},
			{ID: "worker-2", Name: "Reviewer"},
		},
		Tasks: []coding.TeamTaskView{
			{ID: "task-1", Title: "Implement timeline"},
			{ID: "task-2", Title: "Review timeline"},
		},
	}
}
