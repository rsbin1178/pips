//nolint:wsl_v5 // Projection, render and streaming assertions stay adjacent per case.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// thinkingState builds a conversation whose only assistant turn carries visible
// reasoning, with a distinctive tail line.
func thinkingState(tail string) coding.State {
	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("question"),
		ai.Assistant(
			ai.Text("visible answer"),
			ai.ReasoningPart{Text: "step one\n\nstep two\n\nstep three\n\nstep four\n\n" + tail},
		),
	}

	return state
}

func TestProjectTimelineOrdersThinkingBeforeTheAnswer(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("question"),
		ai.Assistant(
			ai.ReasoningPart{Text: "first thought"},
			ai.Text("the answer"),
			ai.ReasoningPart{Text: "second thought"},
		),
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 4)

	assert.Equal(t, blockUser, blocks[0].kind)
	assert.Equal(t, blockThinking, blocks[1].kind)
	assert.Equal(t, thinkingBlockID(2, 0), blocks[1].id)
	assert.Equal(t, "first thought", blocks[1].body)
	assert.Equal(t, blockThinking, blocks[2].kind)
	assert.Equal(t, thinkingBlockID(2, 1), blocks[2].id)
	assert.Equal(t, "second thought", blocks[2].body)
	assert.Equal(t, blockAssistant, blocks[3].kind)
	assert.Equal(t, "the answer", blocks[3].body)
}

func TestProjectTimelineSkipsRedactedAndEmptyReasoning(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.Assistant(
		ai.ReasoningPart{Redacted: true, Signature: "sig"},
		ai.ReasoningPart{Text: "   ", Signature: "sig"},
		ai.Text("answer"),
	)}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockAssistant, blocks[0].kind)
}

// TestThinkingBlockRendersTheWholeBody pins the reader-facing contract: a
// Thinking block shows every reasoning row, with no fold, glyph or truncation
// marker, so nothing the model wrote is hidden.
func TestThinkingBlockRendersTheWholeBody(t *testing.T) {
	t.Parallel()

	const tail = "the very last thought line"
	body := "step one\n\nstep two\n\nstep three\n\nstep four\n\n" + tail
	block := timelineBlock{kind: blockThinking, id: thinkingBlockID(2, 0), body: body}

	got := renderThinkingBlock(block, 60, themeDark, true)

	assert.Equal(t, thinkingTitle+"\n"+body, got)
	assert.Contains(t, got, tail)
	assert.NotContains(t, got, "… +")
	assert.NotContains(t, got, "ctrl+e")
}

// TestThinkingBlockUsesTheThemeMutedColor asserts the block follows the active
// theme and stays visually quiet rather than borrowing the answer's color.
func TestThinkingBlockUsesTheThemeMutedColor(t *testing.T) {
	t.Parallel()

	const body = "a private thought"
	block := timelineBlock{kind: blockThinking, id: draftThinkingID, body: body}

	for _, theme := range []colorTheme{themeDark, themeLight} {
		got := renderThinkingBlock(block, 60, theme, false)
		muted := lipgloss.NewStyle().Foreground(paletteFor(theme).muted)

		assert.Contains(t, got, muted.Render(body), "the body carries the theme's muted color")
		assert.Contains(t, got, lipgloss.NewStyle().Bold(true).Foreground(paletteFor(theme).muted).
			Render(thinkingTitle), "the label carries the theme's muted color")
	}
}

func TestThinkingBlockWrapsToTheRenderWidth(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(strings.Repeat("reasoning word ", 20))
	got := renderThinkingBlock(timelineBlock{kind: blockThinking, id: draftThinkingID, body: body}, 20, themeDark, true)

	rows := strings.Split(got, "\n")
	require.Greater(t, len(rows), 2, "the long reasoning wraps onto several rows")
	for _, row := range rows {
		assert.LessOrEqual(t, ansi.StringWidth(row), 20, "no row runs past the frame")
	}
}

func TestThinkingBlockDropsEmptyAndUnsafeText(t *testing.T) {
	t.Parallel()

	block := timelineBlock{kind: blockThinking, id: draftThinkingID, body: "   \n  "}
	assert.Empty(t, renderThinkingBlock(block, 60, themeDark, true))

	block.body = "before\x1b[31mafter"
	got := renderThinkingBlock(block, 60, themeDark, false)
	assert.NotContains(t, got, "\x1b[31mafter", "model text cannot inject its own styling")
	assert.Contains(t, ansi.Strip(got), "beforeafter")
}

func TestShowThinkingBlocksOffHidesTheText(t *testing.T) {
	t.Parallel()

	const tail = "the very last thought line"
	controller := stubController{
		state: thinkingState(tail),
		tui: config.TUIConfig{
			Screen:             config.ScreenFullscreen,
			ShowThinkingBlocks: false,
		},
	}
	model := fullscreenModel(t, controller, true)
	frame := ansi.Strip(model.View().Content)

	assert.NotContains(t, frame, thinkingTitle)
	assert.NotContains(t, frame, tail)
	assert.Contains(t, frame, "visible answer")
}

func TestInlineModeNeverProjectsThinkingBlocks(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, stubController{state: thinkingState("tail")}, true)
	model.rerenderTranscript(true)

	for _, block := range model.transcriptBlocks() {
		assert.NotEqual(t, blockThinking, block.kind)
	}
	assert.NotContains(t, ansi.Strip(model.View().Content), "the very last thought line")
}

func TestConversationExportExcludesThinkingText(t *testing.T) {
	t.Parallel()

	const (
		tail      = "the very last thought line"
		signature = "opaque-signature"
	)
	state := thinkingState(tail)
	state.Transcript[1] = ai.Assistant(
		ai.Text("visible answer"),
		ai.ReasoningPart{Text: "reasoning body " + tail, Signature: signature},
	)

	model := fullscreenModel(t, stubController{state: state}, true)
	model.rerenderTranscript(true)

	document := conversationMarkdown(model.conversationBlocks())
	assert.Contains(t, document, "visible answer")
	assert.NotContains(t, document, "reasoning body")
	assert.NotContains(t, document, signature)
}

// TestThinkingFrameShowsEveryReasoningRow replaces the old preview bound: the
// fullscreen frame now carries the whole thought, however long it is.
func TestThinkingFrameShowsEveryReasoningRow(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: thinkingState("the very last thought line")}, true)
	frame := ansi.Strip(model.View().Content)

	for _, row := range []string{"step one", "step two", "step three", "step four"} {
		assert.Contains(t, frame, row)
	}
	assert.Contains(t, frame, "the very last thought line")
}

// TestThinkingDraftStreamsUnfolded asserts the live turn shows its growing
// reasoning as it arrives, in the same block, without any fold state.
func TestThinkingDraftStreamsUnfolded(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Draft = []coding.MessageDelta{
		{Kind: ai.StreamReasoningDelta, Text: "first\n\nsecond\n\nthird\n"},
	}
	model.renderTranscript(true)

	assert.Contains(t, ansi.Strip(model.View().Content), "third")

	// The turn keeps streaming, and the new text lands in the same block.
	model.state.Draft = append(model.state.Draft,
		coding.MessageDelta{Kind: ai.StreamReasoningDelta, Text: "fourth\n"})
	model.renderTranscript(false)

	frame := ansi.Strip(model.View().Content)
	assert.Contains(t, frame, "fourth")

	labels := 0
	for row := range strings.SplitSeq(frame, "\n") {
		if strings.TrimSpace(row) == thinkingTitle {
			labels++
		}
	}
	assert.Equal(t, 1, labels, "the draft stays one block")
}
