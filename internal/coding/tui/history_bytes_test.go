package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHistoryWindowTrimsByBytes pins the second bound on the browsable window:
// a page of large messages is trimmed by content even though the message count
// is far below its cap.
func TestHistoryWindowTrimsByBytes(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: cappedState()}, true)
	model.history = historyState{more: true}

	page := coding.HistoryPage{Start: 0, More: true}
	for range 40 {
		page.Messages = append(page.Messages, ai.UserText(strings.Repeat("h", 1<<20)))
	}

	model.applyHistoryPage(historyPageMsg{sequence: model.history.sequence, page: page})

	assert.LessOrEqual(t, model.history.bytes, maxHistoryBytes)
	assert.Less(t, len(model.history.messages), 40, "the byte budget trims the window")
	require.NotEmpty(t, model.history.messages)
	assert.Equal(t, 40, len(model.history.messages)+model.history.start,
		"the window keeps the newest messages and names the dropped boundary")
}

// TestHistoryWindowKeepsOneOversizedMessage pins the pathological case: a
// single message larger than the window is still browsable.
func TestHistoryWindowKeepsOneOversizedMessage(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: cappedState()}, true)
	model.history = historyState{more: true}

	page := coding.HistoryPage{Start: 0, More: true, Messages: []ai.Message{
		ai.UserText(strings.Repeat("h", maxHistoryBytes+1)),
	}}
	model.applyHistoryPage(historyPageMsg{sequence: model.history.sequence, page: page})

	require.Len(t, model.history.messages, 1)
	assert.Equal(t, 0, model.history.start)
}

// historyFixtureController serves pages out of one durable message list, so a
// re-read at the position the window still names returns exactly the messages the
// byte trim dropped.
type historyFixtureController struct {
	stubController

	messages []ai.Message
	requests []coding.HistoryRequest
}

func (c *historyFixtureController) History(
	_ context.Context,
	request coding.HistoryRequest,
) (coding.HistoryPage, error) {
	c.requests = append(c.requests, request)

	limit := request.Limit
	if limit <= 0 || limit > coding.HistoryPageMaxLimit {
		limit = coding.HistoryPageDefaultLimit
	}

	end := len(c.messages)
	if request.Before >= 0 {
		end = min(request.Before, len(c.messages))
	}

	start := max(0, end-limit)

	return coding.HistoryPage{
		Messages: slices.Clone(c.messages[start:end]),
		Start:    start, Total: len(c.messages), More: start > 0,
	}, nil
}

// TestHistoryByteTrimmedPageIsReadAgainFromItsStart pins the recovery contract of
// the byte bound: a page the byte trim dropped is not lost, because the window
// keeps naming the boundary it dropped and the next read continues there.
func TestHistoryByteTrimmedPageIsReadAgainFromItsStart(t *testing.T) {
	t.Parallel()

	const (
		messageBytes = 2 << 20
		durable      = 17
	)

	messages := make([]ai.Message, 0, durable)
	for range durable {
		messages = append(messages, ai.User(ai.ImagePart{
			Source: ai.MediaSource{Data: make([]byte, messageBytes), MIMEType: "image/png"},
		}))
	}

	controller := &historyFixtureController{
		state:    cappedState(),
		messages: messages,
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.rerenderTranscript(true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)
	page, ok := command().(historyPageMsg)
	require.True(t, ok)
	model.Update(page)

	require.Len(t, controller.requests, 1)
	assert.Equal(t, -1, controller.requests[0].Before, "the first read asks for the window boundary")

	dropped := durable - len(model.history.messages)
	require.Positive(t, dropped, "the byte budget trimmed the loaded page")
	assert.LessOrEqual(t, model.history.bytes, maxHistoryBytes, "the window stays inside its byte budget")
	assert.Equal(t, dropped, model.history.start, "the window names the boundary it dropped")
	assert.True(t, model.history.more, "the dropped content can still be read")

	boundary := model.history.start
	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.NotNil(t, command)
	page, ok = command().(historyPageMsg)
	require.True(t, ok)

	require.Len(t, controller.requests, 2)
	assert.Equal(t, boundary, controller.requests[1].Before,
		"the next read continues from the position the window still names")
	assert.Equal(t, boundary, page.page.Start+len(page.page.Messages),
		"the page ends exactly at the boundary: no message is skipped or repeated")
	assert.Len(t, page.page.Messages, dropped, "the dropped page comes back whole")
	assert.False(t, page.page.More)
}
