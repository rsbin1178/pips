package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultActionsAreUniqueAndDriveHints(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateActions(defaultActions))
	action, ok := resolveAction(defaultActions, contextRunning, "tab")
	require.True(t, ok)
	assert.Equal(t, actionFollowUp, action)
	assert.Contains(t, actionHints(defaultActions, contextRunning), "tab follow up")
	help := renderActionHelp(defaultActions, contextRunning)
	assert.Contains(t, help, "ctrl+j / shift+enter — newline")
	assert.Contains(t, help, "ctrl+c / esc — cancel")

	_, ok = resolveAction(defaultActions, contextIdle, "tab")
	assert.False(t, ok)
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
