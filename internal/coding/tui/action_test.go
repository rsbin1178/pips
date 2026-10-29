package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultActionsAreUniqueAndDriveHints(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateActions(defaultActions))
	action, ok := resolveAction(defaultActions, contextRunning, "tab")
	require.True(t, ok)
	assert.Equal(t, actionFollowUp, action)
	assert.Contains(t, actionHints(defaultActions, contextRunning), "tab queue request")
	// The panel reads the same table, so a binding and its hint cannot disagree.
	page := helpPageText(fullscreenModel(t, stubController{state: readyState()}, true), helpTabShortcuts)
	assert.Contains(t, page, "ctrl+j / shift+enter: newline")
	assert.Contains(t, page, "ctrl+c / esc: cancel")

	_, ok = resolveAction(defaultActions, contextIdle, "tab")
	assert.False(t, ok)
}

// Ctrl+X is the shortcuts list, the conventional key for it. It has
// to reach the help block, and it must no longer be the copy binding.
func TestCtrlXShowsTheShortcutList(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)

	_, _ = model.Update(key("ctrl+x"))

	require.Equal(t, routeHelp, model.route.kind)

	listing := ansi.Strip(model.View().Content)
	assert.Contains(t, listing, "Shortcuts")
	assert.Contains(t, listing, "ctrl+a")
	assert.Contains(t, listing, "select all")
	assert.Contains(t, listing, "ctrl+y")
	assert.Contains(t, listing, "copy reply")
	assert.Contains(t, listing, "ctrl+x / ctrl+?")
}

func TestActionValidationRejectsContextKeyConflict(t *testing.T) {
	t.Parallel()

	err := validateActions([]actionBinding{
		{ID: actionSubmit, Contexts: []actionContext{contextIdle}, Keys: []string{"enter"}, Label: "send"},
		{ID: actionCommand, Contexts: []actionContext{contextIdle}, Keys: []string{"enter"}, Label: "commands"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts")
}
