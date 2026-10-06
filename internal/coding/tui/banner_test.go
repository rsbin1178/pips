package tui

import (
	"context"
	"image/color"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupBannerPrintsOnceBeforeStableTimeline(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("inspect the repository")}
	controller := stubController{state: state}
	model := newModel(t.Context(), Options{
		PinPresentation: true, Screen: ScreenInline, AltScreen: AltScreenNever,
		Workspace: "/workspace",
		NoColor:   true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	model.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: defaultHeight})
	_, command := model.Update(bootstrapResult{controller: controller})
	printed := driveModelCommandsCapture(t, model, command)

	assert.Contains(t, printed, "Pips")
	assert.Contains(t, printed, "✻ Pips")
	assert.Contains(t, printed, "workspace")
	assert.Contains(t, printed, "openai/test-model")
	assert.Contains(t, printed, "Type / for commands")
	// The header is plain transcript text: no box, and every row starts in the
	// same content column a message uses.
	assert.NotContains(t, printed, "┌")
	assert.NotContains(t, printed, "└")
	inset := strings.Repeat(" ", transcriptHorizontalInset)
	for row := range strings.SplitSeq(printed, "\n") {
		if strings.TrimSpace(row) == "" {
			continue
		}

		assert.True(t, strings.HasPrefix(row, inset), "header row starts at the content column: %q", row)
	}

	assert.Less(t, strings.Index(printed, "Pips"), strings.Index(printed, "inspect the repository"))
	assert.NotContains(t, model.View().Content, "✻ Pips")
	assert.NotContains(t, model.View().Content, "Start a conversation")

	model.resetScrollback()
	reprinted := modelCommandOutput(model, model.commitStartupOutput())
	assert.NotContains(t, reprinted, "Pips")
	assert.Contains(t, reprinted, "inspect the repository")
}

func TestNewSessionBannerPrintsOnlyAfterSuccessfulNew(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)

	for range 2 {
		_, command := model.Update(controlResultMsg{operation: operationNew})
		newOutput := driveModelCommandsCapture(t, model, command)
		assert.Equal(t, 1, strings.Count(newOutput, "Pips"))
		assert.Contains(t, newOutput, "✻ Pips")
	}

	operations := []controlOperation{
		operationResume,
		operationFork,
		operationModel,
		operationReload,
	}
	for _, operation := range operations {
		_, command := model.Update(controlResultMsg{operation: operation})
		output := driveModelCommandsCapture(t, model, command)
		assert.NotContains(t, output, "Pips")
	}

	_, command := model.Update(controlResultMsg{
		operation: operationNew,
		err:       assert.AnError,
	})
	failedOutput := driveModelCommandsCapture(t, model, command)
	assert.NotContains(t, failedOutput, "Pips")

	_, command = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	assert.Nil(t, command)
	_, command = model.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Nil(t, command)
}

// TestFullscreenNewSessionReplacesThePreviousBanner is the managed-viewport
// counterpart of the inline contract: the transcript shows one session at a
// time, so repeated /new leaves a single banner instead of stacking one banner
// per replacement.
func TestFullscreenNewSessionReplacesThePreviousBanner(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	// Tall enough that an accumulated second banner would be visible.
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	assert.Len(t, model.notices, 1, "the startup banner is the first managed record")

	for range 3 {
		_, command := model.Update(controlResultMsg{operation: operationNew})
		driveModelCommands(t, model, command)

		frame := ansi.Strip(model.View().Content)
		assert.Equal(t, 1, strings.Count(frame, "coding agent"), "one banner per session")
		assert.Len(t, model.notices, 1, "the replaced session's notices are dropped")
	}

	_, command := model.Update(controlResultMsg{operation: operationResume})
	driveModelCommands(t, model, command)

	assert.Empty(t, model.notices, "resume prints no banner and drops the previous session's")
	assert.NotContains(t, ansi.Strip(model.View().Content), "coding agent")
}

// TestStartupBannerLogoMatchesTheHeaderLines pins the header's height: the block
// wordmark is exactly as tall as the four lines of text beside it, so the banner
// ends on the same row as `Type / for commands` with no trailing blank row.
func TestStartupBannerLogoMatchesTheHeaderLines(t *testing.T) {
	t.Parallel()

	banner := ansi.Strip(renderStartupBanner(startupBannerContext{
		width:     72,
		workspace: "workspace",
		model:     "openai/test-model",
		theme:     themeDark,
		noColor:   true,
	}))
	rows := strings.Split(banner, "\n")
	require.Len(t, rows, 4, "the wordmark is as tall as the header lines")

	for index, want := range []string{"coding agent", "workspace", "model", "Type / for commands"} {
		assert.Contains(t, rows[index], "█", "row %d carries part of the wordmark", index)
		assert.Contains(t, rows[index], want, "row %d carries its header line", index)
	}
}

func TestStartupBannerIsBoundedAcrossThemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		width   int
		theme   colorTheme
		noColor bool
	}{
		{name: "dark", width: 72, theme: themeDark},
		{name: "light", width: 40, theme: themeLight},
		{name: "narrow no color", width: 18, theme: themeDark, noColor: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			banner := renderStartupBanner(startupBannerContext{
				width:     test.width,
				workspace: "workspace",
				model:     "provider/a-very-long-model-name",
				theme:     test.theme,
				noColor:   test.noColor,
			})
			assert.Contains(t, banner, "Pips")

			if test.noColor {
				assert.NotContains(t, banner, "\x1b[")
			}

			if test.width >= 38 {
				assert.Contains(t, banner, "██")
			} else {
				assert.NotContains(t, banner, "██")
			}

			for line := range strings.SplitSeq(banner, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), test.width)
			}
		})
	}
}

func commandOutput(command tea.Cmd) string {
	return strings.Join(commandOutputs(command), "\n")
}

func commandOutputs(command tea.Cmd) []string {
	if command == nil {
		return nil
	}

	message := command()

	value := reflect.ValueOf(message)
	if !value.IsValid() || value.Type().PkgPath() != "charm.land/bubbletea/v2" {
		return nil
	}

	switch value.Type().Name() {
	case "printLineMessage":
		return []string{value.FieldByName("messageBody").String()}
	case "sequenceMsg":
		outputs := make([]string, 0, value.Len())
		for index := range value.Len() {
			command, ok := value.Index(index).Interface().(tea.Cmd)
			if !ok {
				continue
			}

			outputs = append(outputs, commandOutputs(command)...)
		}

		return outputs
	default:
		return nil
	}
}

// modelCommandOutput is the projection-test driver. Unlike the old extraction
// helper it acknowledges native writes, so successive calls exercise the FIFO.
func modelCommandOutput(model *Model, command tea.Cmd) string {
	return strings.Join(modelCommandOutputs(model, command), "\n")
}

func modelCommandOutputs(model *Model, command tea.Cmd) []string {
	if command == nil {
		return nil
	}

	message := command()
	if done, ok := message.(scrollbackWriteDoneMsg); ok {
		return modelCommandOutputs(model, model.finishScrollbackWrite(done.sequence))
	}

	if batch, ok := message.(tea.BatchMsg); ok {
		var outputs []string
		for _, cmd := range batch {
			outputs = append(outputs, modelCommandOutputs(model, cmd)...)
		}

		return outputs
	}

	value := reflect.ValueOf(message)
	if !value.IsValid() || value.Type().PkgPath() != "charm.land/bubbletea/v2" {
		return nil
	}

	switch value.Type().Name() {
	case "printLineMessage":
		return []string{value.FieldByName("messageBody").String()}
	case "sequenceMsg":
		var outputs []string

		for index := range value.Len() {
			if cmd, ok := value.Index(index).Interface().(tea.Cmd); ok {
				outputs = append(outputs, modelCommandOutputs(model, cmd)...)
			}
		}

		return outputs
	default:
		return nil
	}
}
