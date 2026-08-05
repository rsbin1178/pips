package tui

import (
	"context"
	"errors"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/statusline"
	"github.com/rsbin/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusLineRendersContextAndTaskProgress(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.ContextWindow = 1_000
	model.state.ContextTokens = 256
	model.state.Tasks = tasklist.Snapshot{Completed: 2, Total: 3}
	model.width = 120

	status := ansi.Strip(model.statusLine())
	assert.Contains(t, status, "Context 25% used")
	assert.Contains(t, status, "Tasks 2/3")

	model.width = 12
	assert.LessOrEqual(t, ansi.StringWidth(model.statusLine()), model.statusLineWidth())
}

func TestBootstrapAppliesExplicitEmptyStatusLine(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{},
	}
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		StatusLine: []statusline.Item{
			statusline.Workspace,
		},
	})
	_, _ = model.Update(bootstrapResult{controller: controller})

	assert.NotNil(t, model.statusLineItems)
	assert.Empty(t, model.statusLineItems)
}

func TestStatusLinePickerSavesAndCancelHasNoSideEffects(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace, statusline.Phase},
	}
	var saved []statusline.Item
	model := newModel(t.Context(), Options{
		Workspace: "/workspace", NoColor: true,
		StatusLine: []statusline.Item{statusline.Workspace, statusline.Phase},
		SaveStatusLine: func(_ context.Context, items []statusline.Item) error {
			saved = slices.Clone(items)

			return nil
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})

	original := slices.Clone(model.statusLineItems)
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.statusLineItems)
	assert.Empty(t, saved)

	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyRight})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	message := commandMessage(t, command)
	_, _ = model.Update(message)
	assert.Equal(t, saved, model.statusLineItems)
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, []statusline.Item{statusline.Phase, statusline.Workspace}, saved)
}

func TestStatusLinePickerSaveFailureKeepsPreviousSelection(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace},
	}
	model := newModel(t.Context(), Options{
		Workspace: "/workspace", NoColor: true,
		StatusLine: []statusline.Item{statusline.Workspace},
		SaveStatusLine: func(context.Context, []statusline.Item) error {
			return errors.New("disk full")
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))

	assert.Equal(t, pickerStatusLine, model.picker.kind)
	require.ErrorContains(t, model.picker.err, "disk full")
	assert.Equal(t, []statusline.Item{statusline.Workspace}, model.statusLineItems)
}

func TestStatusLinePickerDurabilityWarningAppliesCommittedSelection(t *testing.T) {
	t.Parallel()

	controller := stubController{
		state:            readyState(),
		configStatusLine: []statusline.Item{statusline.Workspace},
	}
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		SaveStatusLine: func(context.Context, []statusline.Item) error {
			return config.ErrStatusLineDurability
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openStatusLinePicker()
	_, _ = model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	_, command := model.updateStatusLinePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))

	assert.Equal(t, pickerStatusLine, model.picker.kind)
	assert.Empty(t, model.statusLineItems)
	assert.Contains(t, ansi.Strip(model.statusLinePickerView(20)), "status line saved; disk durability is uncertain")
}

func TestStatusLinePickerUsesSafePersistenceErrorMapping(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openStatusLinePicker()
	model.picker.err = errors.Join(config.ErrDecode, errors.New("/Users/example/config.toml: parser detail"))

	content := ansi.Strip(model.statusLinePickerView(20))
	assert.Contains(t, content, "Error: status-line configuration is invalid")
	assert.NotContains(t, content, "/Users/example/config.toml")
	assert.NotContains(t, content, "parser detail")
}
