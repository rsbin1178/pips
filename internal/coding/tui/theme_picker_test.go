//nolint:gosec,wsl_v5 // Theme picker tests intentionally exercise private modes.
package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThemeBackgroundMessagesOnlyChangeAutoSelection(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	assert.Equal(t, config.ThemeAuto, model.themeSelection)
	assert.Equal(t, "default-dark", model.theme.id)

	model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, "default-light", model.theme.id)

	model.themeSelection = "dracula"
	model.applyTheme(model.themeForSelection(model.themeSelection))
	model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, "dracula", model.theme.id)
}

func TestNoColorIgnoresBackgroundMessages(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	before := model.theme
	assert.False(t, model.themeBackgroundKnown)

	_, command := model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Nil(t, command)
	assert.False(t, model.themeBackgroundKnown)
	assert.True(t, model.themeIsDark)
	assert.Equal(t, before.id, model.theme.id)
}

func TestThemePickerRescansAndRestoresComposerOnCancel(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700)) //nolint:gosec // The test asserts the private theme-directory boundary.
	require.NoError(t, os.WriteFile(filepath.Join(directory, "custom.toml"), []byte(
		"schema = \"pips.tui.theme/v1alpha1\"\ninherits = \"nord\"\n",
	), 0o600))

	model := newModel(t.Context(), Options{
		Workspace:      "/workspace",
		ThemeDirectory: directory,
		NoColor:        true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return stubController{state: readyState()}, nil
		},
	})
	controller := stubController{state: readyState()}
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.composer.SetValue("keep this draft")
	model.openCommandPicker()
	model.picker.query = "theme"
	_, _ = model.executeCommand(commandDescriptor{name: "theme", idleOnly: true})

	require.Equal(t, pickerTheme, model.picker.kind)
	content := ansi.Strip(model.themePickerView(30))
	assert.Contains(t, content, "custom · custom · dark · user")
	assert.Contains(t, content, "auto · Auto · adaptive · built-in · current")

	_, command := model.updateThemePickerKey(key(keyEscape))
	driveModelCommands(t, model, command)
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())
}

func TestThemePickerRescansOnEveryOpen(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	model := newModel(t.Context(), Options{
		Workspace:      "/workspace",
		ThemeDirectory: directory,
		NoColor:        true,
		Bootstrap:      func(context.Context, bool) (Controller, error) { return nil, nil },
	})
	model.openThemePicker()
	assert.NotContains(t, themeOptionIDs(model.picker.themes), "ocean")
	model.picker = pickerState{}

	require.NoError(t, os.WriteFile(filepath.Join(directory, "ocean.toml"), []byte(
		"schema = \"pips.tui.theme/v1alpha1\"\ninherits = \"nord\"\n",
	), 0o600))
	model.openThemePicker()
	assert.Contains(t, themeOptionIDs(model.picker.themes), "ocean")
}

func themeOptionIDs(options []themeOption) []string {
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, option.id)
	}

	return ids
}

func TestThemePickerShortTerminalKeepsCursorDiagnosticsAndFooter(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openThemePicker()
	model.picker.themeDiagnostics = []themeDiagnostic{{category: "file", count: 1}}
	model.picker.cursor = len(model.picker.themes) - 1

	content := ansi.Strip(model.themePickerView(3))
	assert.Contains(t, content, "› "+model.picker.themes[model.picker.cursor].id)
	assert.Contains(t, content, "Notice: 1 custom theme ignored")
	assert.Contains(t, content, "↑/↓ choose · Enter apply · Esc cancel")
	assert.LessOrEqual(t, len(strings.Split(content, "\n")), 3)
}

func TestThemePickerPersistenceErrorUsesSafeCategoryInView(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openThemePicker()
	model.picker.err = fmt.Errorf(
		"%w: /Users/example/config.toml: parser detail: permission denied",
		config.ErrDecode,
	)

	content := ansi.Strip(model.themePickerView(5))
	assert.Contains(t, content, "Error: theme configuration is invalid")
	assert.NotContains(t, content, "/Users/example/config.toml")
	assert.NotContains(t, content, "parser detail")
	assert.NotContains(t, content, "permission denied")
}

func TestThemePickerClearsRecoveredSelectionDiagnostic(t *testing.T) {
	t.Parallel()

	controller := stubController{state: readyState()}
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		SaveTheme: func(context.Context, string) error { return nil },
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.themeDiagnostics = []themeDiagnostic{{category: themeDiagnosticSelection, count: 1}}
	model.openThemePicker()
	for index, option := range model.picker.themes {
		if option.id == "dracula" {
			model.picker.cursor = index
			break
		}
	}

	_, command := model.updateThemePickerKey(key(keyEnter))
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))
	assert.NotContains(t, model.themeDiagnostics, themeDiagnostic{category: themeDiagnosticSelection, count: 1})
}

func TestThemePickerSaveAppliesThemeAndKeepsStateOnFailure(t *testing.T) {
	t.Parallel()

	controller := stubController{state: readyState()}
	var saved string
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		SaveTheme: func(_ context.Context, value string) error {
			saved = value

			return nil
		},
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openThemePicker()
	for index, option := range model.picker.themes {
		if option.id == "dracula" {
			model.picker.cursor = index
			break
		}
	}
	_, command := model.updateThemePickerKey(key(keyEnter))
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))
	assert.Equal(t, "dracula", saved)
	assert.Equal(t, "dracula", model.themeSelection)
	assert.Equal(t, "dracula", model.theme.id)
	assert.Equal(t, pickerNone, model.picker.kind)

	model = newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		SaveTheme: func(context.Context, string) error { return errors.New("disk full") },
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openThemePicker()
	_, command = model.updateThemePickerKey(key(keyEnter))
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))
	assert.Equal(t, pickerTheme, model.picker.kind)
	require.ErrorContains(t, model.picker.err, "disk full")
	assert.Equal(t, config.ThemeAuto, model.themeSelection)
	assert.Equal(t, "default-dark", model.theme.id)

	model = newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		SaveTheme: func(context.Context, string) error { return config.ErrThemeDurability },
		Bootstrap: func(context.Context, bool) (Controller, error) { return controller, nil },
	})
	_, _ = model.Update(bootstrapResult{controller: controller})
	model.openThemePicker()
	for index, option := range model.picker.themes {
		if option.id == "dracula" {
			model.picker.cursor = index
			break
		}
	}
	_, command = model.updateThemePickerKey(key(keyEnter))
	require.NotNil(t, command)
	_, _ = model.Update(commandMessage(t, command))
	assert.Equal(t, pickerTheme, model.picker.kind)
	assert.Equal(t, "dracula", model.themeSelection)
	assert.Contains(t, ansi.Strip(model.themePickerView(5)), "theme saved; disk durability is uncertain")
}
