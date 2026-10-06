//nolint:wsl_v5 // Append-only cursor transitions stay adjacent to their guards.
package tui

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
)

// scrollbackCursor separates immutable conversation history from the live
// tail Bubble Tea still owns. Stable blocks are printed above the inline
// Program once, so the terminal can retain, select, and scroll them natively.
type scrollbackCursor struct {
	messages     int
	tools        int
	diagnostics  int
	toolIDs      map[string]struct{}
	completions  map[string]struct{}
	teamAttempts map[teamAttemptKey]projectionFingerprint
	changes      projectionFingerprint
	lastError    projectionFingerprint
	streamError  string
}

type projectionFingerprint [sha256.Size]byte

type scrollbackWrite struct {
	content      string
	continuation bool
}

func (m *Model) resetScrollback() {
	teamAttempts := m.scrollback.teamAttempts
	m.scrollback = scrollbackCursor{}
	m.scrollback.teamAttempts = teamAttempts
	// Inspection output belongs to the projection being replaced, so a session
	// change drops whatever a route was holding instead of printing it later.
	m.presentation.heldInspections = nil
	m.streaming.reset()
	m.timeline = ""
}

func (m *Model) commitStableTimeline() tea.Cmd {
	if !m.sizeReady || m.route.kind != routeNone || m.presentation.pendingRoute.pending() {
		return nil
	}

	// Fullscreen has no terminal history to catch up on, so a stable commit is a
	// re-render of the managed region and nothing else. The empty write queue also
	// means routes and continuations activate immediately instead of waiting for a
	// native insertion that would not paint anything.
	if m.fullscreen() {
		m.rerenderTranscript(false)

		return nil
	}

	blocks := m.takeStableTimelineBlocks()
	writes := m.streamingScrollbackWrites(blocks)
	m.rerenderTranscript(false)

	return m.printScrollbackWrites(writes)
}

// printScrollback is the single boundary for output that leaves Bubble Tea's
// managed inline frame. Projection resets do not reset this latch because the
// terminal's native history survives New, Resume, Fork, and model changes.
func (m *Model) printScrollback(content string) tea.Cmd {
	return m.printScrollbackWrites([]scrollbackWrite{{content: content}})
}

func (m *Model) printScrollbackWrites(writes []scrollbackWrite) tea.Cmd {
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

	return m.enqueueScrollback(content.String())
}

// takeStableTimeline advances the scrollback cursor and returns the immutable
// delta to print. Keeping projection separate from the Tea command makes the
// append-only contract directly testable.
func (m *Model) takeStableTimeline() string {
	if m.fullscreen() {
		return ""
	}

	return m.renderTimelineBlocks(m.takeStableTimelineBlocks())
}

//nolint:gocyclo // One cursor transaction atomically advances every durable projection.
func (m *Model) takeStableTimelineBlocks() []timelineBlock {
	// Nothing leaves the managed frame in fullscreen mode: the transcript store
	// owns every entry, so there is no native history to synchronize with and the
	// projection cursor stays at zero.
	if m.fullscreen() {
		return nil
	}

	m.reconcileScrollback()

	stableTools := m.scrollback.tools
	heldSubagentTool := ""
	for stableTools < len(m.state.Tools) &&
		m.state.Tools[stableTools].Status == coding.ToolStatusCompleted {
		if m.holdCompletedSubagentTool(m.state.Tools[stableTools]) {
			heldSubagentTool = m.state.Tools[stableTools].Call.ID
			break
		}
		stableTools++
	}
	if heldSubagentTool == "" && stableTools < len(m.state.Tools) &&
		isSubagentToolName(m.state.Tools[stableTools].Call.Name) {
		heldSubagentTool = m.state.Tools[stableTools].Call.ID
	}

	stableMessages := len(m.state.Transcript)
	if heldSubagentTool != "" {
		stableMessages = m.heldToolMessageFrontier(heldSubagentTool)
	}
	if exploreStart, held := m.openTrailingExploreGroup(stableTools); held {
		stableTools = exploreStart
		stableMessages = min(stableMessages, m.heldExploreMessageFrontier(exploreStart))
	}

	delta := m.state.Clone()

	delta.Transcript = delta.Transcript[m.scrollback.messages:stableMessages]
	delta.MessageCandidates = sliceMessageCandidates(
		m.state.MessageCandidates, m.scrollback.messages, stableMessages,
	)
	delta.SyntheticMessages = sliceSyntheticMessageIndexes(
		m.state.SyntheticMessages,
		m.scrollback.messages,
		stableMessages,
	)
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

	blocks := m.projectTimeline(delta, m.scrollback.toolIDs)

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
	blocks = append(blocks, m.takeStableTeamAttemptBlocks()...)
	for index := m.scrollback.tools; index < stableTools; index++ {
		m.markToolIDCommitted(m.state.Tools[index].Call.ID)
	}
	m.scrollback.messages = stableMessages
	m.scrollback.tools = stableTools
	m.scrollback.diagnostics = len(m.state.Diagnostics)
	m.scrollback.changes = changes

	m.scrollback.lastError = lastError

	if streamError != "" {
		m.scrollback.streamError = streamError
	}

	// Inline mode writes these rows to the terminal's native scrollback, which
	// renders no per-block styling, so it never carries reasoning text.
	return withoutThinkingBlocks(blocks)
}

func (m *Model) renderTimelineBlocks(blocks []timelineBlock) string {
	return m.renderTimelineBlocksWithOptions(blocks, timelineRenderOptions{})
}

func (m *Model) renderTimelineBlocksWithOptions(
	blocks []timelineBlock,
	options timelineRenderOptions,
) string {
	return renderTimelineContentWithOptions(
		blocks, m.markdown, m.width, m.theme, m.options.NoColor, options,
	)
}

func (m *Model) activeTimelineBlocks() []timelineBlock {
	m.reconcileScrollback()

	active := m.state.Clone()

	active.Transcript = active.Transcript[m.scrollback.messages:]
	active.MessageCandidates = sliceMessageCandidates(
		m.state.MessageCandidates, m.scrollback.messages, len(m.state.Transcript),
	)
	active.SyntheticMessages = sliceSyntheticMessageIndexes(
		m.state.SyntheticMessages,
		m.scrollback.messages,
		len(m.state.Transcript),
	)
	active.Tools = active.Tools[m.scrollback.tools:]

	active.Diagnostics = active.Diagnostics[m.scrollback.diagnostics:]

	if currentChanges(m.state) == m.scrollback.changes {
		active.Changes = nil
	}

	if currentStateError(m.state) == m.scrollback.lastError {
		active.LastError = nil
	}

	blocks := m.projectTimeline(active, m.scrollback.toolIDs)
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
		replaced := false
		for index := range blocks {
			if blocks[index].kind == blockDraft {
				blocks[index] = m.streamingTailBlock(blocks[index])
				replaced = true

				break
			}
		}
		if !replaced {
			if index := m.matchingStreamingAssistantIndex(blocks); index >= 0 {
				blocks[index] = m.streamingTailBlock(blocks[index])
			}
		}
	}

	blocks = append(blocks, m.activeTeamAttemptBlocks()...)

	// The inline tail is written to the terminal's native scrollback like the
	// stable blocks above it, so it never carries reasoning text either.
	return withoutThinkingBlocks(blocks)
}

// fullscreenTimelineBlocks projects the entire conversation for the transcript
// viewport. Unlike activeTimelineBlocks it has no projection cursor: nothing has
// been handed to the terminal, so the managed region owns every entry, including
// the markers that the inline path prints once.
func (m *Model) fullscreenTimelineBlocks() []timelineBlock {
	return m.managedTimelineBlocks(true, m.showThinkingBlocks())
}

// managedTimelineBlocks builds the mode-independent projection both the managed
// viewport and an export use. Only the viewport includes the operator-facing
// notices (banner, /help, /status), because inline mode hands those to the
// terminal instead of keeping them: including them here would make the same
// session export a different document per presentation mode.
//
// includeThinking gates reasoning text. The viewport shows it as dimmed Thinking
// blocks; an export carries the conversation without it, matching the
// reference agents (see research/q3-q4-export-copy-and-search.md).
func (m *Model) managedTimelineBlocks(includeNotices, includeThinking bool) []timelineBlock {
	blocks := m.timelineBlocks()
	if includeNotices {
		blocks = append(m.noticeBlocks(), blocks...)
	}
	// Durable history older than the bootstrap window leads the conversation.
	blocks = append(m.historyBlocks(), blocks...)
	blocks = append(blocks, m.activeTeamAttemptBlocks()...)

	if m.streaming.active {
		replaced := false
		for index := range blocks {
			if blocks[index].kind == blockDraft {
				blocks[index] = m.streamingTailBlock(blocks[index])
				replaced = true

				break
			}
		}
		if !replaced {
			if index := m.matchingStreamingAssistantIndex(blocks); index >= 0 {
				blocks[index] = m.streamingTailBlock(blocks[index])
			}
		}
	}

	if !includeThinking {
		blocks = withoutThinkingBlocks(blocks)
	}

	return blocks
}

// queuePlanModeNotice records one plan-mode transition row at the current
// conversation position. Rows are printed for an approved agent-initiated
// enter_plan_mode and for a closed plan review; a user /plan or Shift+Tab toggle
// prints none, matching grok-build.
func (m *Model) queuePlanModeNotice(body string) {
	if body == "" {
		return
	}

	m.planNoticeSeq++
	m.completionMarkers = appendMarker(m.completionMarkers, completionMarker{
		interactionID: "plan-" + strconv.FormatUint(m.planNoticeSeq, 10),
		afterMessages: len(m.state.Transcript),
		notice:        body,
	})
}

// planModeEnteredNotice is the row an approved agent-initiated entry prints.
func planModeEnteredNotice(permission string) string {
	return "Agent entered plan mode · active permission mode: " + permission +
		" · file edits outside session plan.md blocked until plan mode exits"
}

// planModeExitedNotice is the row a closed plan review prints, carrying its
// approved or abandoned verdict.
func planModeExitedNotice(verdict, permission string) string {
	return "Plan " + verdict + " · plan mode off · active permission mode: " + permission
}

// planModeNoticePermission names the permission mode armed underneath plan mode.
func (m *Model) planModeNoticePermission() string {
	m.refreshPermissionMode()
	if m.permissionMode == "" {
		return string(config.SandboxWorkspaceWrite)
	}

	return string(m.permissionMode)
}

func sliceMessageCandidates(
	values []coding.CandidateIdentity,
	start int,
	end int,
) []coding.CandidateIdentity {
	result := make([]coding.CandidateIdentity, max(0, end-start))
	if start >= len(values) || len(result) == 0 {
		return result
	}

	copy(result, values[start:min(end, len(values))])

	return result
}

func (m *Model) holdCompletedSubagentTool(tool coding.ToolState) bool {
	if !isSubagentToolName(tool.Call.Name) {
		return false
	}

	for _, child := range m.state.Subagents {
		if (child.ParentToolCallID != "" && child.ParentToolCallID == tool.Call.ID) ||
			(child.ParentToolCallID == "" && tool.RunID != "" &&
				child.ParentRunID == tool.RunID) {
			return !isTerminalSubagent(child.State)
		}
	}

	if m.state.Interaction.Active {
		return true
	}
	for _, run := range m.state.Runs {
		if run.ID == tool.RunID {
			return run.Active
		}
	}

	return false
}

func sliceSyntheticMessageIndexes(values []int, start, end int) []int {
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value >= start && value < end {
			result = append(result, value-start)
		}
	}

	return result
}

func (m *Model) heldToolMessageFrontier(callID string) int {
	for messageIndex, message := range m.state.Transcript {
		for _, part := range portableMessageParts(message) {
			call, ok := part.(ai.ToolCallPart)
			if ok && call.ID == callID {
				return max(m.scrollback.messages, messageIndex)
			}
		}
	}

	return m.scrollback.messages
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
		if _, isAssistant := message.(ai.AssistantMessage); isAssistant && visibleMessageText(message) != "" {
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
		for _, part := range portableMessageParts(message) {
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
		for _, part := range portableMessageParts(message) {
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
