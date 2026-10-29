package tui

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueSubscriptionRestartReleasesObservedCompletionFence(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 20
	controller := &interactionController{state: state}
	model := fullscreenModel(t, controller, true)
	model.subscriptionMode = true
	model.observedSequence = 19
	model.requests.closedSequence = 20
	model.requests.waiting = []pendingRequest{{id: 1, message: ai.UserText("next")}}
	model.queued = 1
	_, command := model.Update(model.requestAdvanceCommand()())
	require.Nil(t, command)

	_, command = model.Update(subscriptionStartedMsg{
		generation: model.subscriptionSeq, supported: true,
		observation: coding.EventObservation{State: state}, bridge: &subscriptionBridge{},
	})
	advance, ok := lastReviewCommandMessage(t, command).(requestAdvanceMsg)
	require.True(t, ok, "the recovered snapshot must wake the waiting queue")

	_, command = model.Update(advance)
	require.NotNil(t, command)
	require.NotNil(t, model.requests.active)
	assert.Equal(t, "next", visibleMessageText(model.requests.active.message))
}

func TestQueueCancellationCompletesBeforeExplicitResume(t *testing.T) {
	t.Parallel()

	controller := &cancelController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	bridge := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) {}
	})
	require.Eventually(t, func() bool {
		select {
		case <-bridge.stopped():
			return true
		default:
			return false
		}
	}, 30*time.Second, 10*time.Millisecond)

	model.bridge = bridge
	model.requests.active = &pendingRequest{id: 1, message: ai.UserText("old")}
	model.requests.interactionID = "old"
	model.requests.waiting = []pendingRequest{{id: 2, message: ai.UserText("next")}}
	model.queued = 2
	cancel := model.cancelStream()
	controller.state.Interaction = coding.InteractionState{ID: "old", Outcome: coding.InteractionCanceled}

	model.finishStream()

	model.executeQueueCommand("resume")
	assert.Nil(t, model.requestAdvanceCommand(), "an outstanding Cancel must not target a new operation")
	model.composer.SetValue("draft during cancellation")
	model.Update(key("enter"))
	assert.Equal(t, 2, model.queued, "new input is queued while cancellation finishes")
	assert.Empty(t, model.composer.Value())

	result := cancel()
	model.Update(result)
	require.False(t, model.canceling)
	advance := model.requestAdvanceCommand()
	require.NotNil(t, advance)
	model.Update(advance())
	require.True(t, model.starting)
	require.NotNil(t, model.requests.active)
	active := model.requests.active
	model.Update(result)
	assert.Same(t, active, model.requests.active, "a late cancellation result cannot requeue a new active item")
	assert.True(t, model.starting)
	assert.Equal(t, 2, model.queued)
}

func TestQueueSuccessfulOperationIgnoresUnrelatedComposerError(t *testing.T) {
	t.Parallel()

	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	model.requests.active = &pendingRequest{id: 1, message: ai.UserText("running")}
	model.requests.interactionID = "running"
	model.requests.waiting = []pendingRequest{{id: 2, message: ai.UserText("waiting")}}
	model.queued = 2
	model.streamErr = errors.New("file could not be resolved")
	controller.state.Interaction = coding.InteractionState{ID: "running", Outcome: coding.InteractionSucceeded}

	model.finishRequestOperation()
	assert.False(t, model.requests.paused)
	assert.Equal(t, 1, model.queued)
	assert.NotNil(t, model.requestAdvanceCommand())
}

func TestQueueCancellationTimeoutRetainsProducerOwnership(t *testing.T) {
	t.Parallel()

	controller := &interactionController{state: readyState()}
	model := fullscreenModel(t, controller, true)
	bridge := &eventBridge{done: make(chan struct{})}
	model.bridge = bridge
	model.canceling = true
	model.requests.active = &pendingRequest{id: 1, message: ai.UserText("executing")}
	model.requests.interactionID = "executing"
	model.requests.paused = true
	model.queued = 1
	controller.state.Interaction = coding.InteractionState{ID: "executing", Outcome: coding.InteractionCanceled}

	model.Update(cancelResultMsg{bridge: bridge, err: context.DeadlineExceeded})
	assert.Same(t, bridge, model.bridge, "terminal state cannot release a producer still cleaning up")
	assert.True(t, model.waiting, "a timed-out startup cancellation must await eventual producer closure")
	assert.NotNil(t, model.requests.active)
	assert.Equal(t, 1, model.queued)
	assert.Nil(t, model.requestAdvanceCommand())

	close(bridge.done)
	model.finishStream()
	assert.Nil(t, model.bridge)
	assert.Nil(t, model.requests.active)
	assert.Zero(t, model.queued)
}

// Inspect only the final continuation: subscription waits stay blocked until
// events arrive, so executing unrelated branches would obscure the wakeup.
func lastReviewCommandMessage(t *testing.T, command tea.Cmd) tea.Msg {
	t.Helper()

	require.NotNil(t, command)

	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		require.NotEmpty(t, batch)

		return lastReviewCommandMessage(t, batch[len(batch)-1])
	}

	value := reflect.ValueOf(message)
	if value.IsValid() && value.Type().PkgPath() == "charm.land/bubbletea/v2" && value.Type().Name() == "sequenceMsg" {
		require.Positive(t, value.Len())
		last, ok := reflect.TypeAssert[tea.Cmd](value.Index(value.Len() - 1))
		require.True(t, ok)

		return lastReviewCommandMessage(t, last)
	}

	return message
}
