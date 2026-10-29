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
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

// scrollbackCursor separates immutable conversation history from the live
// tail Bubble Tea still owns. Stable blocks are printed above the inline
// Program once, so the terminal can retain, select, and scroll them natively.
type scrollbackCursor struct {
	messages    int
	tools       int
	diagnostics int
	// transcriptBase and toolsBase record the State window the cursors were
	// measured against. A live transcript trim advances the base; the cursors
	// then move down by the same amount so the printed prefix stays aligned.
	transcriptBase int
	toolsBase      int
	toolIDs        map[string]struct{}
	completions    map[string]struct{}
	teamAttempts   map[teamAttemptKey]projectionFingerprint
	changes        projectionFingerprint
	lastError      projectionFingerprint
	streamError    string
	// compactionShown records that the first stable projection after a compaction
	// already handed the divider to the terminal. The cursor may still sit at
	// message zero because a checkpoint replaces the transcript with no visible
	// message of its own.
	compactionShown bool
	// subagentStarted records the description each spawn committed with its
	// opening row, keyed by child session; subagentTerminal records the children
	// that already committed a separate terminal row. A background child produces
	// both rows; a foreground child produces only the terminal one, because its
	// opening row is held until the child settles.
	subagentStarted  map[string]string
	subagentTerminal map[string]struct{}
}

type projectionFingerprint [sha256.Size]byte

type scrollbackWrite struct {
	content      string
	continuation bool
}

func (m *Model) resetScrollback() {
	teamAttempts := m.scrollback.teamAttempts
	m.scrollback = scrollbackCursor{
		transcriptBase: m.state.TranscriptBase(),
		toolsBase:      m.state.ToolsBase(),
	}
	m.scrollback.teamAttempts = teamAttempts
	// Inspection output belongs to the projection being replaced, so a session
	// change drops whatever a route was holding instead of printing it later.
	m.presentation.heldInspections = nil
	// Deferred pages are the replaced projection's rendered content; accepted
	// writes stay queued because they are already on their way to the terminal.
	m.dropPendingScrollback()
	m.streaming.reset()
	// A live version of the replaced projection must not reach the new frame, so
	// outstanding background work is retired with it.
	m.retireLiveRenderState()
	// The managed store is part of the replaced projection. Its position-based
	// identities can collide with different content after a compaction or a
	// session switch, so the next frame must render the replacement from scratch.
	m.transcript.reset()
	// The rail's focus names a turn of the projection being replaced.
	m.endTurnRailFocus()
}

func (m *Model) commitStableTimeline() tea.Cmd {
	if !m.sizeReady || m.route.kind != routeNone || m.presentation.pendingRoute.pending() {
		return m.armLiveWork()
	}

	// Fullscreen has no terminal history to catch up on, so a stable commit is a
	// re-render of the managed region and nothing else. The empty write queue also
	// means routes and continuations activate immediately instead of waiting for a
	// native insertion that would not paint anything.
	if m.fullscreen() {
		m.rerenderTranscript(false)

		return m.armLiveWork()
	}

	// A slow terminal is the backpressure boundary: while rendered pages wait,
	// keep the stable projection in State instead of rendering another copy. The
	// write completion catches up once the queue has room.
	if m.presentation.backpressured() {
		m.rerenderTranscript(false)

		return m.armLiveWork()
	}

	blocks := m.takeStableTimelineBlocks()
	sources := m.streamingScrollbackSources(blocks)
	m.rerenderTranscript(false)

	return tea.Batch(m.printScrollbackSources(sources), m.armLiveWork())
}

// printScrollback is the single boundary for output that leaves Bubble Tea's
// managed inline frame. Projection resets do not reset this latch because the
// terminal's native history survives New, Resume, Fork, and model changes.
func (m *Model) printScrollback(content string) tea.Cmd {
	return m.printScrollbackWrites([]scrollbackWrite{{content: content}})
}

// printScrollbackBlocks renders stable blocks in byte-bounded pages instead of
// joining the whole delta into one string. The banner, when present, is the
// first page's pre-rendered content. Pages that do not fit the queue stay in a
// pager and are written when earlier pages complete.
func (m *Model) printScrollbackBlocks(blocks []timelineBlock, banner string) tea.Cmd {
	pager := newScrollbackPager(blocks)
	pager.prependRendered(banner)
	if pager.empty() {
		return nil
	}

	return m.printScrollbackSources([]scrollbackSource{pager})
}

// printScrollbackSources queues lazy sources behind whatever is already waiting
// and moves as many pages into the write queue as it has room for.
func (m *Model) printScrollbackSources(sources []scrollbackSource) tea.Cmd {
	var commands []tea.Cmd

	for _, source := range sources {
		if source == nil {
			continue
		}

		m.presentation.pendingSources = append(m.presentation.pendingSources, source)
	}

	if !m.presentation.hasPendingScrollback() {
		return nil
	}

	if command := m.pumpPendingScrollback(); command != nil {
		commands = append(commands, command)
	}

	return tea.Batch(commands...)
}

// printScrollbackWrites queues already rendered writes. A write that fits one
// page is enqueued directly; a larger one waits as a pager over the caller's
// string, so deferring a huge payload never holds a rendered copy per page.
func (m *Model) printScrollbackWrites(writes []scrollbackWrite) tea.Cmd {
	var commands []tea.Cmd

	for _, write := range writes {
		if write.content == "" {
			continue
		}

		// Content already waiting is older than this write, so this write is
		// deferred behind it rather than overtaking it in the terminal.
		if len(write.content) <= scrollbackPageBytes && !m.presentation.backpressured() {
			if command := m.enqueueScrollbackPage(write.content, write.continuation); command != nil {
				commands = append(commands, command)
			}

			continue
		}

		m.presentation.pendingSources = append(m.presentation.pendingSources,
			newScrollbackTextPager(write.content, write.continuation))
	}

	// A deferred write owns no dispatch of its own, so the queue is filled here
	// instead of waiting for a completion that would never arrive.
	if m.presentation.hasPendingScrollback() {
		if command := m.pumpPendingScrollback(); command != nil {
			commands = append(commands, command)
		}
	}

	return tea.Batch(commands...)
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

	delta := m.incrementalProjection(stableMessages, stableTools)

	// The divider belongs to the first stable projection after a compaction.
	// Later deltas start below it, and the terminal already holds the printed row.
	if m.scrollback.compactionShown {
		delta.Compaction = coding.CompactionState{}
	}
	delta.Draft = coding.StreamDraft{}

	changes := currentChanges(m.state)
	if changes == m.scrollback.changes {
		delta.Changes = nil
	}

	lastError := currentStateError(m.state)
	if lastError == m.scrollback.lastError {
		delta.LastError = nil
	}

	blocks := m.projectTimeline(delta, m.scrollback.toolIDs)
	if _, _, hasDivider := projectCompactionDivider(m.state); hasDivider {
		m.scrollback.compactionShown = true
	}

	ready, _ := m.completionMarkersForDelta(stableMessages)
	for _, marker := range ready {
		m.markCompletionCommitted(marker)
	}
	blocks = insertCompletionMarkers(blocks, ready)
	blocks = insertQuestionPreviews(
		blocks,
		rebaseQuestionPreviews(m.takeOptimisticQuestionBlocks(), m.scrollback.messages),
	)

	streamError := currentStreamError(m.streamErr)
	if streamError != "" && streamError != m.scrollback.streamError {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: streamError,
		})
	}

	for _, block := range blocks {
		m.markToolActivitiesCommitted(block)
		m.markSubagentLifecycleCommitted(block)
	}
	blocks = append(blocks, m.takeSubagentTerminalBlocks()...)
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

// incrementalProjection returns a read-only view of the current State with the
// committed prefix sliced away. State transitions are copy-on-write and the
// timeline projection only reads, so the message, tool and diagnostic backings
// are shared instead of copied; the inline tail therefore costs its own rows,
// not a clone of the whole conversation.
func (m *Model) incrementalProjection(messageEnd, toolEnd int) coding.State {
	state := m.state
	messageStart := min(max(0, m.scrollback.messages), len(m.state.Transcript))
	messageEnd = min(max(messageStart, messageEnd), len(m.state.Transcript))
	toolStart := min(max(0, m.scrollback.tools), len(m.state.Tools))
	toolEnd = min(max(toolStart, toolEnd), len(m.state.Tools))
	diagnosticStart := min(max(0, m.scrollback.diagnostics), len(m.state.Diagnostics))

	state.Transcript = state.Transcript[messageStart:messageEnd]
	state.MessageCandidates = sliceMessageCandidates(
		m.state.MessageCandidates, messageStart, messageEnd,
	)
	state.SyntheticMessages = sliceSyntheticMessageIndexes(
		m.state.SyntheticMessages, messageStart, messageEnd,
	)
	state.Tools = state.Tools[toolStart:toolEnd]
	state.Diagnostics = state.Diagnostics[diagnosticStart:]

	return state
}

func (m *Model) activeTimelineBlocks() []timelineBlock {
	m.reconcileScrollback()

	active := m.incrementalProjection(len(m.state.Transcript), len(m.state.Tools))

	// The stable projection already printed the compaction divider. The live tail
	// starts below it, so it must not paint a second copy.
	if m.scrollback.compactionShown {
		active.Compaction = coding.CompactionState{}
	}

	if currentChanges(m.state) == m.scrollback.changes {
		active.Changes = nil
	}

	if currentStateError(m.state) == m.scrollback.lastError {
		active.LastError = nil
	}

	blocks := m.projectTimeline(active, m.scrollback.toolIDs)
	ready, _ := m.completionMarkersForDelta(len(m.state.Transcript))
	blocks = insertCompletionMarkers(blocks, ready)
	blocks = insertQuestionPreviews(
		blocks,
		rebaseQuestionPreviews(m.optimisticQuestionBlocks(), m.scrollback.messages),
	)

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
// prints none.
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

// sliceMessageCandidates returns a read-only view of the candidate identities
// covering [start, end). The timeline projection only reads them, so the view
// shares the State backing array instead of copying it.
func sliceMessageCandidates(
	values []coding.CandidateIdentity,
	start int,
	end int,
) []coding.CandidateIdentity {
	start = min(max(0, start), len(values))
	end = min(max(start, end), len(values))

	return values[start:end]
}

func (m *Model) holdCompletedSubagentTool(tool coding.ToolState) bool {
	if !isSubagentToolName(tool.Call.Name) {
		return false
	}

	for _, child := range m.state.Subagents {
		if (child.ParentToolCallID != "" && child.ParentToolCallID == tool.Call.ID) ||
			(child.ParentToolCallID == "" && tool.RunID != "" &&
				child.ParentRunID == tool.RunID) {
			// A background child does not hold the parent transcript. Its opening
			// row commits as soon as the spawn call returns, and the child's
			// terminal row is appended later as its own line — the two-row shape
			// the source gives a background spawn. A foreground child holds its
			// row until it settles so the spawn commits exactly once, in its
			// terminal form, which is the source's in-place shape.
			if child.Delivery == subagent.DeliveryBackground {
				return false
			}

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

// markSubagentLifecycleCommitted records which half of a spawn's lifecycle the
// committed row covered, so a background child's terminal row is appended once,
// with the same description its opening row used.
func (m *Model) markSubagentLifecycleCommitted(block timelineBlock) {
	row := block.subagent
	if row == nil || row.childSessionID == "" {
		return
	}
	if row.started() {
		if m.scrollback.subagentStarted == nil {
			m.scrollback.subagentStarted = make(map[string]string)
		}
		m.scrollback.subagentStarted[row.childSessionID] = row.description

		return
	}
	if m.scrollback.subagentTerminal == nil {
		m.scrollback.subagentTerminal = make(map[string]struct{})
	}
	m.scrollback.subagentTerminal[row.childSessionID] = struct{}{}
}

// takeSubagentTerminalBlocks appends the separate terminal row a background
// child's spawn leaves behind. Only a child whose opening row was already
// committed in this projection qualifies, so replaying a session whose child was
// already finished renders one row rather than two.
func (m *Model) takeSubagentTerminalBlocks() []timelineBlock {
	if visibleDraftText(m.state.Draft) != "" {
		return nil
	}

	blocks := make([]timelineBlock, 0, 1)
	for _, value := range m.state.Subagents {
		if value.ChildSessionID == "" || value.Delivery != subagent.DeliveryBackground ||
			!isTerminalSubagent(value.State) {
			continue
		}
		if _, started := m.scrollback.subagentStarted[value.ChildSessionID]; !started {
			continue
		}
		if _, committed := m.scrollback.subagentTerminal[value.ChildSessionID]; committed {
			continue
		}
		if m.scrollback.subagentTerminal == nil {
			m.scrollback.subagentTerminal = make(map[string]struct{})
		}
		m.scrollback.subagentTerminal[value.ChildSessionID] = struct{}{}
		description := m.scrollback.subagentStarted[value.ChildSessionID]
		if description == "" {
			description = oneLineSubagentTask(value.TaskPreview)
		}
		row := subagentLifecycleRow{
			childSessionID: value.ChildSessionID,
			description:    description,
			identity:       value.Identity,
			role:           value.Role,
			model:          value.Model,
			state:          value.State,
			durationMillis: value.DurationMillis,
			code:           value.Code,
			background:     true,
		}
		blocks = append(blocks, timelineBlock{
			kind:     blockSubagent,
			subagent: &row,
			tools:    []*toolActivity{{childSessionID: value.ChildSessionID}},
		})
	}

	return blocks
}

// rebaseScrollbackWindow moves the append-only cursors when the live transcript
// window releases its oldest messages. The printed prefix stays printed; the
// cursors only need to address the new backing window.
func (m *Model) rebaseScrollbackWindow() {
	m.rebaseWindowBases(m.state.TranscriptBase(), m.state.ToolsBase())
	m.pruneSubagentCursors()
}

// pruneSubagentCursors releases lifecycle bookkeeping for children the bounded
// parent projection has already dropped. The two maps only suppress duplicate
// rows for children the projection still carries.
func (m *Model) pruneSubagentCursors() {
	if len(m.scrollback.subagentStarted) == 0 && len(m.scrollback.subagentTerminal) == 0 {
		return
	}

	live := make(map[string]struct{}, len(m.state.Subagents))
	for _, child := range m.state.Subagents {
		live[child.ChildSessionID] = struct{}{}
	}
	for childSessionID := range m.scrollback.subagentStarted {
		if _, ok := live[childSessionID]; !ok {
			delete(m.scrollback.subagentStarted, childSessionID)
		}
	}
	for childSessionID := range m.scrollback.subagentTerminal {
		if _, ok := live[childSessionID]; !ok {
			delete(m.scrollback.subagentTerminal, childSessionID)
		}
	}
}

// rebaseWindowBases applies a window move to the inline cursors and the
// position-addressed TUI bookkeeping.
func (m *Model) rebaseWindowBases(transcriptBase, toolsBase int) {
	if drop := transcriptBase - m.scrollback.transcriptBase; drop > 0 {
		m.scrollback.messages = max(0, m.scrollback.messages-drop)
		m.rebaseCompletionMarkersForWindow(drop)
		m.rebaseQuestionPreviewsForWindow(drop)
		if len(m.history.messages) > 0 {
			// The loaded pages ended at the old window. Keep History contiguous by
			// discarding them; the first request reloads the page immediately above
			// the new window.
			m.history = historyState{more: true}
		} else {
			m.history.more = true
		}
	}
	m.scrollback.transcriptBase = transcriptBase

	if drop := toolsBase - m.scrollback.toolsBase; drop > 0 {
		m.scrollback.tools = max(0, m.scrollback.tools-drop)
		m.pruneCommittedToolIDs()
	}
	m.scrollback.toolsBase = toolsBase
}

// rebaseCompletionMarkersForWindow drops markers whose anchor left the window and
// moves the rest onto the retained transcript.
func (m *Model) rebaseCompletionMarkersForWindow(drop int) {
	if len(m.completionMarkers) == 0 {
		return
	}

	kept := make([]completionMarker, 0, len(m.completionMarkers))
	for _, marker := range m.completionMarkers {
		marker.afterMessages -= drop
		if marker.afterMessages <= 0 {
			delete(m.scrollback.completions, marker.interactionID)

			continue
		}
		kept = append(kept, marker)
	}

	m.completionMarkers = kept
	// The shifted count is only the fallback; a marker whose message is still in
	// the window re-resolves onto its own run or digest.
	m.rebaseCompletionMarkers()
}

// rebaseQuestionPreviewsForWindow drops optimistic previews whose result can no
// longer land in the retained transcript.
func (m *Model) rebaseQuestionPreviewsForWindow(drop int) {
	for toolCallID, preview := range m.questionPreviews {
		preview.position -= drop
		if preview.position <= 0 {
			delete(m.questionPreviews, toolCallID)

			continue
		}
		m.questionPreviews[toolCallID] = preview
	}
}

// pruneCommittedToolIDs releases identities whose Tool projections left the
// window, so the exclusion set stays proportional to the retained tools.
func (m *Model) pruneCommittedToolIDs() {
	if len(m.scrollback.toolIDs) == 0 {
		return
	}

	retained := make(map[string]struct{}, len(m.state.Tools))
	for _, tool := range m.state.Tools {
		if _, committed := m.scrollback.toolIDs[tool.Call.ID]; committed {
			retained[tool.Call.ID] = struct{}{}
		}
	}

	m.scrollback.toolIDs = retained
}

func (m *Model) reconcileScrollback() {
	m.rebaseScrollbackWindow()
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

// completionMarkersForDelta rebases the pending markers onto one inline delta
// and reports which of them the delta now covers. Native history is append-only,
// so a marker whose answer is not inside the delta waits for the delta that
// carries it instead of being printed above that answer.
func (m *Model) completionMarkersForDelta(covered int) ([]completionMarker, []completionMarker) {
	pending := m.pendingCompletionMarkers()
	if len(pending) == 0 {
		return nil, nil
	}

	base := m.scrollback.messages
	ready := make([]completionMarker, 0, len(pending))
	waiting := make([]completionMarker, 0, len(pending))

	for _, marker := range pending {
		marker.afterMessages -= base
		if marker.afterMessages <= covered-base {
			ready = append(ready, marker)

			continue
		}
		waiting = append(waiting, marker)
	}

	return ready, waiting
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
