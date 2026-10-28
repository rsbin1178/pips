//nolint:wsl_v5 // Queue lifecycle fixtures keep state changes beside their assertions.
package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueuedCountRetiresOnlyAtCompleteProducerBoundary(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.starting = true
	for _, text := range []string{"first", "second"} {
		model.composer.SetValue(text)
		model.Update(key("enter"))
	}
	model.starting = false
	advance := model.requestAdvanceCommand()
	require.NotNil(t, advance)
	message, ok := advance().(requestAdvanceMsg)
	require.True(t, ok)
	require.NotNil(t, model.advanceRequestQueue(message))
	assert.Equal(t, 2, model.queued)
	assert.Nil(t, model.advanceRequestQueue(message), "duplicate dispatch cannot consume the next head")
	model.observeRequestEvent(coding.Event{
		SessionID: model.state.SessionID, InteractionID: "notification",
		Payload: coding.InteractionStarted{Mode: coding.ModeAgent, Source: coding.InteractionSourceAgentNotification},
	})
	assert.Empty(t, model.requests.interactionID)
	model.observeRequestEvent(coding.Event{
		SessionID: model.state.SessionID, InteractionID: "queued-1",
		Payload: coding.InteractionStarted{Mode: coding.ModeAgent},
	})
	model.observeRequestEvent(coding.Event{
		SessionID: model.state.SessionID, InteractionID: "unrelated",
		Payload: coding.InteractionCompleted{Outcome: coding.InteractionSucceeded},
	})
	assert.Equal(t, 2, model.queued)
	controller.state.Interaction = coding.InteractionState{ID: "queued-1", Active: true}
	model.starting = false
	model.finishRequestOperation()
	assert.Equal(t, 2, model.queued, "approval/question pauses preserve the active request")
	assert.Nil(t, model.requestAdvanceCommand())
	model.observeRequestEvent(coding.Event{
		SessionID: model.state.SessionID, InteractionID: "queued-1",
		Payload: coding.InteractionCompleted{Outcome: coding.InteractionSucceeded},
	})
	assert.Equal(t, 2, model.queued, "terminal delivery is earlier than producer cleanup")
	controller.state.Interaction = coding.InteractionState{ID: "queued-1", Outcome: coding.InteractionSucceeded}
	model.finishRequestOperation()
	assert.Equal(t, 1, model.queued)
	model.finishRequestOperation()
	assert.Equal(t, 1, model.queued, "duplicate closure cannot retire an unsent item")
	require.NotNil(t, model.requestAdvanceCommand())
}

func TestQueueAdmissionFailureRetainsHeadAfterPreviousSuccess(t *testing.T) {
	t.Parallel()
	state := readyState()
	state.Interaction = coding.InteractionState{ID: "previous", Outcome: coding.InteractionSucceeded}
	controller := &interactionController{state: state}
	model := fullscreenModel(t, controller, true)
	model.starting = true
	model.composer.SetValue("unsent")
	model.Update(key("enter"))
	model.starting = false
	advance := model.requestAdvanceCommand()
	require.NotNil(t, advance)
	model.Update(advance())
	model.starting = false
	model.streamErr = errors.New("busy before interaction started")
	model.finishRequestOperation()
	assert.Equal(t, 1, model.queued)
	require.Len(t, model.requests.waiting, 1)
	assert.Equal(t, "unsent", visibleMessageText(model.requests.waiting[0].message))
	assert.Nil(t, model.requests.active)
	assert.True(t, model.requests.paused)
	assert.Nil(t, model.requestAdvanceCommand())
	_, command := model.executeQueueCommand("resume")
	require.NotNil(t, command)
	assert.False(t, model.requests.paused)
	assert.NotNil(t, model.requestAdvanceCommand())
}

func TestQueueCancelUsesTerminalSnapshotAndDiscardKeepsActive(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.requests.active = &pendingRequest{id: 1, message: ai.UserText("executing")}
	model.requests.interactionID = "executing"
	model.requests.waiting = []pendingRequest{{id: 2, message: ai.UserText("unsent")}}
	model.queued = 2
	_, _ = model.executeQueueCommand("discard")
	assert.Equal(t, 1, model.queued)
	require.NotNil(t, model.requests.active)
	model.requests.waiting = []pendingRequest{{id: 3, message: ai.UserText("later")}}
	model.queued++
	controller.state.Interaction = coding.InteractionState{ID: "executing", Outcome: coding.InteractionCanceled}
	model.finishRequestOperation()
	assert.Equal(t, 1, model.queued)
	assert.True(t, model.requests.paused)
	assert.Nil(t, model.requests.active)
	assert.Equal(t, "later", visibleMessageText(model.requests.waiting[0].message))
}

func TestQueueSessionResetInvalidatesPendingAndResolvedMessages(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.requests.waiting = []pendingRequest{{id: 1, message: ai.UserText("old")}}
	model.queued = 1
	advance := model.requestAdvanceCommand()
	require.NotNil(t, advance)
	model.composerResolving = true
	resolved := composerResolvedMsg{
		generation: model.composerResolveSeq, kind: submissionQueued,
		snapshot: plainComposerSnapshot("old file"), message: ai.UserText("old file"),
	}
	model.resetRequestQueue()
	model.state.SessionID = "replacement"
	model.Update(advance())
	model.Update(resolved)
	assert.Zero(t, model.queued)
	assert.Empty(t, model.requests.waiting)
	assert.Nil(t, model.requests.active)
	assert.Empty(t, controller.prompts)
}

func TestResolvedQueuedAttachmentWaitsAndRecordsHistoryOnce(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.requests.paused = true
	model.composerResolving = true
	model.composerResolveSeq = 3
	model.composer.SetValue("attached draft")
	message := composerResolvedMsg{
		generation: 3, kind: submissionQueued, snapshot: model.composer.Snapshot(),
		message: ai.UserText("exact resolved file contents"),
	}
	model.Update(message)
	model.Update(message)
	assert.Equal(t, 1, model.queued)
	require.Len(t, model.requests.waiting, 1)
	assert.Equal(t, "exact resolved file contents", visibleMessageText(model.requests.waiting[0].message))
	assert.Len(t, model.composer.history, 1)
	assert.Empty(t, model.composer.Value())
	assert.Empty(t, controller.prompts)
}

func TestQueueSubscriptionMustProjectClosedProducerBeforeAdvancing(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.subscriptionMode = true
	model.requests.closedSequence = 20
	model.observedSequence = 19
	model.requests.waiting = []pendingRequest{{id: 1, message: ai.UserText("later")}}
	model.queued = 1
	advance := model.requestAdvanceCommand()
	require.NotNil(t, advance)
	message := advance()
	_, command := model.Update(message)
	assert.Nil(t, command)
	assert.Nil(t, model.requests.active)
	model.observedSequence = 20
	_, command = model.Update(message)
	require.NotNil(t, command)
	require.NotNil(t, model.requests.active)
	assert.Equal(t, 1, model.queued)
}

func TestQueuedFileResolutionPreservesOrderWhenForegroundFinishes(t *testing.T) {
	t.Parallel()
	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "foreground", Active: true}
	controller := newWorkspaceAttachmentController(state)
	controller.resolve = func(_ context.Context, reference attachment.Reference) (attachment.Resolved, error) {
		return attachment.NewResolvedText(attachment.Text{Reference: reference, Content: "exact file payload"})
	}
	model := fullscreenModel(t, controller, true)
	model.composer.SetValue("older pending")
	model.Update(key("enter"))
	model.composer.SetValue("review @file")
	require.NoError(t, model.composer.InsertFile(7, len("review @file"),
		attachment.Reference{Path: "file.txt", Kind: attachment.KindText}))
	_, resolve := model.Update(key("enter"))
	require.NotNil(t, resolve)
	assert.True(t, model.composerResolving)
	controller.state.Phase = coding.PhaseIdle
	controller.state.Interaction = coding.InteractionState{ID: "foreground", Outcome: coding.InteractionSucceeded}
	model.finishStream()
	assert.Nil(t, model.requestAdvanceCommand())
	model.Update(resolve())
	require.Len(t, model.requests.waiting, 2)
	assert.Equal(t, "older pending", visibleMessageText(model.requests.waiting[0].message))
	resolved, ok := model.requests.waiting[1].message.(ai.UserMessage)
	require.True(t, ok)
	assert.Contains(t, resolved.Parts, ai.TextPart{Text: "\n\n[Workspace file: file.txt]\nexact file payload"})
	assert.Len(t, model.composer.history, 2)
	assert.Equal(t, 2, model.queued)
}

func TestFailedFileResolutionReleasesPreviouslyQueuedRequest(t *testing.T) {
	t.Parallel()
	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.starting = true
	model.composer.SetValue("already queued")
	model.Update(key("enter"))
	model.starting = false
	model.composer.SetValue("keep unresolved draft")
	model.composerResolving = true
	model.composerResolveSeq = 3
	_, command := model.Update(composerResolvedMsg{generation: 3, err: errors.New("file changed")})
	driveModelCommands(t, model, command)
	require.Len(t, controller.prompts, 1)
	assert.Equal(t, "already queued", visibleMessageText(controller.prompts[0]))
	assert.Equal(t, "keep unresolved draft", model.composer.Value())
}
