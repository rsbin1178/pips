package tui

import (
	"crypto/sha256"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
)

// scrollbackCursor separates immutable conversation history from the live
// tail Bubble Tea still owns. Stable blocks are printed above the inline
// Program once, so the terminal can retain, select, and scroll them natively.
type scrollbackCursor struct {
	messages    int
	tools       int
	diagnostics int
	completions map[string]struct{}
	changes     projectionFingerprint
	lastError   projectionFingerprint
	streamError string
}

type projectionFingerprint [sha256.Size]byte

func (m *Model) resetScrollback() {
	m.scrollback = scrollbackCursor{}
	m.timeline = ""
}

func (m *Model) commitStableTimeline() tea.Cmd {
	content := m.takeStableTimeline()
	m.rerenderTranscript(false)

	return m.printScrollback(content)
}

// printScrollback is the single boundary for output that leaves Bubble Tea's
// managed inline frame. Projection resets do not reset this latch because the
// terminal's native history survives New, Resume, Fork, and model changes.
func (m *Model) printScrollback(content string) tea.Cmd {
	if content == "" {
		return nil
	}

	if m.scrollbackOutput {
		content = strings.Repeat("\n", conversationGapHeight) + content
	}

	m.scrollbackOutput = true

	// Bubble Tea's inline insertAbove implementation first reserves physical
	// rows below the managed frame. One insert must therefore fit in the
	// currently unused terminal rows; a multi-screen Println otherwise scrolls
	// blank reservation rows into native history before writing the payload.
	managedHeight := lipgloss.Height(m.readyView().Content)
	maximumRows := max(1, m.height-managedHeight)
	chunks := splitScrollbackContent(content, m.width, maximumRows)

	commands := make([]tea.Cmd, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk == "" {
			// insertAbove ignores an empty body, so retain a deliberately blank
			// physical row with a zero-width terminal reset sequence.
			chunk = "\x1b[0m"
		}

		commands = append(commands, tea.Println(chunk))
	}

	return tea.Sequence(commands...)
}

// splitScrollbackContent pre-wraps physical terminal rows and groups them so
// each Bubble Tea insertAbove call fits above the managed inline frame. The
// chunks must be executed in order; each completed insertion becomes the
// backing history for the next one instead of introducing blank scrollback.
func splitScrollbackContent(content string, width, maximumRows int) []string {
	maximumRows = max(1, maximumRows)

	lines := make([]string, 0, strings.Count(content, "\n")+1)
	for line := range strings.SplitSeq(content, "\n") {
		if width > 0 && ansi.StringWidth(line) > width {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)

			continue
		}

		lines = append(lines, line)
	}

	chunks := make([]string, 0, (len(lines)+maximumRows-1)/maximumRows)
	for start := 0; start < len(lines); start += maximumRows {
		end := min(len(lines), start+maximumRows)
		chunks = append(chunks, strings.Join(lines[start:end], "\n"))
	}

	return chunks
}

// takeStableTimeline advances the scrollback cursor and returns the immutable
// delta to print. Keeping projection separate from the Tea command makes the
// append-only contract directly testable.
func (m *Model) takeStableTimeline() string {
	m.reconcileScrollback()

	stableTools := m.scrollback.tools
	for stableTools < len(m.state.Tools) &&
		m.state.Tools[stableTools].Status == coding.ToolStatusCompleted {
		stableTools++
	}

	delta := m.state.Clone()

	delta.Transcript = delta.Transcript[m.scrollback.messages:]
	delta.Draft = nil
	delta.Tools = delta.Tools[m.scrollback.tools:stableTools]

	delta.Diagnostics = delta.Diagnostics[m.scrollback.diagnostics:]

	changes := currentChanges(m.state)
	if changes == m.scrollback.changes {
		delta.Changes = nil
	}

	lastError := currentStateError(m.state)
	if lastError == m.scrollback.lastError {
		delta.LastError = nil
	}

	blocks := projectTimeline(delta)
	blocks = m.expandTimelineBlocks(blocks, delta.Tools)

	for _, marker := range m.pendingCompletionMarkers() {
		if block, ok := projectCompletionMarker(marker); ok {
			blocks = append(blocks, block)
		}

		m.markCompletionCommitted(marker)
	}

	streamError := currentStreamError(m.streamErr)
	if streamError != "" && streamError != m.scrollback.streamError {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: streamError,
		})
	}

	m.scrollback.messages = len(m.state.Transcript)
	m.scrollback.tools = stableTools
	m.scrollback.diagnostics = len(m.state.Diagnostics)
	m.scrollback.changes = changes

	m.scrollback.lastError = lastError

	if streamError != "" {
		m.scrollback.streamError = streamError
	}

	content := renderTimelineContent(
		blocks,
		m.markdown,
		m.width,
		m.theme,
		m.options.NoColor,
	)

	return content
}

func (m *Model) activeTimelineBlocks() []timelineBlock {
	m.reconcileScrollback()

	active := m.state.Clone()

	active.Transcript = active.Transcript[m.scrollback.messages:]
	active.Tools = active.Tools[m.scrollback.tools:]

	active.Diagnostics = active.Diagnostics[m.scrollback.diagnostics:]

	if currentChanges(m.state) == m.scrollback.changes {
		active.Changes = nil
	}

	if currentStateError(m.state) == m.scrollback.lastError {
		active.LastError = nil
	}

	blocks := m.expandTimelineBlocks(projectTimeline(active), active.Tools)
	for _, marker := range m.pendingCompletionMarkers() {
		if block, ok := projectCompletionMarker(marker); ok {
			blocks = append(blocks, block)
		}
	}

	streamError := currentStreamError(m.streamErr)
	if streamError != "" && streamError != m.scrollback.streamError {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: streamError,
		})
	}

	return blocks
}

func (m *Model) reconcileScrollback() {
	if m.scrollback.messages > len(m.state.Transcript) ||
		m.scrollback.tools > len(m.state.Tools) ||
		m.scrollback.diagnostics > len(m.state.Diagnostics) {
		m.resetScrollback()
	}

	if m.state.Changes == nil {
		m.scrollback.changes = projectionFingerprint{}
	}

	if currentStateError(m.state) == (projectionFingerprint{}) {
		m.scrollback.lastError = projectionFingerprint{}
	}

	if m.streamErr == nil {
		m.scrollback.streamError = ""
	}
}

func (m *Model) pendingCompletionMarkers() []completionMarker {
	if len(m.completionMarkers) == 0 {
		return nil
	}

	pending := make([]completionMarker, 0, len(m.completionMarkers))
	for _, marker := range m.completionMarkers {
		if _, committed := m.scrollback.completions[marker.interactionID]; !committed {
			pending = append(pending, marker)
		}
	}

	return pending
}

func (m *Model) markCompletionCommitted(marker completionMarker) {
	if m.scrollback.completions == nil {
		m.scrollback.completions = make(map[string]struct{})
	}

	m.scrollback.completions[marker.interactionID] = struct{}{}
}

func currentChanges(state coding.State) projectionFingerprint {
	if state.Changes == nil {
		return projectionFingerprint{}
	}

	return sha256.Sum256([]byte(fmt.Sprintf("%#v", *state.Changes)))
}

func currentStateError(state coding.State) projectionFingerprint {
	if state.LastError == nil || state.Interaction.Outcome == coding.InteractionCanceled {
		return projectionFingerprint{}
	}

	return sha256.Sum256([]byte(fmt.Sprintf("%#v", *state.LastError)))
}

func currentStreamError(err error) string {
	if err == nil {
		return ""
	}

	return strings.TrimSpace(safeError(err))
}
