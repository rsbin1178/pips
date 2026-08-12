//nolint:wsl_v5 // Queue convergence assertions stay adjacent to exact control transitions.
package tui

import (
	"context"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamInteractionQueueOrdersSourcesAndDispatchesExactQuestion(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now.Add(time.Second),
		false,
	)
	controller.states[view.Attempts[1].Target] = teamQuestionState(
		t,
		view.Attempts[1].ChildSessionID,
		now,
	)
	model := readyModelWithController(t, controller, true)
	closed := false
	model.route = routeState{
		kind: routeChild, childKind: childTeamWorker,
		workerBridge: &workerSubscriptionBridge{close: func() { closed = true }},
	}
	model.storeTeamProjectionView(view)

	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	assert.True(t, closed)
	assert.Equal(t, routeNone, model.route.kind)
	require.Len(t, model.teamInteractions.entries, 2)
	assert.Equal(t, teamInteractionQuestion, model.teamInteractions.entries[0].key.kind)
	assert.Equal(t, teamInteractionApproval, model.teamInteractions.entries[1].key.kind)
	assert.Equal(t, promptQuestion, model.prompt.kind)
	content := model.View().Content
	assert.Contains(t, content, "Team Worker · Reviewer · Task: Review Team route")
	for _, private := range []string{
		"team-1", "worker-2", "task-2", "attempt-2", "worker-session-2",
	} {
		assert.NotContains(t, content, private)
	}

	model.Update(key("enter"))
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.questionResolutions, 1)
	assert.Equal(t, view.Attempts[1].Target, controller.questionResolutions[0].target)
	assert.Equal(t, []string{"React"}, controller.questionResolutions[0].resolution.Answers[0].Selections)
	assert.Empty(t, controller.controlsSnapshot())
	require.True(t, model.prompt.question.loading)
	entry := model.teamInteractions.entries[0]
	require.NotEmpty(t, entry.commandID)

	model.applyTeamInteractionControl(teamInteractionControlLifecycle(
		entry,
		coding.TeamControlApplied,
	))
	model.syncApprovalPrompt()

	require.Len(t, model.teamInteractions.entries, 1)
	assert.Equal(t, promptApproval, model.prompt.kind)
	assert.Contains(t, model.View().Content, "Team Worker · Builder · Task: Build Team route")
	assert.Contains(t, model.View().Content, "go test ./...")
}

func TestTeamInteractionQueueBreaksCommittedTimeTiesByAttemptID(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts[0].Activity = coding.TeamActivityAwaitingApproval
	view.Attempts[1].Activity = coding.TeamActivityAwaitingApproval
	view.Attempts[0], view.Attempts[1] = view.Attempts[1], view.Attempts[0]
	controller := newTeamInteractionController()
	for _, attempt := range view.Attempts {
		controller.states[attempt.Target] = teamApprovalState(
			attempt.ChildSessionID,
			now,
			false,
		)
	}
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)

	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	require.Len(t, model.teamInteractions.entries, 2)
	assert.Equal(
		t,
		team.AttemptID("attempt-1"),
		model.teamInteractions.entries[0].key.target.AttemptID,
	)
	assert.Equal(
		t,
		team.AttemptID("attempt-2"),
		model.teamInteractions.entries[1].key.target.AttemptID,
	)
}

func TestTeamInteractionUnknownApprovalRequiresExplicitReviewAfterDeliveryUnknown(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[:1]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		true,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	require.Equal(t, promptApproval, model.prompt.kind)
	assert.Equal(t, approval.ChoiceRetry, model.prompt.choices[model.prompt.cursor])
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Len(t, controller.approvalResolutions, 1)
	assert.Equal(t, view.Attempts[0].Target, controller.approvalResolutions[0].target)
	assert.Equal(t, approval.ChoiceRetry, controller.approvalResolutions[0].resolution.Choice)
	entry := model.teamInteractions.entries[0]

	control := teamInteractionControlLifecycle(
		entry,
		coding.TeamControlDeliveryUnknown,
	)
	model.state.TeamControls = append(
		model.state.TeamControls,
		coding.TeamControlLifecycleState{TeamControlLifecycle: control},
	)
	model.applyTeamInteractionControl(control)
	model.syncApprovalPrompt()

	require.Len(t, model.teamInteractions.entries, 1)
	assert.False(t, model.teamInteractions.entries[0].loading)
	assert.Contains(t, model.View().Content, "delivery is unknown")
	assert.Len(t, controller.approvalResolutions, 1)

	_, retry := model.Update(key("enter"))
	require.NotNil(t, retry)
	model.settleKnownTeamInteractionControls()
	require.Len(t, model.teamInteractions.entries, 1)
	assert.True(t, model.teamInteractions.entries[0].loading)
	assert.Empty(t, model.teamInteractions.entries[0].commandID)
	require.NoError(t, model.teamInteractions.entries[0].err)
	assert.Len(t, controller.approvalResolutions, 1)

	driveModelCommands(t, model, retry)
	require.Len(t, controller.approvalResolutions, 2)
	assert.NotEqual(t, entry.commandID, model.teamInteractions.entries[0].commandID)
}

func TestTeamInteractionOwnerChangeInvalidatesInFlightSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[:1]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		false,
	)
	model := readyModelWithController(t, controller, true)
	child := mergeTeamWorkerChildren(nil, view)[0]
	staleCommand := model.loadTeamInteraction(child)
	require.NotNil(t, staleCommand)

	current := view.Attempts[0]
	current.Target.OwnerGeneration++
	view.Attempts[0] = current
	model.reconcileTeamInteractionView(view)
	staleMessage, ok := staleCommand().(teamInteractionDataMsg)
	require.True(t, ok)
	model.applyTeamInteractionData(staleMessage)

	assert.Empty(t, model.teamInteractions.entries)
	assert.NotContains(t, model.teamInteractions.loading, child.worker.Target)
	assert.NotZero(t, model.teamInteractions.loading[current.Target])
}

func TestTeamInteractionRejectedRemainsAndStaleIsRemoved(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[:1]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		false,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)
	entry := model.teamInteractions.entries[0]
	model.applyTeamInteractionControl(teamInteractionControlLifecycle(
		entry,
		coding.TeamControlRejected,
	))
	model.syncApprovalPrompt()

	require.Len(t, model.teamInteractions.entries, 1)
	assert.Contains(t, model.View().Content, "rejected this response")

	model.applyTeamInteractionControl(teamInteractionControlLifecycle(
		entry,
		coding.TeamControlStale,
	))
	model.syncApprovalPrompt()
	assert.Empty(t, model.teamInteractions.entries)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestTeamInteractionRunningLifecycleRemovesOnlyMatchingAttempt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		false,
	)
	controller.states[view.Attempts[1].Target] = teamQuestionState(
		t,
		view.Attempts[1].ChildSessionID,
		now.Add(time.Second),
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	first := view.Attempts[0]
	model.applyTeamInteractionLifecycle(coding.Event{Payload: coding.TeamLifecycle{
		TeamID: first.Target.TeamID, MemberID: first.Target.MemberID,
		TaskID: first.Target.TaskID, AttemptID: first.Target.AttemptID,
		State: coding.TeamLifecycleRunning, Activity: coding.TeamActivityWorking,
	}})

	require.Len(t, model.teamInteractions.entries, 1)
	assert.Equal(t, view.Attempts[1].Target, model.teamInteractions.entries[0].key.target)
}

func TestTeamInteractionDuplicateSnapshotPreservesQuestionSelection(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[1:]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamQuestionState(
		t,
		view.Attempts[0].ChildSessionID,
		now,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	model.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	require.True(t, model.prompt.question.selected[0][0])
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	require.Len(t, model.teamInteractions.entries, 1)
	assert.True(t, model.prompt.question.selected[0][0])
	assert.Equal(t, promptQuestion, model.prompt.kind)
}

func TestTeamInteractionPromptSupersedesCommandPickerAndRestoresDraft(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[:1]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		false,
	)
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("keep this draft")
	model.openCommandPicker()
	model.storeTeamProjectionView(view)

	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())
	assert.Equal(t, promptApproval, model.prompt.kind)
}

func TestTeamInteractionControlResultCatchesUpEarlierAppliedEvent(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[:1]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamApprovalState(
		view.Attempts[0].ChildSessionID,
		now,
		false,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	message, ok := commandMessage(t, command).(teamInteractionControlMsg)
	require.True(t, ok)
	entry := model.teamInteractions.entries[0]
	entry.commandID = message.reference.CommandID
	entry.controlAction = message.reference.Action
	control := teamInteractionControlLifecycle(entry, coding.TeamControlApplied)
	model.state.TeamControls = append(
		model.state.TeamControls,
		coding.TeamControlLifecycleState{TeamControlLifecycle: control},
	)

	model.Update(message)

	assert.Empty(t, model.teamInteractions.entries)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestTeamInteractionQuestionCancelUsesExactRejectAndSessionResetClearsQueue(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[1:]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamQuestionState(
		t,
		view.Attempts[0].ChildSessionID,
		now,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Len(t, controller.questionRejections, 1)
	assert.Equal(t, view.Attempts[0].Target, controller.questionRejections[0].target)
	assert.Equal(t, model.prompt.question.request.ID, controller.questionRejections[0].requestID)
	assert.Empty(t, controller.controlsSnapshot())

	model.state.SessionID = "session-next"
	model.resetTeamProjection(model.state.SessionID)
	model.syncApprovalPrompt()
	assert.Empty(t, model.teamInteractions.entries)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestTeamInteractionMissingQuestionIdentityDoesNotLeaveLoadingPrompt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamInteractionTestView(now)
	view.Attempts = view.Attempts[1:]
	controller := newTeamInteractionController()
	controller.states[view.Attempts[0].Target] = teamQuestionState(
		t,
		view.Attempts[0].ChildSessionID,
		now,
	)
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	driveModelCommands(t, model, model.reconcileTeamInteractionView(view))
	model.teamInteractions.entries = nil

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	assert.Nil(t, command)
	assert.Equal(t, promptNone, model.prompt.kind)
	assert.False(t, model.prompt.question.loading)
}

type teamApprovalResolution struct {
	target     coding.TeamWorkerTarget
	resolution approval.Resolution
}

type teamQuestionResolution struct {
	target     coding.TeamWorkerTarget
	resolution question.Resolution
}

type teamQuestionRejection struct {
	target       coding.TeamWorkerTarget
	requestID    string
	schemaDigest string
}

type teamInteractionController struct {
	*teamWorkerRouteController
	states              map[coding.TeamWorkerTarget]coding.State
	approvalResolutions []teamApprovalResolution
	questionResolutions []teamQuestionResolution
	questionRejections  []teamQuestionRejection
	controlSequence     int
}

func newTeamInteractionController() *teamInteractionController {
	return &teamInteractionController{
		teamWorkerRouteController: newTeamWorkerRouteController(),
		states:                    make(map[coding.TeamWorkerTarget]coding.State),
	}
}

func (c *teamInteractionController) ObserveTeamWorker(
	_ context.Context,
	target coding.TeamWorkerTarget,
) (coding.EventObservation, error) {
	state, ok := c.states[target]
	if !ok {
		return coding.EventObservation{}, coding.ErrTeamWorkerStale
	}

	return coding.EventObservation{State: state.Clone()}, nil
}

func (c *teamInteractionController) ResolveTeamWorkerApproval(
	_ context.Context,
	target coding.TeamWorkerTarget,
	resolution approval.Resolution,
) (coding.TeamControlReference, error) {
	c.approvalResolutions = append(c.approvalResolutions, teamApprovalResolution{
		target: target, resolution: resolution,
	})

	return c.teamInteractionReference(target, coding.TeamControlResolveApproval), nil
}

func (c *teamInteractionController) ResolveTeamWorkerQuestion(
	_ context.Context,
	target coding.TeamWorkerTarget,
	resolution question.Resolution,
) (coding.TeamControlReference, error) {
	c.questionResolutions = append(c.questionResolutions, teamQuestionResolution{
		target: target, resolution: question.CloneResolution(resolution),
	})

	return c.teamInteractionReference(target, coding.TeamControlResolveQuestion), nil
}

func (c *teamInteractionController) RejectTeamWorkerQuestion(
	_ context.Context,
	target coding.TeamWorkerTarget,
	requestID string,
	schemaDigest string,
) (coding.TeamControlReference, error) {
	c.questionRejections = append(c.questionRejections, teamQuestionRejection{
		target: target, requestID: requestID, schemaDigest: schemaDigest,
	})

	return c.teamInteractionReference(target, coding.TeamControlRejectQuestion), nil
}

func (c *teamInteractionController) teamInteractionReference(
	target coding.TeamWorkerTarget,
	action coding.TeamControlAction,
) coding.TeamControlReference {
	c.controlSequence++

	return coding.TeamControlReference{
		CommandID: team.CommandID(fmt.Sprintf("interaction-control-%d", c.controlSequence)),
		TeamID:    target.TeamID, Action: action, CreatedAt: time.Now().UTC(),
	}
}

func teamInteractionTestView(now time.Time) coding.TeamView {
	view := teamWorkerTestView()
	view.Attempts[0].StartedAt = now.Add(-2 * time.Minute)
	view.Attempts[0].LifecycleState = coding.TeamLifecyclePaused
	view.Attempts[0].Activity = coding.TeamActivityAwaitingApproval
	view.Members = append(view.Members, coding.TeamMemberView{
		ID: "worker-2", Name: "Reviewer", Role: "review", Status: team.MemberStatusActive,
	})
	view.Tasks = append(view.Tasks, coding.TeamTaskView{
		ID: "task-2", Title: "Review Team route", AssignedMemberID: "worker-2",
		Status: team.TaskStatusRunning,
	})
	second := view.Attempts[0]
	second.Target.MemberID = "worker-2"
	second.Target.TaskID = "task-2"
	second.Target.AttemptID = "attempt-2"
	second.Target.OwnerGeneration = 8
	second.Number = 1
	second.StartedAt = now.Add(-time.Minute)
	second.ChildSessionID = "worker-session-2"
	second.Activity = coding.TeamActivityAwaitingQuestion
	view.Attempts = append(view.Attempts, second)

	return view
}

func teamApprovalState(sessionID string, requestedAt time.Time, unknown bool) coding.State {
	state := teamWorkerState()
	state.SessionID = sessionID
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "worker-interaction", Active: true}
	state.Approval.RequestedAt = requestedAt
	if unknown {
		state.Approval.Kind = coding.ApprovalUncertain
		state.Approval.Unknown = &coding.ApprovalUnknown{
			RequestID: "approval-unknown", CallID: "call-unknown", Tool: "shell",
			Fingerprint: "fingerprint-unknown", Attempt: 1, Pending: true, Recoverable: true,
			Choices: []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed},
		}

		return state
	}
	state.Approval.Kind = coding.ApprovalReview
	state.Approval.Required = &coding.ApprovalRequired{
		RequestID: "approval-required", CallID: "call-required", Tool: "shell",
		Command: []string{"go", "test", "./..."}, CWD: ".", Justification: "validate changes",
		Choices: []approval.Choice{
			approval.ChoiceAllowOnce, approval.ChoiceAllowSession, approval.ChoiceDeny,
		},
	}

	return state
}

func teamQuestionState(t *testing.T, sessionID string, requestedAt time.Time) coding.State {
	t.Helper()

	state := teamWorkerState()
	state.SessionID = sessionID
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{ID: "worker-interaction", Active: true}
	request := testQuestionRequest(t)
	state.Question.RequestedAt = requestedAt
	state.Question.Required = &request

	return state
}

func teamInteractionControlLifecycle(
	entry teamInteractionEntry,
	state coding.TeamControlStatus,
) coding.TeamControlLifecycle {
	code := ""
	if state == coding.TeamControlRejected || state == coding.TeamControlStale ||
		state == coding.TeamControlDeliveryUnknown {
		code = "control_failed"
	}

	return coding.TeamControlLifecycle{
		TeamID: entry.key.target.TeamID, Revision: 1,
		CommandID: entry.commandID, Action: entry.controlAction,
		MemberID: entry.key.target.MemberID, TaskID: entry.key.target.TaskID,
		AttemptID:       entry.key.target.AttemptID,
		OwnerGeneration: entry.key.target.OwnerGeneration,
		State:           state, Code: code,
	}
}
