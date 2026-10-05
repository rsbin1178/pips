//nolint:wsl_v5 // Wheel gestures and their screen assertions stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wheelModel is a fullscreen viewport with enough history to scroll and a
// controllable environment, because the wheel policy depends on the multiplexer.
func wheelModel(t *testing.T, environment []string) *Model {
	t.Helper()

	state := readyState()
	for index := range 40 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("ROW-%03d", index)))
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.options.Environment = environment
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.rerenderTranscript(true)

	return model
}

// TestFullscreenWheelScrollsTheManagedTranscript is the AC6 contract: with the
// mouse captured, a wheel notch must move the viewport, and scrolling past the
// bottom must restore following.
func TestFullscreenWheelScrollsTheManagedTranscript(t *testing.T) {
	t.Parallel()

	model := wheelModel(t, nil)
	require.True(t, model.transcriptScroll.follow)
	bottom := model.transcriptScroll.offset
	require.Positive(t, bottom, "the fixture must overflow the window")

	updated, command := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, Y: 4})
	require.Same(t, model, updated)
	assert.Nil(t, command)
	assert.False(t, model.transcriptScroll.follow, "scrolling up pauses following")
	assert.Equal(t, bottom-wheelLinesDefault, model.transcriptScroll.offset)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.True(t, model.transcriptScroll.follow, "reaching the bottom resumes following")
	assert.Equal(t, bottom, model.transcriptScroll.offset)
}

// TestWheelDeltaIsConservativeUnderAMultiplexer pins the per-terminal policy: one
// line inside TMUX/Zellij/screen, three elsewhere.
func TestWheelDeltaIsConservativeUnderAMultiplexer(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name        string
		environment []string
		want        int
	}{
		{name: "plain terminal", environment: nil, want: wheelLinesDefault},
		{
			name:        "tmux",
			environment: []string{"TMUX=/tmp/tmux,1,0", "TERM=tmux-256color"},
			want:        wheelLinesMultiplexer,
		},
		{name: "zellij", environment: []string{"ZELLIJ=1"}, want: wheelLinesMultiplexer},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			model := wheelModel(t, testCase.environment)
			bottom := model.transcriptScroll.offset

			model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
			assert.Equal(t, bottom-testCase.want, model.transcriptScroll.offset)
		})
	}
}

// TestWheelIsRoutedToTheSurfaceThatOwnsTheScreen keeps the gesture from reaching a
// region the user cannot see: whatever owns the screen takes the notch, and the
// transcript underneath never moves.
func TestWheelIsRoutedToTheSurfaceThatOwnsTheScreen(t *testing.T) {
	t.Parallel()

	t.Run("list route", func(t *testing.T) {
		t.Parallel()

		model := wheelModel(t, nil)
		model.route = newSessionPickerState("", model.theme, true)
		model.route.sessions = []session.Metadata{{ID: "alpha"}, {ID: "beta"}}
		model.setLayout()
		bottom := model.transcriptScroll.offset

		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
		assert.Equal(t, bottom, model.transcriptScroll.offset, "the transcript is untouched")
		assert.Equal(t, 1, model.route.cursor, "the route that owns the screen takes the notch")
	})

	t.Run("picker", func(t *testing.T) {
		t.Parallel()

		model := wheelModel(t, nil)
		model.picker.kind = pickerCommand
		bottom := model.transcriptScroll.offset

		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		assert.Equal(t, bottom, model.transcriptScroll.offset)
	})

	t.Run("transcript mode", func(t *testing.T) {
		t.Parallel()

		model := wheelModel(t, nil)
		_, _ = model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)
		bottom := model.transcriptScroll.offset

		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		assert.Equal(t, bottom, model.transcriptScroll.offset,
			"the terminal owns the wheel while the escape hatch is open")
	})
}

// TestStatusLineReportsAPausedReader is the visible half of R4: a reader who is
// not following the tail can see that the newest output is off screen.
func TestStatusLineReportsAPausedReader(t *testing.T) {
	t.Parallel()

	model := wheelModel(t, nil)
	assert.NotContains(t, ansi.Strip(model.statusLine()), "scrolled ·")

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Contains(t, ansi.Strip(model.statusLine()), "scrolled · End for latest")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.NotContains(t, ansi.Strip(model.statusLine()), "scrolled ·")
	assert.True(t, model.transcriptScroll.follow)
}

// TestHelpStatesTheModifierDragFallback records the user-visible cost of mouse
// capture where a user can read it.
func TestHelpStatesTheModifierDragFallback(t *testing.T) {
	t.Parallel()

	fullscreen := wheelModel(t, nil)
	help := fullscreen.historyHelpLine()
	assert.Contains(t, help, "wheel")
	assert.Contains(t, help, "Shift")
	assert.Contains(t, help, "[tui] mouse = false")

	inline := readyModel(t, true)
	assert.NotContains(t, inline.historyHelpLine(), "Shift",
		"inline mode never captures the mouse, so there is nothing to bypass")
	assert.Contains(t, inline.historyHelpLine(), "drag normally to select")
}

// TestWheelScrollsTheToolDetailPanel is the Ctrl+T contract: the panel body is a
// viewport, so a wheel notch must move it exactly like PgUp/PgDn, and the panel
// must clamp at its own ends instead of scrolling into blank space.
func TestWheelScrollsTheToolDetailPanel(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("segment ", 60)
	state := detailState(coding.ToolState{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell",
			Arguments: ai.JSON(`{"command":"ls","note":"` + long + `"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("call-1", "shell", "stdout:\n"+long+"\n"),
	})
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.Equal(t, routeToolDetail, model.route.kind)

	maximum := model.toolDetailMaximumOffset()
	require.Positive(t, maximum, "the fixture must overflow the panel")

	_, command := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Nil(t, command)
	assert.Equal(t, wheelLinesDefault, model.route.offset)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, Y: 3})
	assert.Equal(t, 2*wheelLinesDefault, model.route.offset)

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	assert.Equal(t, maximum, model.route.offset, "the panel stops at its last row")

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	}
	assert.Equal(t, 0, model.route.offset, "the panel stops at its first row")
}

// TestWheelScrollsTheChildSessionPanel covers the other surface Ctrl+T opens: a
// subagent call opens the child session instead of a Tool document.
func TestWheelScrollsTheChildSessionPanel(t *testing.T) {
	t.Parallel()

	claim := strings.Repeat("the reviewer explains the risk in detail ", 4)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			Identity: subagent.AgentIdentity{ID: "agent-1", Name: "reviewer"},
			Role:     subagent.RoleReview, State: subagent.StateSucceeded,
			Model: "openai/model",
		},
		Transcript: []ai.Message{
			ai.UserText("Review the timeline change."),
			ai.AssistantText("{}"),
		},
		Result: subagent.ReviewResult{
			Summary: "Reviewed the timeline change.",
			Findings: []subagent.ReviewFinding{{
				Severity: "high", Title: "Diagnostics never wrap",
				Path: "internal/coding/tui/timeline.go", Line: 12,
				Evidence: claim, Recommendation: claim,
			}},
		},
	}

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	child := testSubagentState(detail)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 10})
	model.route = routeState{
		kind: routeChild, childSessionID: "child-1", detail: &detail, childState: &child,
	}

	maximum := model.subagentRouteMaximumOffset()
	require.Positive(t, maximum, "the fixture must overflow the panel")

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.offset)

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	assert.Equal(t, maximum, model.route.offset)

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	}
	assert.Equal(t, 0, model.route.offset)
}

// TestWheelScrollsTheAgentList pins the list shape the user asked for: the Agent
// list has an independent viewport, so the wheel scrolls it exactly like its
// PgUp/PgDn keys and leaves the selection where it was.
func TestWheelScrollsTheAgentList(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	for index := range 12 {
		controller.agents = append(controller.agents, subagent.Summary{
			ChildSessionID: fmt.Sprintf("child-%02d", index),
			Role:           subagent.RoleExplore,
			State:          subagent.StateSucceeded,
			TaskPreview:    fmt.Sprintf("Inspect package %02d", index),
			Model:          "openai/model",
			Code:           "ok",
		})
	}

	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	driveModelCommands(t, model, model.openAgentsRoute())
	require.Equal(t, routeAgents, model.route.kind)

	maximum := max(0, model.routeScrollLineCount()-model.routeScrollRows())
	require.Positive(t, maximum, "the fixture must overflow the list window")

	cursor := model.route.cursor
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.offset)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Equal(t, 0, model.route.offset)

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	assert.Equal(t, maximum, model.route.offset, "the list stops at its last row")
	assert.Equal(t, cursor, model.route.cursor, "the wheel never moves the selection")
}

// TestWheelScrollsTheTeamRouteViewport covers the other full-area list: a Team
// stage outside the composer band has a viewport too.
func TestWheelScrollsTheTeamRouteViewport(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	for index := range 6 {
		controller.recoveries = append(controller.recoveries, coding.TeamRecoveryCandidate{
			TeamID:           team.ID(fmt.Sprintf("team-%d", index)),
			ResourceRevision: 9,
			ResourceState:    teamstate.StateInterrupted,
			TeamStatus:       team.StatusActive,
			Disposition:      coding.TeamRecoveryResume,
		})
	}

	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 10})
	_, command := model.Update(controlResultMsg{operation: operationResume})
	driveModelCommands(t, model, command)

	require.Equal(t, routeTeam, model.route.kind)
	require.Equal(t, teamRouteRecovery, model.route.team.stage)

	maximum := model.teamRouteMaximumOffset()
	require.Positive(t, maximum, "the fixture must overflow the Team view")

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.offset)

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	assert.Equal(t, maximum, model.route.offset, "the route stops at its last row")

	for range 100 {
		model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	}
	assert.Equal(t, 0, model.route.offset)
}

// TestWheelLeavesInlineTeamStagesAlone pins the one Team surface that is not a
// full-area list: an inline stage lives in the composer's band and its offset is
// measured against a different height, so the wheel is dropped there exactly as it
// is over any other overlay that owns the screen.
func TestWheelLeavesInlineTeamStagesAlone(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	driveModelCommands(t, model, model.openTeamRoute(""))

	require.Equal(t, routeTeam, model.route.kind)
	require.True(t, model.teamRouteIsInline())

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, 0, model.route.offset)
}

// TestWheelMovesASelectionListCursor covers the routes whose window follows the
// cursor: the pickers and the tree have no viewport of their own, so the wheel
// moves the selection exactly as their up/down keys do, including the wrap.
func TestWheelMovesASelectionListCursor(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{
		{ID: "alpha", Preview: "first question"},
		{ID: "beta", Preview: "second question"},
		{ID: "gamma", Preview: "third question"},
		{ID: "delta", Preview: "fourth question"},
	}

	model := readyModelWithController(t, controller, true)
	model.Update(key("/"))
	model.Update(tea.KeyPressMsg{Text: "res"})

	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)
	require.Equal(t, routeSessions, model.route.kind)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.cursor)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Equal(t, 0, model.route.cursor)

	// A wheel is N key presses, so it wraps exactly where the keys wrap.
	model.route.cursor = len(controller.sessions) - 1
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault-1, model.route.cursor)
}

func TestWheelMovesTheTreeSelection(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	for index := range 12 {
		controller.tree.Nodes = append(controller.tree.Nodes, coding.SessionNode{
			ID:           fmt.Sprintf("node-%02d", index),
			Kind:         coding.SessionNodeMessage,
			CreatedAt:    time.Unix(int64(index), 0).UTC(),
			OnActivePath: true,
		})
	}
	controller.tree.SessionID = "session-1"
	controller.tree.LeafID = "node-11"
	controller.tree.TotalNodes = len(controller.tree.Nodes)

	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.Update(commandMessage(t, model.openTreeRoute(false)))
	require.Equal(t, routeTree, model.route.kind)
	require.NotEmpty(t, model.filteredTreeNodes())

	model.route.cursor = 0
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.cursor,
		"the wheel moves the selection the way the keys do")
	assert.Equal(t, model.treeRouteScrollOffset(model.filteredTreeNodes()), model.route.offset,
		"the window follows the selection")

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Equal(t, 0, model.route.cursor)
}

// TestWheelMovesAPickerDropdownSelection covers the dropdowns the ready view shows
// above the composer. They are lists too, so the wheel moves their selection
// exactly as their up/down keys do, and the transcript underneath stays put.
func TestWheelMovesAPickerDropdownSelection(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	model.openCommandPicker()
	require.Equal(t, pickerCommand, model.picker.kind)

	commands := model.filteredCommands()
	require.Greater(t, len(commands), wheelLinesDefault)
	offset := model.transcriptScroll.offset

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.picker.cursor)

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Equal(t, 0, model.picker.cursor)
	assert.Equal(t, offset, model.transcriptScroll.offset, "the transcript stays put")

	// Wrapping is the keys' behaviour, not a separate rule for the wheel.
	model.picker.cursor = len(commands) - 1
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault-1, model.picker.cursor)
}
