package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewStartupDoesNotAcceptHiddenCommandsBeforeSize(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{NoColor: true})
	_, command := model.Update(bootstrapResult{controller: stubController{state: readyState()}})
	require.Nil(t, command)

	for _, input := range []tea.Msg{key("/"), tea.PasteMsg{Content: "/help"}, key("enter")} {
		_, command = model.Update(input)
		require.Nil(t, command, "input must not start hidden commands before geometry is ready")
	}

	assert.Empty(t, model.presentation.writes)
	assert.False(t, model.scrollbackOutput)
	assert.Empty(t, model.composer.Value())
	assert.False(t, model.bannerPrinted)

	_, command = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, command, "waiting for initial size must remain cancellable")
	assert.IsType(t, tea.QuitMsg{}, command())
}

func TestReviewObsoleteSubscriptionCannotReplaceResumedState(t *testing.T) {
	t.Parallel()

	controller := &reviewObserverController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	oldStart := model.startSubscription()
	oldResult := oldStart()
	controller.state.SessionID = "resumed-session"

	_, command := model.Update(controlResultMsg{operation: operationResume})
	model.Update(commandMessage(t, command))
	require.Equal(t, "resumed-session", model.state.SessionID)
	current := model.subscription
	_, command = model.Update(oldResult)
	assert.Nil(t, command, "a dispatched old continuation must be rejected too")
	assert.Equal(t, "resumed-session", model.state.SessionID)
	assert.Same(t, current, model.subscription)
}

type reviewObserverController struct {
	stubController
}

func (c *reviewObserverController) ObserveEvents() (coding.EventObservation, error) {
	return coding.EventObservation{
		State: c.state.Clone(), Subscription: &coding.EventSubscription{},
	}, nil
}
