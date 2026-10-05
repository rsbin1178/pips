//nolint:wsl_v5 // Paging transactions and their screen assertions stay adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagingController records the history requests the viewport makes and serves one
// canned page per request.
type pagingController struct {
	stubController

	requests []coding.HistoryRequest
	pages    []coding.HistoryPage
	err      error
}

func (c *pagingController) History(
	_ context.Context,
	request coding.HistoryRequest,
) (coding.HistoryPage, error) {
	c.requests = append(c.requests, request)
	if c.err != nil {
		return coding.HistoryPage{}, c.err
	}
	if len(c.pages) == 0 {
		return coding.HistoryPage{}, nil
	}

	page := c.pages[0]
	c.pages = c.pages[1:]

	return page, nil
}

// cappedState is a transcript at the bootstrap cap, which is the only signal a
// fresh frontend has that older durable messages exist.
func cappedState() coding.State {
	state := readyState()
	state.Transcript = make([]ai.Message, 4096)
	for index := range state.Transcript {
		state.Transcript[index] = ai.UserText("LIVE")
	}

	return state
}

// TestHistoryPagesPrependOlderConversation is the AC8 path in the viewport: a
// window at the transcript cap can load the durable messages older than it, in
// order, one bounded page per upward scroll.
func TestHistoryPagesPrependOlderConversation(t *testing.T) {
	t.Parallel()

	controller := &pagingController{
		stubController: stubController{state: cappedState()},
		pages: []coding.HistoryPage{{
			Messages: []ai.Message{ai.UserText("OLD-00"), ai.UserText("OLD-01"), ai.UserText("OLD-02")},
			Start:    120, Total: 4216, More: true,
		}},
	}
	model := fullscreenModel(t, controller, true)
	require.True(t, model.historyOffered(), "a window at the cap may have older history")
	assert.Empty(t, model.history.messages)

	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.rerenderTranscript(true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command, "reaching the top asks for the page above it")
	assert.True(t, model.history.loading)

	message, ok := command().(historyPageMsg)
	require.True(t, ok)
	model.Update(message)

	require.Len(t, controller.requests, 1)
	assert.Equal(t, -1, controller.requests[0].Before, "the first read asks for the window boundary")
	assert.Equal(t, historyPageLimit, controller.requests[0].Limit)
	assert.False(t, model.history.loading)
	assert.Equal(t, 120, model.history.start)
	assert.True(t, model.history.more)
	assert.NotContains(t, ansi.Strip(model.View().Content), "OLD-00",
		"a load does not move the reading window")

	// Scrolling above the window reaches the older conversation.
	model.transcriptScroll.gotoTop()
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "OLD-00")
	assert.Contains(t, content, "OLD-02")

	// The next upward scroll continues from the page boundary.
	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)
	message, ok = command().(historyPageMsg)
	require.True(t, ok)
	model.Update(message)
	require.Len(t, controller.requests, 2)
	assert.Equal(t, 120, controller.requests[1].Before, "the next read continues at the page start")
}

// TestHistoryWindowIsBounded asserts the browsable window keeps a fixed number of
// messages no matter how many pages are loaded, and that the page it drops can be
// read again from the position the window still names.
func TestHistoryWindowIsBounded(t *testing.T) {
	t.Parallel()

	// The durable conversation below the window runs from 0 to this boundary.
	const boundary = 1000

	boundaryPage := coding.HistoryPage{
		Messages: []ai.Message{ai.UserText("DROPPED")},
		Start:    boundary - maxHistoryMessages - historyPageLimit,
		Total:    boundary,
		More:     true,
	}
	controller := &pagingController{
		stubController: stubController{state: cappedState()},
		pages:          []coding.HistoryPage{boundaryPage},
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.rerenderTranscript(true)

	// Pages arrive newest first. Once the window is full, the next read asks again
	// at the boundary it names, which is the page it just dropped.
	pages := maxHistoryMessages/historyPageLimit + 2

	for page := range pages {
		messages := make([]ai.Message, historyPageLimit)
		for index := range messages {
			messages[index] = ai.UserText(fmt.Sprintf("OLD-%03d", index))
		}

		start := max(boundary-maxHistoryMessages-historyPageLimit, boundary-(page+1)*historyPageLimit)

		model.applyHistoryPage(historyPageMsg{
			sequence: model.history.sequence,
			page: coding.HistoryPage{
				Messages: messages,
				Start:    start,
				Total:    boundary,
				More:     start > 0,
			},
		})
	}

	require.Len(t, model.history.messages, maxHistoryMessages,
		"the window keeps its bound instead of growing with every page")
	assert.True(t, model.history.more, "the dropped page is still reachable")
	assert.Equal(t, boundary-maxHistoryMessages, model.history.start,
		"the window still names where the dropped page begins")

	// The dropped page is read again from that boundary.
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)

	message, ok := command().(historyPageMsg)
	require.True(t, ok)
	model.Update(message)

	require.NotEmpty(t, controller.requests)
	assert.Equal(t, boundary-maxHistoryMessages, controller.requests[0].Before,
		"the next read continues at the window's oldest message")
	assert.Len(t, model.history.messages, maxHistoryMessages,
		"reading the boundary back does not grow the window")
}

// TestHistoryLoadKeepsTheReaderOnTheSameRecord asserts that prepending rows does
// not move the reading window: the anchor is record identity, not a row offset.
func TestHistoryLoadKeepsTheReaderOnTheSameRecord(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 40 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("LIVE-%03d", index)))
	}
	controller := &pagingController{
		stubController: stubController{state: state},
		pages: []coding.HistoryPage{{
			Messages: []ai.Message{ai.UserText("OLD-00"), ai.UserText("OLD-01")},
			Start:    900, Total: 942, More: true,
		}},
	}
	model := fullscreenModel(t, controller, true)
	model.history = historyState{more: true}
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.rerenderTranscript(true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	require.False(t, model.transcriptScroll.follow)

	// Reaching the top asks for the page above the window.
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)
	_ = model.View() // readyView owns the window height
	window := model.transcriptScroll.visible()
	require.NotEmpty(t, window)
	anchor := model.transcript.recordIDAt(model.transcriptScroll.offset)
	require.NotEmpty(t, anchor)

	message, ok := command().(historyPageMsg)
	require.True(t, ok)
	model.Update(message)

	assert.Equal(t, anchor, model.transcript.recordIDAt(model.transcriptScroll.offset),
		"the reader stays on the same record")
	assert.Equal(t, window, model.transcriptScroll.visible(),
		"prepending history does not move the window")

	// Scrolling above the window reaches the older conversation.
	model.transcriptScroll.gotoTop()
	older := ansi.Strip(model.transcriptScroll.visible())
	assert.Contains(t, older, "OLD-00")
	assert.Contains(t, older, "OLD-01")
}

// TestHistoryIsOfferedOnlyForAFullWindow pins the trigger conditions: a short
// conversation has nothing older, and the inline mode never pages at all.
func TestHistoryIsOfferedOnlyForAFullWindow(t *testing.T) {
	t.Parallel()

	short := fullscreenModel(t, stubController{state: readyState()}, true)
	short.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	short.rerenderTranscript(true)
	assert.False(t, short.historyOffered())
	_, command := short.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Nil(t, command, "a short conversation has no older page")

	inline := readyModel(t, true)
	inline.history = historyState{more: true}
	inline.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	_, command = inline.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Nil(t, command, "the inline mode leaves history with the terminal")

	capped := fullscreenModel(t, stubController{state: cappedState()}, true)
	assert.True(t, capped.historyOffered())
}

// TestHistoryFailureStopsOfferingAndIsVisible keeps a failed read from being
// retried forever and tells the user why the older messages are missing.
func TestHistoryFailureStopsOfferingAndIsVisible(t *testing.T) {
	t.Parallel()

	controller := &pagingController{
		stubController: stubController{state: cappedState()},
		err:            errors.New("history read failed"),
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.rerenderTranscript(true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	model.Update(historyPageMsg{sequence: model.history.sequence, err: controller.err})

	assert.False(t, model.historyOffered())
	assert.Contains(t, ansi.Strip(model.statusLine()), "unavailable")
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Nil(t, command, "a failed read is not retried on every scroll")
}

// TestHistoryIsResetByASessionChange keeps another session's messages out of the
// current conversation.
func TestHistoryIsResetByASessionChange(t *testing.T) {
	t.Parallel()

	controller := &pagingController{
		stubController: stubController{state: cappedState()},
		pages: []coding.HistoryPage{{
			Messages: []ai.Message{ai.UserText("OTHER-SESSION")},
			Start:    10, Total: 4216, More: true,
		}},
	}
	model := fullscreenModel(t, controller, true)
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)
	message, ok := command().(historyPageMsg)
	require.True(t, ok)
	model.Update(message)
	require.NotEmpty(t, model.history.messages)

	_, _ = model.Update(controlResultMsg{operation: operationNew})
	assert.Empty(t, model.history.messages)
	assert.Equal(t, 0, model.history.start)
}

// TestCompactionKeepsThePreCompactionHistoryBrowsable is the AC8 separation: the
// model context shrinks to the checkpoint's after count while the browsable
// conversation keeps every record.
func TestCompactionKeepsThePreCompactionHistoryBrowsable(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.ContextTokens = 9000
	for index := range 30 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("KEEP-%02d", index)))
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.rerenderTranscript(true)
	before := slices.Clone(model.transcript.flatten())
	require.Contains(t, strings.Join(before, "\n"), "KEEP-00")

	preview := coding.CompactionPreview{
		Available: true, Token: "plan-1", SourceLeafID: "leaf-9",
		EstimatedTokens: 9000, ThresholdTokens: 8000,
		SummarizedMessages: 20, KeptMessages: 10,
		Strategy: coding.CompactionStrategyFull,
	}
	model.reduceObservedEvent(coding.Event{
		Schema: coding.EventSchema, Sequence: 1, Time: time.Now().UTC(),
		Type: coding.EventCompactionStarted, SessionID: model.state.SessionID,
		Payload: coding.CompactionStarted{Mode: coding.CompactionManual, Preview: preview},
	})
	require.NoError(t, model.streamErr)
	model.reduceObservedEvent(coding.Event{
		Schema: coding.EventSchema, Sequence: 2, Time: time.Now().UTC(),
		Type: coding.EventCompactionCompleted, SessionID: model.state.SessionID,
		Payload: coding.CompactionCompleted{
			Mode: coding.CompactionManual, TokensBefore: 9000, TokensAfter: 1200,
			CheckpointID: "checkpoint-1",
			Strategy:     coding.CompactionStrategyFull,
		},
	})
	require.NoError(t, model.streamErr)

	model.rerenderTranscript(false)
	assert.Equal(t, before, model.transcript.flatten(),
		"a context checkpoint does not clear the browsable history")
	assert.Equal(t, 1200, model.state.ContextTokens, "the model context did shrink")
}
