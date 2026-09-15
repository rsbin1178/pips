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
)

func TestStartupBannerPrintsOnceBeforeStableTimeline(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("inspect the repository")}
	controller := stubController{state: state}
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	_, command := model.Update(bootstrapResult{controller: controller})
	printed := driveModelCommandsCapture(t, model, command)

	assert.Contains(t, printed, "Pips")
	assert.Contains(t, printed, "✻ Pips")
	assert.Contains(t, printed, "workspace")
	assert.Contains(t, printed, "openai/test-model")
	assert.Contains(t, printed, "Type / for commands")
	assert.Contains(t, printed, "┌")
	assert.Contains(t, printed, "└")
	assert.Less(t, strings.Index(printed, "Pips"), strings.Index(printed, "inspect the repository"))
	assert.NotContains(t, model.View().Content, "✻ Pips")
	assert.NotContains(t, model.View().Content, "Start a conversation")

	model.resetScrollback()
	reprinted := commandOutput(model.commitStartupOutput())
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
