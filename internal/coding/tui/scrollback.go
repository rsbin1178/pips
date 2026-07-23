//nolint:wsl_v5 // Append-only cursor transitions stay adjacent to their guards.
package tui

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
)

// scrollbackCursor separates immutable conversation history from the live
// tail Bubble Tea still owns. Stable blocks are printed above the inline
// Program once, so the terminal can retain, select, and scroll them natively.
type scrollbackCursor struct {
	messages    int
	tools       int
	subagents   int
	diagnostics int
	toolIDs     map[string]struct{}
	completions map[string]struct{}
	changes     projectionFingerprint
	lastError   projectionFingerprint
	streamError string
}

type projectionFingerprint [sha256.Size]byte

type scrollbackWrite struct {
	content      string
	continuation bool
}

func (m *Model) resetScrollback() {
	m.scrollback = scrollbackCursor{}
	m.streaming.reset()
	m.timeline = ""
}

func (m *Model) commitStableTimeline() tea.Cmd {
	managedHeight := lipgloss.Height(m.readyView().Content)
	blocks := m.takeStableTimelineBlocks()
	writes := m.streamingScrollbackWrites(blocks)
	m.rerenderTranscript(false)
	waitForRender := managedHeight != lipgloss.Height(m.readyView().Content)

	return m.printScrollbackWritesAfterRender(writes, waitForRender)
}

// printScrollback is the single boundary for output that leaves Bubble Tea's
// managed inline frame. Projection resets do not reset this latch because the
// terminal's native history survives New, Resume, Fork, and model changes.
func (m *Model) printScrollback(content string) tea.Cmd {
	return m.printScrollbackWrites([]scrollbackWrite{{content: content}})
}

func (m *Model) printScrollbackWrites(writes []scrollbackWrite) tea.Cmd {
	return m.printScrollbackWritesAfterRender(writes, true)
}

func (m *Model) printScrollbackWritesAfterRender(
	writes []scrollbackWrite,
	waitForRender bool,
) tea.Cmd {
	var content strings.Builder

	hasOutput := m.scrollbackOutput

	for _, write := range writes {
		if write.content == "" {
			continue
		}

		switch {
		case content.Len() > 0 && write.continuation:
			content.WriteByte('\n')
		case content.Len() > 0:
			content.WriteString(strings.Repeat("\n", conversationGapHeight+1))
		case hasOutput && !write.continuation:
			content.WriteString(strings.Repeat("\n", conversationGapHeight))
		}

		content.WriteString(write.content)

		hasOutput = true
	}

	if content.Len() == 0 {
		return nil
	}

	m.scrollbackOutput = true

	return m.printPreparedScrollback(content.String(), waitForRender)
}

func (m *Model) printPreparedScrollback(content string, waitForRender bool) tea.Cmd {
	// Bubble Tea's inline insertAbove implementation first reserves physical
	// rows below the managed frame. One insert must therefore fit in the
	// currently unused terminal rows; a multi-screen Println otherwise scrolls
	// blank reservation rows into native history before writing the payload.
	managedHeight := lipgloss.Height(m.readyView().Content)
	maximumRows := max(1, m.height-managedHeight)
	chunks := splitScrollbackContent(content, m.width, maximumRows)

	commands := make([]tea.Cmd, 0, len(chunks)+2)
	// The renderer flushes on its own frame clock. When a multi-line live
	// draft becomes stable, the Model has already removed it from the managed
	// View, but insertAbove can still observe the previous full-height cell
	// buffer if Println runs immediately. Give the shrunken View one complete
	// renderer window before Bubble Tea performs terminal-relative insertions.
	if waitForRender {
		commands = append(commands, tea.Tick(renderFrame, func(time.Time) tea.Msg {
			return scrollbackRenderReadyMsg{}
		}))
	}

	for _, chunk := range chunks {
		if chunk == "" {
			// insertAbove ignores an empty body, so retain a deliberately blank
			// physical row with a zero-width terminal reset sequence.
			chunk = "\x1b[0m"
		}

		commands = append(commands, tea.Println(chunk))
	}

	// Bubble Tea v2.0.8 resets its renderer cursor after insertAbove, then
	// skips an identical View. Keep a cursor-only change alive across one
	// renderer frame so it cannot be coalesced with its restoration. The
	// generation prevents overlapping stable commits from clearing a newer
	// refresh before Bubble Tea restores the Composer coordinates.
	m.cursorRefreshSeq++
	sequence := m.cursorRefreshSeq

	commands = append(commands, func() tea.Msg {
		return scrollbackCursorRefreshMsg{sequence: sequence}
	})

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
	return m.renderTimelineBlocks(m.takeStableTimelineBlocks())
}

//nolint:gocyclo // One cursor transaction atomically advances every durable projection.
func (m *Model) takeStableTimelineBlocks() []timelineBlock {
	m.reconcileScrollback()

	stableTools := m.scrollback.tools
	for stableTools < len(m.state.Tools) &&
		m.state.Tools[stableTools].Status == coding.ToolStatusCompleted {
		stableTools++
	}

	stableMessages := len(m.state.Transcript)
	if exploreStart, held := m.openTrailingExploreGroup(stableTools); held {
		stableTools = exploreStart
		stableMessages = m.heldExploreMessageFrontier(exploreStart)
	}

	stableSubagents := m.scrollback.subagents
	for stableSubagents < len(m.state.Subagents) &&
		isTerminalSubagent(m.state.Subagents[stableSubagents].State) {
		stableSubagents++
	}

	delta := m.state.Clone()

	delta.Transcript = delta.Transcript[m.scrollback.messages:stableMessages]
	delta.Draft = nil
	delta.Tools = delta.Tools[m.scrollback.tools:stableTools]
	delta.Subagents = delta.Subagents[m.scrollback.subagents:stableSubagents]

	delta.Diagnostics = delta.Diagnostics[m.scrollback.diagnostics:]

	changes := currentChanges(m.state)
	if changes == m.scrollback.changes {
		delta.Changes = nil
	}

	lastError := currentStateError(m.state)
	if lastError == m.scrollback.lastError {
		delta.LastError = nil
	}

	blocks := projectTimelineExcludingTools(delta, m.scrollback.toolIDs)

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

	for _, block := range blocks {
		m.markToolActivitiesCommitted(block)
	}
	for index := m.scrollback.tools; index < stableTools; index++ {
		m.markToolIDCommitted(m.state.Tools[index].Call.ID)
	}
	for index := m.scrollback.subagents; index < stableSubagents; index++ {
		child := m.state.Subagents[index]
		for _, tool := range m.state.Tools {
			if tool.Call.Name == subagent.ToolName && tool.RunID == child.ParentRunID {
				m.markToolIDCommitted(tool.Call.ID)

				break
			}
		}
	}

	m.scrollback.messages = stableMessages
	m.scrollback.tools = stableTools
	m.scrollback.subagents = stableSubagents
	m.scrollback.diagnostics = len(m.state.Diagnostics)
	m.scrollback.changes = changes

	m.scrollback.lastError = lastError

	if streamError != "" {
		m.scrollback.streamError = streamError
	}

	return blocks
}

func (m *Model) renderTimelineBlocks(blocks []timelineBlock) string {
	return renderTimelineContent(
		blocks, m.markdown, m.width, m.theme, m.options.NoColor,
	)
}

func (m *Model) activeTimelineBlocks() []timelineBlock {
	m.reconcileScrollback()

	active := m.state.Clone()

	active.Transcript = active.Transcript[m.scrollback.messages:]
	active.Tools = active.Tools[m.scrollback.tools:]
	active.Subagents = active.Subagents[m.scrollback.subagents:]

	active.Diagnostics = active.Diagnostics[m.scrollback.diagnostics:]

	if currentChanges(m.state) == m.scrollback.changes {
		active.Changes = nil
	}

	if currentStateError(m.state) == m.scrollback.lastError {
		active.LastError = nil
	}

	blocks := projectTimelineExcludingTools(active, m.scrollback.toolIDs)
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

	if m.streaming.active {
		for index := range blocks {
			if blocks[index].kind == blockDraft {
				blocks[index] = m.streamingTailBlock(blocks[index])

				break
			}
		}
	}

	return blocks
}

// openTrailingExploreGroup reports the completed-tool frontier that must stay
// mutable. An exploration group is held until another Tool class begins,
// visible assistant output starts, or its owning run/interaction terminates.
func (m *Model) openTrailingExploreGroup(stableTools int) (int, bool) {
	cursor := m.scrollback.tools
	if cursor >= len(m.state.Tools) {
		return stableTools, false
	}

	if stableTools < len(m.state.Tools) {
		if !isExploreToolName(m.state.Tools[stableTools].Call.Name) {
			return stableTools, false
		}

		start := stableTools
		for start > cursor && isExploreToolName(m.state.Tools[start-1].Call.Name) {
			start--
		}

		return start, true
	}

	if stableTools == cursor || !isExploreToolName(m.state.Tools[stableTools-1].Call.Name) {
		return stableTools, false
	}

	start := stableTools - 1
	for start > cursor && isExploreToolName(m.state.Tools[start-1].Call.Name) {
		start--
	}
	if m.exploreGroupHasBoundary(start, stableTools) {
		return stableTools, false
	}

	return start, true
}

func (m *Model) exploreGroupHasBoundary(start, end int) bool {
	if visibleDraftText(m.state.Draft) != "" || !m.state.Interaction.Active {
		return true
	}
	if m.state.Phase == coding.PhaseIdle || m.state.Phase == coding.PhaseClosing ||
		m.state.Phase == coding.PhaseClosed {
		return true
	}

	groupIDs := make(map[string]struct{}, end-start)
	for _, tool := range m.state.Tools[start:end] {
		groupIDs[tool.Call.ID] = struct{}{}
	}
	latest := latestToolMessagePosition(m.state.Transcript, groupIDs)
	if latest > 0 && hasVisibleAssistantMessage(m.state.Transcript[latest:]) {
		return true
	}

	lastRunID := m.state.Tools[end-1].RunID
	if lastRunID == "" {
		return false
	}
	for _, run := range m.state.Runs {
		if run.ID == lastRunID {
			return !run.Active
		}
	}

	return false
}

func hasVisibleAssistantMessage(transcript []ai.Message) bool {
	for _, message := range transcript {
		if message.Role == ai.RoleAssistant && visibleMessageText(message) != "" {
			return true
		}
	}

	return false
}

func (m *Model) heldExploreMessageFrontier(start int) int {
	groupIDs := make(map[string]struct{})
	for index := start; index < len(m.state.Tools); index++ {
		tool := m.state.Tools[index]
		if !isExploreToolName(tool.Call.Name) {
			break
		}
		groupIDs[tool.Call.ID] = struct{}{}
	}

	frontier := earliestToolCallPosition(m.state.Transcript, groupIDs)
	return max(m.scrollback.messages, frontier)
}

func latestToolMessagePosition(
	transcript []ai.Message,
	ids map[string]struct{},
) int {
	latest := 0
	for messageIndex, message := range transcript {
		for _, part := range message.Parts {
			switch value := part.(type) {
			case ai.ToolCallPart:
				if _, ok := ids[value.ID]; ok {
					latest = messageIndex + 1
				}
			case ai.ToolResultPart:
				if _, ok := ids[value.ToolCallID]; ok {
					latest = messageIndex + 1
				}
			}
		}
	}

	return latest
}

func earliestToolCallPosition(
	transcript []ai.Message,
	ids map[string]struct{},
) int {
	for messageIndex, message := range transcript {
		for _, part := range message.Parts {
			call, ok := part.(ai.ToolCallPart)
			if !ok {
				continue
			}
			if _, exists := ids[call.ID]; exists {
				return messageIndex + 1
			}
		}
	}

	return 0
}

func isExploreToolName(name string) bool {
	return name == toolNameRead || name == toolNameList ||
		name == toolNameGlob || name == toolNameSearch
}

func (m *Model) markToolActivitiesCommitted(block timelineBlock) {
	for _, id := range toolActivityIDs(block) {
		m.markToolIDCommitted(id)
	}
}

func (m *Model) markToolIDCommitted(id string) {
	if id == "" {
		return
	}
	if m.scrollback.toolIDs == nil {
		m.scrollback.toolIDs = make(map[string]struct{})
	}

	m.scrollback.toolIDs[id] = struct{}{}
}

func (m *Model) reconcileScrollback() {
	if m.scrollback.messages > len(m.state.Transcript) ||
		m.scrollback.tools > len(m.state.Tools) ||
		m.scrollback.subagents > len(m.state.Subagents) ||
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
