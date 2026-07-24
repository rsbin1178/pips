package tui

import (
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
		{name: "diff", command: commandDescriptor{name: "diff"}, want: "Workspace changes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModelWithController(t, newOverlayController(readyState()), true)
			model.state.Changes = &coding.WorkspaceChanged{
				Entries: []coding.WorkspaceChange{{Kind: "modified", Path: "main.go"}},
			}
			_, command := model.executeCommand(test.command)
			assert.Contains(t, commandOutput(command), test.want)
			assert.Equal(t, routeNone, model.route.kind)
		})
	}
}
