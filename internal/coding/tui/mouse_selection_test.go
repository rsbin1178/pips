//nolint:wsl_v5 // Gesture setup, the copy and its assertions stay adjacent.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectionModel builds a fullscreen viewport over one user message per row and
// paints it, so a test can drag over the rows a reader actually sees.
func selectionModel(t *testing.T, noColor bool, markers ...string) (*Model, *[]TextSaveRequest) {
	t.Helper()

	state := readyState()
	for _, marker := range markers {
		state.Transcript = append(state.Transcript, ai.UserText(marker))
	}

	model := fullscreenModel(t, stubController{state: state}, noColor)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	model.rerenderTranscript(true)
	model.View()

	calls := &[]TextSaveRequest{}
	model.options.SaveText = captureSaver(calls)

	return model, calls
}

// paintedRow returns the frame row showing the given marker, so a gesture aims at
// what is on screen rather than at an assumed offset.
func paintedRow(t *testing.T, model *Model, marker string) int {
	t.Helper()

	rows := strings.Split(ansi.Strip(model.transcriptScroll.visible()), "\n")
	for index, row := range rows {
		if strings.Contains(row, marker) {
			return index
		}
	}

	t.Fatalf("marker %q is not on screen", marker)

	return -1
}

func TestSelectionSpanCoversTheDrag(t *testing.T) {
	t.Parallel()

	start := selectionPoint{row: 4, column: 3}
	end := selectionPoint{row: 6, column: 5}

	cases := []struct {
		name  string
		row   int
		width int
		want  [2]int
	}{
		{"above the selection", 3, 20, [2]int{0, 0}},
		{"below the selection", 7, 20, [2]int{0, 0}},
		{"the first row keeps its start column", 4, 20, [2]int{3, 20}},
		{"a middle row is selected whole", 5, 20, [2]int{0, 20}},
		{"the last row ends at the focus column", 6, 20, [2]int{0, 5}},
		{"a row shorter than the drag clamps", 5, 4, [2]int{0, 4}},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			left, right := selectionSpan(test.row, start, end, test.width)
			assert.Equal(t, test.want, [2]int{left, right})
		})
	}

}

// TestDragSelectionMadeBackwardsCopiesTheSameText asserts the gesture orders its
// ends before the span is computed, so dragging up copies what dragging down did.
func TestDragSelectionMadeBackwardsCopiesTheSameText(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-A", "MARKER-B")
	first := paintedRow(t, model, "MARKER-A")
	second := paintedRow(t, model, "MARKER-B")
	require.Less(t, first, second)

	// Press on the lower entry and drag up to the first one.
	model.handleMouse(tea.MouseClickMsg{X: 40, Y: second, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 0, Y: first, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: 0, Y: first, Button: tea.MouseLeft})
	require.NotNil(t, command)

	runCommandTree(t, command)
	require.Len(t, *calls, 1)
	assert.Equal(t, "❯ MARKER-A\n\n❯ MARKER-B", (*calls)[0].Content)
}

func TestDragSelectionCopiesTheAddressedRows(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-A", "MARKER-B")
	first := paintedRow(t, model, "MARKER-A")
	second := paintedRow(t, model, "MARKER-B")
	require.Less(t, first, second)

	model.handleMouse(tea.MouseClickMsg{X: 0, Y: first, Button: tea.MouseLeft})
	require.True(t, model.selection.dragging, "the press arms the selection")
	model.handleMouse(tea.MouseMotionMsg{X: 40, Y: second, Button: tea.MouseLeft})

	command := model.handleMouse(tea.MouseReleaseMsg{X: 40, Y: second, Button: tea.MouseLeft})
	require.NotNil(t, command, "the release copies what the drag addressed")

	messages := runCommandTree(t, command)
	require.Len(t, *calls, 1)
	assert.Equal(t, TextKindCopy, (*calls)[0].Kind)
	// The blank row that separates the two entries is on screen, so it is part of
	// what the drag addressed.
	assert.Equal(t, "❯ MARKER-A\n\n❯ MARKER-B", (*calls)[0].Content)
	assert.Equal(t, []string{"❯ MARKER-A\n\n❯ MARKER-B"}, clipboardTexts(messages))

	saved, ok := findTextSaved(messages)
	require.True(t, ok)
	_, notice := model.Update(saved)
	require.NotNil(t, notice)
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "/resolved/copy")

	// The highlight stays until the confirmation that explains it expires.
	assert.True(t, model.selection.visible)
	model.Update(statusNoticeExpiredMsg{generation: model.statusNoticeSeq})
	assert.False(t, model.selection.visible, "the selection and its notice expire together")
}

func TestDragSelectionCopiesOnlyTheAddressedCells(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-ALPHA")
	row := paintedRow(t, model, "MARKER-ALPHA")

	// The transcript inset and the prompt glyph occupy the first four cells, so
	// a drag that starts after them copies the text without the glyph.
	model.handleMouse(tea.MouseClickMsg{X: 4, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 10, Y: row, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: 10, Y: row, Button: tea.MouseLeft})
	require.NotNil(t, command)

	runCommandTree(t, command)
	require.Len(t, *calls, 1)
	assert.Equal(t, "MARKER", (*calls)[0].Content)
}

func TestDragSelectionSkipsASelectionThatIsOnlyPadding(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-A")
	row := paintedRow(t, model, "MARKER-A")

	// The cells past the row's text are the renderer's padding, and a reader who
	// drags only over them has addressed nothing.
	model.handleMouse(tea.MouseClickMsg{X: 38, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 39, Y: row, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: 39, Y: row, Button: tea.MouseLeft})
	require.NotNil(t, command)

	runCommandTree(t, command)
	assert.Empty(t, *calls, "padding is not copied")
	assert.False(t, model.selection.visible)
}

func TestSelectionHighlightKeepsTheRowWidth(t *testing.T) {
	t.Parallel()

	model, _ := selectionModel(t, false, "MARKER-A")
	row := paintedRow(t, model, "MARKER-A")
	before := strings.Split(model.transcriptScroll.visible(), "\n")

	model.handleMouse(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 20, Y: row, Button: tea.MouseLeft})

	after := strings.Split(model.highlightSelectionWindow(model.transcriptScroll.visible()), "\n")
	require.Greater(t, len(after), row)
	assert.NotEqual(t, before[row], after[row], "the addressed cells are painted")
	assert.Equal(t, ansi.StringWidth(before[row]), ansi.StringWidth(after[row]),
		"the hit map and the cursor were measured against this width")

	// NO_COLOR carries the same text and no highlight.
	plain, _ := selectionModel(t, true, "MARKER-A")
	plainRow := paintedRow(t, plain, "MARKER-A")
	plain.handleMouse(tea.MouseClickMsg{X: 0, Y: plainRow, Button: tea.MouseLeft})
	plain.handleMouse(tea.MouseMotionMsg{X: 20, Y: plainRow, Button: tea.MouseLeft})
	assert.Equal(t,
		plain.transcriptScroll.visible(),
		plain.highlightSelectionWindow(plain.transcriptScroll.visible()),
	)
}

func TestToggleMouseCaptureReleasesTheTerminal(t *testing.T) {
	t.Parallel()

	model, _ := selectionModel(t, true, "MARKER-A")
	require.True(t, model.mouseReportingEnabled())
	assert.Equal(t, tea.MouseModeAllMotion, model.View().MouseMode)

	_, command := model.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	assert.False(t, model.mouseReportingEnabled(), "Ctrl+R hands the mouse back to the terminal")
	assert.Equal(t, tea.MouseModeNone, model.View().MouseMode)
	assert.Contains(t, model.statusNotice, "Ctrl+R captures again")

	// With the capture released the viewport receives no gesture at all.
	assert.Nil(t, model.handleMouse(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft}))

	_, command = model.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	assert.True(t, model.mouseReportingEnabled())
	assert.Equal(t, tea.MouseModeAllMotion, model.View().MouseMode)
}

func TestEscapeClearsTheSelectionBeforeCanceling(t *testing.T) {
	t.Parallel()

	model, _ := selectionModel(t, true, "MARKER-A")
	row := paintedRow(t, model, "MARKER-A")

	model.handleMouse(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 10, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseReleaseMsg{X: 10, Y: row, Button: tea.MouseLeft})
	require.True(t, model.selection.visible)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	messages := runCommandTree(t, command)
	require.Len(t, messages, 1)
	assert.IsType(t, selectionRedrawMsg{}, messages[0], "Escape closes the selection, not the session")
	assert.False(t, model.selection.visible)
}

// TestSelectionHighlightRepaintsTheWholeSpan pins the fix for a highlight that a
// row's own styling used to swallow: a transcript row can carry its own SGR inside
// the addressed span (inline code background, a bold run, a dimmed suffix), and a
// reset in there cancelled the reverse video, leaving most of the selected cells
// unpainted.
func TestSelectionHighlightRepaintsTheWholeSpan(t *testing.T) {
	t.Parallel()

	const row = "\x1b[38;5;252mplain \x1b[38;5;203;48;5;236mcode\x1b[m\x1b[38;5;252m tail\x1b[m"
	width := ansi.StringWidth(row)

	painted := highlightColumns(row, 0, width, selectionStyle)

	assert.Equal(t, 1, strings.Count(painted, "\x1b[7m"),
		"one highlight opens the span: %q", painted)
	assert.Equal(t, width, ansi.StringWidth(painted), "the row keeps its width")
	assert.NotContains(t, painted, "48;5;236",
		"the span's own styling is replaced, so nothing inside can cancel the highlight")

	// A partially addressed row keeps the unselected cells as they were.
	partial := highlightColumns(row, 2, 6, selectionStyle)
	assert.True(t, strings.HasPrefix(partial, "\x1b[38;5;252mpl"), "the prefix keeps its style")
	assert.Equal(t, width, ansi.StringWidth(partial))
	assert.Equal(t, 1, strings.Count(partial, "\x1b[7m"))
}
