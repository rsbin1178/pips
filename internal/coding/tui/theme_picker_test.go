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
	uv "github.com/charmbracelet/ultraviolet"
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

// TestThemeBackgroundMessageRestampsAnExplicitSelection pins that a detected
// canvas revises the surfaces an explicit selection may paint without replacing
// the selection itself.
func TestThemeBackgroundMessageRestampsAnExplicitSelection(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.themeSelection = "dracula"
	model.applyTheme(model.themeForSelection(model.themeSelection))
	require.Equal(t, "dracula", model.theme.id)
	assert.False(t, model.theme.codeSurface, "the canvas is unknown before detection")

	model.Update(tea.BackgroundColorMsg{Color: color.Black})
	assert.Equal(t, "dracula", model.theme.id, "an explicit selection is never replaced")
	assert.True(t, model.theme.codeSurface, "a dark canvas is the one dracula was built for")

	model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, "dracula", model.theme.id)
	assert.False(t, model.theme.codeSurface, "a light canvas is not dracula's own")
}

// TestThemePickerMarksTheOppositePolarity pins the hint that a theme is built for
// the other terminal background. A light palette inside a dark terminal is
// unreadable rather than merely ugly, so the picker says which rows do not fit
// the canvas it detected.
func TestThemePickerMarksTheOppositePolarity(t *testing.T) {
	t.Parallel()

	dark := readyModel(t, false)
	dark.themeBackgroundKnown = true
	dark.themeIsDark = true
	dark.openThemePicker()

	content := ansi.Strip(dark.themePickerView(30))
	assert.Contains(t, content, "default-light "+themePolarityMark)
	assert.Contains(t, content, "solarized-light "+themePolarityMark)
	assert.NotContains(t, content, "default-dark "+themePolarityMark)
	assert.NotContains(t, content, "auto "+themePolarityMark)
	// A palette that borrows the terminal's own colours fits either canvas.
	assert.Contains(t, content, "terminal · Terminal · adaptive · built-in")
	assert.NotContains(t, content, "terminal "+themePolarityMark)
	assert.Contains(t, content, "Notice: "+themePolarityMark+
		" marks a theme built for the other background; this terminal is dark")

	light := readyModel(t, false)
	light.themeBackgroundKnown = true
	light.themeIsDark = false
	light.openThemePicker()

	content = ansi.Strip(light.themePickerView(30))
	assert.Contains(t, content, "default-dark "+themePolarityMark)
	assert.NotContains(t, content, "default-light "+themePolarityMark)
	assert.Contains(t, content, "this terminal is light")

	// Before the terminal answers there is no canvas to disagree with.
	unknown := readyModel(t, false)
	unknown.openThemePicker()
	content = ansi.Strip(unknown.themePickerView(30))
	assert.NotContains(t, content, themePolarityMark)
	assert.NotContains(t, content, "marks a theme built for")
}

// TestThemePickerShortTerminalKeepsThePolarityNotice pins that the new notice
// competes for height like every other trailing line and never costs the cursor
// row or the footer.
func TestThemePickerShortTerminalKeepsThePolarityNotice(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.themeBackgroundKnown = true
	model.themeIsDark = true
	model.openThemePicker()
	model.picker.themeDiagnostics = []themeDiagnostic{{category: "file", count: 1}}
	for index, option := range model.picker.themes {
		if option.id == "solarized-light" {
			model.picker.cursor = index
			break
		}
	}

	content := ansi.Strip(model.themePickerView(3))
	// Three lines keep the cursor row, its mark, and the operating hints; the
	// notice yields its tail to them.
	assert.Contains(t, content, "› solarized-light "+themePolarityMark)
	assert.Contains(t, content, "↑/↓ choose · Enter apply · Esc cancel")
	assert.Contains(t, content, "Notice: "+themePolarityMark)
	assert.LessOrEqual(t, len(strings.Split(content, "\n")), 3)
}

// TestThemePickerPolarityHintReachesTheFrame drives /theme the way an operator
// does and reads the hint off the composed frame, so the mark survives the
// picker's layout and height budget rather than only its row builder.
func TestThemePickerPolarityHintReachesTheFrame(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.Update(tea.BackgroundColorMsg{Color: color.Black})
	require.True(t, model.themeBackgroundKnown)

	model.openCommandPicker()
	model.picker.query = "theme"
	_, _ = model.executeCommand(commandDescriptor{name: "theme", idleOnly: true})
	require.Equal(t, pickerTheme, model.picker.kind)

	frame := ansi.Strip(model.View().Content)
	assert.Contains(t, frame, "default-light "+themePolarityMark)
	assert.Contains(t, frame, "this terminal is dark")

	// The mark follows the selection window, so the last row shows it too.
	for range model.picker.themes {
		if model.picker.themes[model.picker.cursor].id == "solarized-light" {
			break
		}
		model.Update(key(keyDown))
	}
	require.Equal(t, "solarized-light", model.picker.themes[model.picker.cursor].id)
	assert.Contains(t, ansi.Strip(model.View().Content), "solarized-light "+themePolarityMark)
}

// TestThemePickerAsksForTheCanvasWhenItOpens pins the refresh: the picker is where
// every row's polarity is on screen, so it re-asks the terminal instead of showing
// whatever the canvas was at startup.
func TestThemePickerAsksForTheCanvasWhenItOpens(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.openCommandPicker()
	model.picker.query = "theme"
	_, command := model.executeCommand(commandDescriptor{name: "theme", idleOnly: true})
	require.Equal(t, pickerTheme, model.picker.kind)
	require.NotNil(t, command)

	// The request marker itself is what the program turns into the query, so its
	// presence among the commands is the observable effect.
	assert.Contains(t, commandMessages(t, command), tea.RequestBackgroundColor())

	// The answer marks the rows from the canvas it reports.
	for index, option := range model.picker.themes {
		if option.id == themeIDDefaultDark {
			model.picker.cursor = index

			break
		}
	}
	model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, themeIDDefaultLight, model.theme.id)

	// A run that paints no colour never asks: the answer would only move surfaces
	// the run does not draw.
	plain := readyModel(t, true)
	assert.Nil(t, plain.openThemePicker())
	assert.False(t, plain.themeBackgroundKnown)
}

// TestColorSchemeReportReResolvesAuto pins the desktop-appearance signal: a
// terminal that reports the light/dark extension re-resolves auto, keeps an
// explicit selection, and then asks for the terminal's own background, which is
// what the surfaces and the marks describe.
func TestColorSchemeReportReResolvesAuto(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	require.Equal(t, config.ThemeAuto, model.themeSelection)

	_, command := model.Update(uv.DarkColorSchemeEvent{})
	assert.Equal(t, "default-dark", model.theme.id)
	assert.Contains(t, commandMessages(t, command), tea.RequestBackgroundColor())

	_, _ = model.Update(uv.LightColorSchemeEvent{})
	assert.Equal(t, "default-light", model.theme.id)

	model.themeSelection = "dracula"
	model.applyTheme(model.themeForSelection(model.themeSelection))
	_, _ = model.Update(uv.LightColorSchemeEvent{})
	assert.Equal(t, "dracula", model.theme.id, "an explicit selection is never replaced")
	assert.False(t, model.theme.codeSurface, "a light appearance is not dracula's canvas")

	// NO_COLOR ignores the report, exactly as it ignores the background answer.
	plain := readyModel(t, true)
	before := plain.theme.id
	_, command = plain.Update(uv.DarkColorSchemeEvent{})
	assert.Nil(t, command)
	assert.False(t, plain.themeBackgroundKnown)
	assert.Equal(t, before, plain.theme.id)
}

// TestInitEnablesColorSchemeReports pins that the startup sequence asks the
// terminal to report appearance changes, and that a NO_COLOR run does not.
func TestInitEnablesColorSchemeReports(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	assert.Contains(t, commandMessages(t, model.Init()), tea.RawMsg{Msg: ansi.SetModeLightDark})

	plain := readyModel(t, true)
	assert.NotContains(t, commandMessages(t, plain.Init()), tea.RawMsg{Msg: ansi.SetModeLightDark})
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
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
