//nolint:wsl_v5 // Pointer fixtures and their frame assertions stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolHitState is a conversation with two addressable Tool calls followed by more
// output, so a hit test can prove it addressed the entry under the pointer rather
// than the newest one.
func toolHitState() coding.State {
	state := readyState()
	// The two calls sit at different transcript positions, so they project as two
	// separate entries rather than one parallel-call group.
	state.Transcript = []ai.Message{ai.UserText("inspect")}
	for index := range 6 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("LIVE-%02d", index)))
	}
	// The calls are the newest entries, so they sit inside the window while the
	// viewport follows the tail. They are different Tool classes on purpose: two
	// consecutive explore calls would render as one grouped card and could not be
	// addressed separately.
	state.Transcript = append(state.Transcript,
		ai.Assistant(ai.ToolCallPart{ID: "call-a", Name: toolNameRead, Args: ai.JSON(`{"path":"a.go"}`)}),
		ai.ToolResultText("call-a", toolNameRead, "package a"),
		ai.Assistant(ai.ToolCallPart{ID: "call-b", Name: toolNameShell, Args: ai.JSON(`{"command":"echo b"}`)}),
		ai.ToolResultText("call-b", toolNameShell, "b"),
	)

	return state
}

// toolFrameRow converts a Tool record's store row into the frame row a click would
// have to use, or -1 when the record is not in the store.
func toolFrameRow(model *Model, callID string) int {
	_ = model.transcript.flatten()
	for index, record := range model.transcript.records {
		if record.tool == nil {
			continue
		}
		for _, activity := range record.tool.tools {
			if activity.id != callID {
				continue
			}

			return model.transcript.starts[index] - model.transcriptScroll.offset - model.frameHit.topDropped
		}
	}

	return -1
}

// frameRowOfTool is the test-shaped wrapper.
func frameRowOfTool(t *testing.T, model *Model, callID string) int {
	t.Helper()

	row := toolFrameRow(model, callID)
	if row < 0 {
		t.Fatalf("no visible transcript record for %s", callID)
	}

	return row
}

// mousePressAt and mouseReleaseAt are the two halves of one click. The press arms
// the selection; the release either activates the entry under the pointer or
// copies the text a drag selected.
func mousePressAt(row int) tea.MouseMsg {
	return tea.MouseClickMsg{X: 3, Y: row, Button: tea.MouseLeft}
}

func mouseReleaseAt(row int) tea.MouseMsg {
	return tea.MouseReleaseMsg{X: 3, Y: row, Button: tea.MouseLeft}
}

// clickAt delivers one complete click — a press and a release on the same cell —
// and returns the command the release produced.
func clickAt(model *Model, row int) tea.Cmd {
	model.handleMouse(mousePressAt(row))

	return model.handleMouse(mouseReleaseAt(row))
}

// TestFrameHitMapMatchesThePaintedFrame keeps the map honest: the transcript band
// starts at frame row zero, every row inside it resolves to the row that was
// painted there, and a control row resolves to nothing.
func TestFrameHitMapMatchesThePaintedFrame(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: toolHitState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	model.rerenderTranscript(true)
	view := model.View()

	require.True(t, model.frameHit.painted)
	require.Positive(t, model.transcriptScroll.height)
	assert.Equal(t, model.transcriptScroll.height, model.frameHit.transcript,
		"the band fills its allocation when the store is longer")

	// The band is the top of the frame, so the first painted row is the window's
	// first row. The turn indicator is the one thing the frame draws over that row,
	// so its span is checked separately and the rest of the row must match.
	frameRows := strings.Split(view.Content, "\n")
	windowRows := strings.Split(model.transcriptScroll.visible(), "\n")
	require.NotEmpty(t, windowRows)

	withoutIndicator := func(row string) string { return row }
	if model.frameHit.turnIndicator {
		_, left, right, ok := model.turnIndicator()
		require.True(t, ok)
		assert.Equal(t, turnIndicatorIcon, ansi.Strip(ansi.Cut(frameRows[0], left, right)),
			"the frame draws the indicator in the transcript's first row")
		withoutIndicator = func(row string) string {
			return ansi.Truncate(row, left, "") + ansi.TruncateLeft(row, right, "")
		}
	}

	assert.Equal(t,
		strings.TrimRight(withoutIndicator(windowRows[0]), " "),
		strings.TrimRight(withoutIndicator(frameRows[0]), " "),
		"the band is painted at the top of the frame")

	first, ok := model.entryRowAt(0)
	require.True(t, ok)
	assert.Equal(t, model.transcriptScroll.offset, first)
	last, ok := model.entryRowAt(model.frameHit.transcript - 1)
	require.True(t, ok)
	assert.Equal(t, model.transcriptScroll.offset+model.frameHit.transcript-1, last)

	_, ok = model.entryRowAt(model.frameHit.transcript)
	assert.False(t, ok, "a control row addresses no entry")
	_, ok = model.entryRowAt(-1)
	assert.False(t, ok)
}

// TestClickOpensTheAddressedToolEntry is the phase-2 R7 contract for tools: the
// pointer addresses the entry it points at, not the newest one that Ctrl+T opens.
func TestClickOpensTheAddressedToolEntry(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: toolHitState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	model.rerenderTranscript(true)
	model.View()

	row := frameRowOfTool(t, model, "call-a")
	require.GreaterOrEqual(t, row, 0)
	require.Less(t, row, model.frameHit.transcript, "the addressed entry is on screen")

	clickAt(model, row)
	require.Equal(t, routeToolDetail, model.route.kind, "the click opened a detail route")
	require.NotNil(t, model.route.toolDetail)
	assert.Contains(t, model.route.toolDetail.callIDs, "call-a")
	assert.NotContains(t, model.route.toolDetail.callIDs, "call-b")
}

// TestClickCannotAddressARecordThatMoved is the staleness guard: after the window
// has moved, an old coordinate resolves against the frame now on screen.
func TestClickCannotAddressARecordThatMoved(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: toolHitState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	model.rerenderTranscript(true)
	model.View()

	row := frameRowOfTool(t, model, "call-a")
	require.GreaterOrEqual(t, row, 0)

	// The conversation grows past the window, so that row now shows newer output.
	for index := range 20 {
		model.state.Transcript = append(model.state.Transcript, ai.UserText(fmt.Sprintf("NEW-%02d", index)))
	}
	model.rerenderTranscript(true)
	model.View()

	command := clickAt(model, row)
	assert.Equal(t, routeNone, model.route.kind, "the pointer no longer points at a tool entry")
	assert.Nil(t, command)
}

// TestClickIsIgnoredWhileAnotherSurfaceOwnsInput pins the ownership rule: an entry
// click is dropped, never forwarded, while a route, prompt, picker or the escape
// hatch owns the screen.
func TestClickIsIgnoredWhileAnotherSurfaceOwnsInput(t *testing.T) {
	t.Parallel()

	open := func(t *testing.T) (*Model, int) {
		t.Helper()

		model := fullscreenModel(t, stubController{state: toolHitState()}, true)
		model.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
		model.rerenderTranscript(true)
		model.View()
		row := frameRowOfTool(t, model, "call-a")
		require.GreaterOrEqual(t, row, 0)

		return model, row
	}

	t.Run("route", func(t *testing.T) {
		t.Parallel()
		model, row := open(t)
		model.route = newSessionPickerState("", model.theme, true)
		model.route.controlling = true
		model.route.loading = true
		model.setLayout()

		assert.Nil(t, clickAt(model, row))
		assert.Equal(t, routeSessions, model.route.kind)
	})

	t.Run("prompt", func(t *testing.T) {
		t.Parallel()
		model, row := open(t)
		model.prompt.kind = promptApproval

		assert.Nil(t, clickAt(model, row))
		assert.Equal(t, routeNone, model.route.kind)
	})

	t.Run("picker", func(t *testing.T) {
		t.Parallel()
		model, row := open(t)
		model.picker.kind = pickerCommand

		assert.Nil(t, clickAt(model, row))
		assert.Equal(t, routeNone, model.route.kind)
	})

	t.Run("transcript mode", func(t *testing.T) {
		t.Parallel()
		model, row := open(t)
		_, _ = model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)
		model.View()

		assert.Nil(t, clickAt(model, row))
		assert.Equal(t, routeNone, model.route.kind)
	})
}

// TestClickIsIgnoredWithoutMouseReporting pins the inline compatibility mode and
// the pre-paint window: neither has a frame the pointer may address.
func TestClickIsIgnoredWithoutMouseReporting(t *testing.T) {
	t.Parallel()

	inline := readyModel(t, true)
	inline.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	inline.rerenderTranscript(true)
	inline.View()
	assert.Equal(t, tea.MouseModeNone, inline.View().MouseMode)
	assert.Nil(t, clickAt(inline, 0))
	assert.Equal(t, routeNone, inline.route.kind)

	// A click that arrives before the first paint resolves against nothing.
	unpainted := fullscreenModel(t, stubController{state: toolHitState()}, true)
	assert.False(t, unpainted.frameHit.painted)
	assert.Nil(t, clickAt(unpainted, 0))
}

// clickAttemptMsg asks the probe for another click attempt.
type clickAttemptMsg struct{}

// clickProbe clicks the addressed Tool entry until the detail route opens. The
// renderer only dispatches pointer events once a frame has been flushed, so the
// probe retries on a bounded tick instead of assuming a paint order; the
// assertions still gate the result.
type clickProbe struct {
	*Model

	done  atomic.Bool
	tries atomic.Int64
}

func (p *clickProbe) Init() tea.Cmd {
	return tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg { return clickAttemptMsg{} })
}

func (p *clickProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := p.Model.Update(message)
	if p.route.kind == routeToolDetail {
		p.done.Store(true)

		return p, command
	}
	if _, ok := message.(clickAttemptMsg); ok {
		if p.tries.Add(1) > 150 {
			return p, nil
		}

		row := max(0, toolFrameRow(p.Model, "call-a"))

		// The press and the release are ordered, so the drag that decides between
		// activating an entry and copying text sees both in sequence.
		return p, tea.Sequence(
			tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg { return clickAttemptMsg{} }),
			func() tea.Msg { return mousePressAt(row) },
			func() tea.Msg { return mouseReleaseAt(row) },
		)
	}

	return p, command
}

func (p *clickProbe) finished() bool { return p.done.Load() }

// TestClickThroughThePaintedViewOpensTheToolDetail is the end-to-end wiring check:
// the click travels through the Program's mouse dispatch and the frame that is
// painted afterwards is the addressed Tool's detail.
func TestClickThroughThePaintedViewOpensTheToolDetail(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: toolHitState()}, true)
	probe := &clickProbe{Model: model}
	var output synchronizedBuffer
	program := tea.NewProgram(probe, tea.WithInput(nil), tea.WithOutput(&output),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		tea.WithWindowSize(60, 14), tea.WithFPS(60), tea.WithoutSignalHandler())
	finished := make(chan error, 1)
	go func() { _, err := program.Run(); finished <- err }()
	t.Cleanup(program.Kill)
	// The click resolves through the painted view, so the assertion waits for the
	// frame that follows it rather than for the model state alone.
	require.Eventually(t, func() bool {
		return probe.finished() && strings.Contains(ansi.Strip(output.String()), "package a")
	}, 30*time.Second, 10*time.Millisecond)

	capture := output.String()
	program.Quit()
	require.NoError(t, <-finished)

	rendered := ansi.Strip(capture)
	assert.Contains(t, rendered, "Exploration details", "the Tool detail route is on screen")
	assert.Contains(t, rendered, "package a", "showing the addressed call's result")
}

// TestClickOnAGroupedEntryOpensTheGroup documents the deliberate limit: consecutive
// explore calls render as one grouped card, so the pointer addresses that card and
// its detail covers the whole group. A click cannot split a card.
func TestClickOnAGroupedEntryOpensTheGroup(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "read-a", Name: toolNameRead, Args: ai.JSON(`{"path":"a.go"}`)}),
		ai.ToolResultText("read-a", toolNameRead, "package a"),
		ai.Assistant(ai.ToolCallPart{ID: "read-b", Name: toolNameRead, Args: ai.JSON(`{"path":"b.go"}`)}),
		ai.ToolResultText("read-b", toolNameRead, "package b"),
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
	model.rerenderTranscript(true)
	model.View()

	row := frameRowOfTool(t, model, "read-b") // the group keeps the newest call's id
	require.GreaterOrEqual(t, row, 0)

	clickAt(model, row)
	require.Equal(t, routeToolDetail, model.route.kind)
	require.NotNil(t, model.route.toolDetail)
	assert.Contains(t, model.route.toolDetail.callIDs, "read-a")
	assert.Contains(t, model.route.toolDetail.callIDs, "read-b")
}
