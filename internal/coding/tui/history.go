//nolint:wsl_v5 // The load transaction and its bounds stay adjacent.
package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

// historyPageLimit is the page size one load asks for. The Runtime clamps it, and
// one page renders at most that many records, so the value trades round trips
// against a burst of re-rendering.
const historyPageLimit = coding.HistoryPageDefaultLimit

// maxHistoryMessages bounds the browsable window. Older pages that fall off the
// front can be read again, because the Runtime's history is a pure read and the
// remaining window still names the position to continue from. A page is 100
// messages, so the window holds four pages before the oldest is dropped.
const maxHistoryMessages = 4 * historyPageLimit

// maxHistoryBytes is the second bound on the same window. A page of a few very
// large messages must not pin the byte budget the message count would allow.
const maxHistoryBytes = 32 << 20

// historyState is the browsable conversation older than the bootstrap window. The
// managed viewport owns the whole conversation, so a session whose durable
// history is longer than the transcript cap would otherwise be unreachable.
type historyState struct {
	messages []ai.Message // oldest first
	start    int          // absolute index of messages[0]
	bytes    int          // payload bytes retained by messages
	more     bool         // older durable messages may exist
	loading  bool
	sequence uint64
	err      error
}

// historyPageMsg delivers one history read.
type historyPageMsg struct {
	sequence uint64
	page     coding.HistoryPage
	err      error
}

// resetHistory drops browsable history and re-derives whether more may exist. The
// bootstrap window is the newest transcript-cap messages, so a window at the cap
// is the only signal a fresh frontend has that older messages exist.
func (m *Model) resetHistory() {
	m.history = historyState{more: coding.TranscriptWindowMayBeTruncated(m.state)}
}

// historyOffered reports whether a load is worth offering to the user.
func (m *Model) historyOffered() bool {
	return m.history.more && m.history.err == nil
}

// requestOlderHistory asks the Runtime for the page before the loaded window. The
// read is bounded, read-only and safe to drop: a session change or a newer page
// invalidates it by sequence.
func (m *Model) requestOlderHistory() tea.Cmd {
	if m.history.loading || !m.historyOffered() || !m.fullscreen() || m.controller == nil {
		return nil
	}

	m.history.loading = true
	m.history.sequence++
	sequence := m.history.sequence
	before := -1
	if m.history.start > 0 {
		before = m.history.start
	}

	ctx := m.ctx
	controller := m.controller

	return func() tea.Msg {
		page, err := controller.History(ctx, coding.HistoryRequest{
			Before: before,
			Limit:  historyPageLimit,
		})

		return historyPageMsg{sequence: sequence, page: page, err: err}
	}
}

// applyHistoryPage prepends one page and keeps the reader on the same record. Row
// identity, not a row offset, is what makes that possible: prepending shifts every
// row below it.
func (m *Model) applyHistoryPage(message historyPageMsg) {
	if message.sequence != m.history.sequence {
		return
	}

	m.history.loading = false
	if message.err != nil {
		m.history.err = message.err
		m.history.more = false
		m.rerenderTranscript(false)

		return
	}

	page := message.page
	m.history.err = nil
	m.history.more = page.More
	m.history.start = page.Start
	if len(page.Messages) == 0 {
		m.rerenderTranscript(false)

		return
	}

	wasFollowing := m.transcriptScroll.follow
	anchor, resolved := m.transcript.captureAnchor(m.transcriptScroll.offset)
	m.history.messages = append(slices.Clone(page.Messages), m.history.messages...)
	for _, message := range page.Messages {
		m.history.bytes += historyMessageBytes(message)
	}
	// The dropped page can be read again from the Runtime, so the window keeps
	// its bound instead of growing with every load. The newest message stays even
	// when it alone exceeds the byte budget: it is the window the reader reached.
	for len(m.history.messages) > 1 &&
		(len(m.history.messages) > maxHistoryMessages || m.history.bytes > maxHistoryBytes) {
		m.history.bytes -= historyMessageBytes(m.history.messages[0])
		m.history.messages = m.history.messages[1:]
		m.history.start++
		m.history.more = true
	}
	m.rerenderTranscript(false)

	switch {
	case wasFollowing:
		m.transcriptScroll.gotoBottom()
	case resolved >= 0:
		if row := m.transcript.resolveAnchor(anchor); row >= 0 {
			m.transcriptScroll.scrollTo(row)
		}
	}
}

// historyMessageBytes reports the payload one browsable message retains. It
// mirrors the reducer's accounting so the window bound is about content, not
// slice headers.
func historyMessageBytes(message ai.Message) int {
	return historyPartsBytes(portableMessageParts(message))
}

func historyPartsBytes(parts []ai.Part) int {
	total := 0
	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			total += len(value.Text)
		case ai.ReasoningPart:
			total += len(value.Text) + len(value.Signature)
		case ai.ImagePart:
			total += len(value.Source.Data)
		case ai.FilePart:
			total += len(value.Source.Data) + len(value.Name)
		case ai.ToolCallPart:
			total += len(value.Args) + len(value.Name)
		case ai.ToolResultPart:
			total += len(value.Name) + len(value.ToolCallID) + historyPartsBytes(value.Content)
		case ai.StructuredContentPart:
			total += len(value.Data)
		case ai.ResourceLinkPart:
			total += len(value.URI) + len(value.Name) + len(value.Title) + len(value.Description)
		case ai.EmbeddedResourcePart:
			total += len(value.Text) + len(value.Blob) + len(value.URI)
		}
	}

	return total
}

// historyBlocks projects the browsable history ahead of the loaded window.
//
// Rows carry an explicit identity because their positions are absolute indexes
// into the durable conversation, and the loaded window numbers its own rows from
// one. Without the prefix a history record and an unrelated loaded record could
// share a cache key.
func (m *Model) historyBlocks() []timelineBlock {
	if len(m.history.messages) == 0 {
		return nil
	}

	blocks := projectHistoryBlocks(m.history.messages)
	for index := range blocks {
		block := &blocks[index]
		// One message can now produce more than one block (Thinking + answer
		// text), so the absolute position alone is no longer unique. The block's
		// local id distinguishes them, and the absolute prefix stays invariant
		// when an older page is prepended.
		prefix := "history:" + itoa(m.history.start+block.position)
		if block.id != "" {
			block.id = prefix + ":" + block.id
		} else {
			block.id = prefix + ":" + kindName(block.kind)
		}
	}

	return blocks
}

// projectHistoryBlocks renders durable messages older than the loaded window. It
// mirrors the message half of the timeline projection and feeds the same block
// renderer; tool results stand alone because the live projection pairs them with
// Tool activity that this window does not carry.
func projectHistoryBlocks(messages []ai.Message) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(messages))
	for index, message := range messages {
		position := index + 1
		if _, ok := message.(ai.AssistantMessage); ok {
			// Local ids are position-independent so the absolute prefix computed
			// by historyBlocks stays the cache key when a page is prepended.
			thinking := reasoningBlocks(message, position)
			for partIndex := range thinking {
				thinking[partIndex].id = "thinking:" + itoa(partIndex)
			}
			blocks = append(blocks, thinking...)
			if body := visibleMessageText(message); body != "" {
				blocks = append(blocks, timelineBlock{
					kind: blockAssistant, id: "text", body: body, position: position,
				})
			}

			continue
		}

		body := visibleMessageText(message)
		if body == "" {
			continue
		}

		switch message.(type) {
		case ai.UserMessage:
			blocks = append(blocks, timelineBlock{kind: blockUser, body: body, position: position})
		case ai.SystemMessage:
			blocks = append(blocks, timelineBlock{
				kind: blockDiagnostic, title: "System", body: body, position: position,
			})
		case ai.ToolMessage:
			blocks = append(blocks, timelineBlock{
				kind: blockDiagnostic, title: "Tool result", body: body, position: position,
			})
		}
	}

	return blocks
}
