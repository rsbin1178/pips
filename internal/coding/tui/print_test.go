package tui

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
)

func TestInspectionCommandsPrintIntoScrollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command commandDescriptor
		want    string
	}{
		{name: "help", command: commandDescriptor{name: "help"}, want: "Help"},
		{name: "status", command: commandDescriptor{name: "status"}, want: "Status"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModelWithController(t, newOverlayController(readyState()), true)
			model.state.Changes = &coding.WorkspaceChanged{
				Entries: []coding.WorkspaceChange{{Kind: "modified", Path: "main.go"}},
			}
			_, command := model.executeCommand(test.command)
			assert.Contains(t, driveModelCommandsCapture(t, model, command), test.want)
			assert.Equal(t, routeNone, model.route.kind)
		})
	}
}

func TestPrintInspectionRemovesTerminalControlSequences(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	command := model.printInspection(
		"Workspace changes",
		"safe\x1b[2J\x1b]52;c;ZXhmaWx0cmF0ZQ==\a\rtext",
	)
	output := driveModelCommandsCapture(t, model, command)

	assert.Contains(t, output, "safetext")
	assert.NotContains(t, output, "\x1b")
	assert.NotContains(t, output, "\a")
	assert.False(t, strings.ContainsRune(output, '\r'))
}
