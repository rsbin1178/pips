package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectionCommandsPrintIntoScrollback(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Kind: "modified", Path: "main.go"}},
	}
	_, command := model.executeCommand(commandDescriptor{name: "help"})

	assert.Contains(t, driveModelCommandsCapture(t, model, command), "Help")
	assert.Equal(t, routeNone, model.route.kind)
}

// TestInspectionOutputWaitsForAnOpenRoute pins the print path: a report that
// arrives while a route owns the screen is held instead of landing above the
// preview, and it prints once the route returns.
func TestInspectionOutputWaitsForAnOpenRoute(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	model.openMCPRoute()
	require.Equal(t, routeMCP, model.route.kind)

	command := model.printInspection("Status", "held while a route owns the screen")
	assert.Nil(t, command, "no native write may land above the route")
	assert.Empty(t, model.presentation.writes)

	printed := driveModelCommandsCapture(t, model, model.closeRouteToParent())
	assert.Contains(t, printed, "held while a route owns the screen")
}

func TestStatusReportOmitsTheMissingRepositoryNotice(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.state.Diagnostics = []coding.IntegrationDiagnostic{
		suppressedDiagnosticFixture(),
		keptDiagnosticFixture(),
	}

	status := statusPageText(model, statusTabStatus)

	assert.Contains(t, status, keptDiagnosticFixture().Message)
	assert.NotContains(t, status, suppressedDiagnosticFixture().Message)
	assert.NotContains(t, status, diagnosticCodeNotRepository)
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
