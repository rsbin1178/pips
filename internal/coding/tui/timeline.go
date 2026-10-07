//nolint:wsl_v5 // Projection branches keep title/body assembly adjacent.
package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockDraft
	blockPlan
	blockTool
	blockQuestion
	blockDiagnostic
	blockChange
	blockError
	blockCompletion
	blockTeam
	// blockThinking carries one visible reasoning section. Appending it keeps the
	// existing kinds' identity ints stable for cached records and tests.
	blockThinking
	// blockIncomplete carries the retained text of an abandoned provisional
	// answer. It is its own kind so it can never render as, or be mistaken for,
	// a committed assistant message.
	blockIncomplete
)

const (
	completionGlyph       = "▣"
	errorAccentBar        = "▌"
	questionAnsweredTitle = "Question answered"
	questionFailedTitle   = "Question failed"
	planApprovedTitle     = "Plan · Approved"
)

// thinkingGlyph leads one visible reasoning section. The block follows the
// transcript's glyph convention — an icon before the content, like a user
// message's arrow and a tool heading's state glyph — instead of spending a
// header row on a "Thinking" label. The glyph sits outside the tool state set
// (✻ running, • succeeded, ✗ failed, ! interrupted), so a settled reasoning
// block cannot be mistaken for a running tool.
//
// Reasoning is rendered as plain, dimmed prose rather than through the Markdown
// engine: a live section grows with every streamed delta, and parsing it on each
// frame costs orders of magnitude more CPU than wrapping it.
const thinkingGlyph = "✧"

// transcriptHorizontalInset is how far conversation rows move in from the frame
// edge, matching the Composer's text column: the input box spends one border
// cell and one padding cell before its content.
const transcriptHorizontalInset = 2

// timelineInset reports the inset conversation rows use at one frame width. Only
// a frame wide enough for the bordered Composer has a text column to align with.
func timelineInset(width int) int {
	if width < composerBoxMinWidth {
		return 0
	}

	return transcriptHorizontalInset
}

type timelineBlock struct {
	kind             blockKind
	id               string
	title            string
	body             string
	status           string
	position         int
	rendered         bool
	tools            []toolActivity
	workspaceChanges *coding.WorkspaceChanged
	// notice marks operator output (the banner, /help, /status, goal reports)
	// that the frame pins to the terminal edge. It is the one entry kind that
	// keeps the full width, because the banner is a bordered box that lines up
	// with the Composer box instead of with the conversation inside it.
	notice bool
}

type timelineRenderOptions struct {
	expandToolResults bool
}

// completionMarker is a one-line durable row placed at a conversation position.
// Most markers report an interaction outcome; a marker whose notice is set
// instead carries a pre-rendered row that is not a completion (today, the
// plan-mode transition lines), and its outcome fields are unused.
type completionMarker struct {
	interactionID  string
	afterMessages  int
	outcome        coding.InteractionOutcome
	stop           agent.StopReason
	durationMillis int64
	model          string
	notice         string
}

func projectTimeline(state coding.State) []timelineBlock {
	return projectTimelineExcluding(state, nil)
}

func projectTimelineExcluding(
	state coding.State,
	excludedTools map[string]struct{},
) []timelineBlock {
	return projectTimelineActivities(state, projectToolActivities(state, excludedTools))
}

// committedProjection is the durable half of one timeline projection together
// with what the volatile half needs to know about it.
type committedProjection struct {
	blocks []timelineBlock
	// tailStart is the index of the first activity that belongs to the volatile
	// tail, so a cached frame can project only that tail.
	tailStart int
	// cutoff is the highest conversation position the committed blocks cover. It
	// bounds which completion markers are interleaved into the prefix.
	cutoff int
}

// projectTimelineActivities composes the pure projection: the committed prefix
// followed by the volatile tail. The incremental frame path reuses the prefix and
// rebuilds only the tail, and must produce exactly this list.
func projectTimelineActivities(state coding.State, activities []toolActivity) []timelineBlock {
	committed := projectCommittedBlocks(state, activities)

	return appendTimelineTail(
		appendTimelinePrefix(nil, committed.blocks),
		projectVolatileBlocks(state, activities[committed.tailStart:]),
	)
}

// appendTimelinePrefix appends a committed prefix to dst. The result never
// aliases committed, so a cached prefix is safe to pass here.
func appendTimelinePrefix(dst, committed []timelineBlock) []timelineBlock {
	return append(dst, committed...)
}

// appendTimelineTail appends the volatile tail after a prefix dst already holds.
// Grouping is a left fold, so grouping the halves separately and merging the seam
// matches grouping the concatenation. The merged block is copied first, which
// keeps a cached prefix immutable.
func appendTimelineTail(dst, tail []timelineBlock) []timelineBlock {
	if len(tail) == 0 {
		return dst
	}

	grouped := groupExploreBlocks(tail)
	if len(dst) == 0 || !isExploreBlock(dst[len(dst)-1]) || !isLoneExploreBlock(grouped[0]) {
		return append(dst, grouped...)
	}

	previous := &dst[len(dst)-1]
	previous.tools = append(slices.Clone(previous.tools), grouped[0].tools...)
	previous.id = grouped[0].id

	return append(dst, grouped[1:]...)
}

// appendCommittedMessageBlocks appends one transcript message's durable blocks
// and reports the conversation position it commits, or zero when the message
// commits none. It is the per-message half of [projectCommittedBlocks], shared so
// the append path can project only the messages that arrived since the cached
// frame instead of the whole conversation. It appends in place for the same
// reason [appendCommittedActivityBlock] does.
func appendCommittedMessageBlocks(
	blocks []timelineBlock,
	message ai.Message,
	position int,
	candidateID string,
) ([]timelineBlock, int) {
	if _, ok := message.(ai.AssistantMessage); ok {
		// An assistant turn can carry visible reasoning, which becomes its own
		// Thinking block ahead of the answer text.
		return append(blocks, projectAssistantBlocks(message, position, candidateID)...), position
	}

	body := visibleMessageText(message)
	if body == "" {
		return blocks, 0
	}

	switch message.(type) {
	case ai.UserMessage:
		return append(blocks, timelineBlock{
			kind: blockUser, body: body, position: position,
		}), position
	case ai.SystemMessage:
		return append(blocks, timelineBlock{
			kind: blockDiagnostic, title: "System", body: body, position: position,
		}), position
	default:
		// A Tool message's result reaches the timeline through the tool activity
		// overlay, never as a message block.
		return blocks, 0
	}
}

// projectCommittedBlocks projects the durable half of the timeline: the messages
// already committed to the transcript together with the tool activity that sits
// between them.
//
//nolint:gocyclo,cyclop // The already projected Tools merge with messages in durable order.
func projectCommittedBlocks(state coding.State, activities []toolActivity) committedProjection {
	blocks := make([]timelineBlock, 0, len(state.Transcript)+len(state.Tools)+4)
	syntheticMessages := make(map[int]struct{}, len(state.SyntheticMessages))
	for _, index := range state.SyntheticMessages {
		syntheticMessages[index] = struct{}{}
	}
	cutoff := 0
	activityIndex := 0
	for messageIndex, message := range state.Transcript {
		position := messageIndex + 1
		candidateID := ""
		if messageIndex < len(state.MessageCandidates) {
			candidateID = state.MessageCandidates[messageIndex].Key()
		}
		if _, synthetic := syntheticMessages[messageIndex]; !synthetic {
			committed := 0
			blocks, committed = appendCommittedMessageBlocks(blocks, message, position, candidateID)
			cutoff = max(cutoff, committed)
		}

		for activityIndex < len(activities) && activities[activityIndex].position <= position {
			activity := activities[activityIndex]
			activityIndex++

			committed := 0
			blocks, committed = appendCommittedActivityBlock(state, activity, blocks)
			cutoff = max(cutoff, committed)
		}
	}

	return committedProjection{
		blocks:    groupExploreBlocks(blocks),
		tailStart: activityIndex,
		cutoff:    cutoff,
	}
}

// projectCommittedRange projects the messages in Transcript[covered:] together
// with the activity the appended messages commit, in durable order. It is the
// append path's half of [projectCommittedBlocks]: for the messages an append
// added it must produce exactly the blocks that function would.
func projectCommittedRange(
	state coding.State,
	covered int,
	synthetic map[int]struct{},
	candidates []coding.CandidateIdentity,
	activities []toolActivity,
) ([]timelineBlock, int) {
	blocks := make([]timelineBlock, 0, len(state.Transcript)-covered+len(activities))
	cutoff := 0

	activityIndex := 0

	for messageIndex := covered; messageIndex < len(state.Transcript); messageIndex++ {
		position := messageIndex + 1
		candidateID := ""
		if messageIndex < len(candidates) {
			candidateID = candidates[messageIndex].Key()
		}
		if _, skip := synthetic[messageIndex]; !skip {
			committed := 0
			blocks, committed = appendCommittedMessageBlocks(
				blocks, state.Transcript[messageIndex], position, candidateID,
			)
			cutoff = max(cutoff, committed)
		}

		for activityIndex < len(activities) && activities[activityIndex].position <= position {
			activity := activities[activityIndex]
			activityIndex++

			committed := 0
			blocks, committed = appendCommittedActivityBlock(state, activity, blocks)
			cutoff = max(cutoff, committed)
		}
	}

	return blocks, cutoff
}

// appendCommittedActivityBlock appends one settled tool activity's committed block
// and reports the conversation position it commits, or zero when the activity
// produces no card. A protocol activity can be handled without a card of its own.
// It appends in place rather than returning the block, because every committed
// activity goes through here on a full projection.
func appendCommittedActivityBlock(
	state coding.State,
	activity toolActivity,
	blocks []timelineBlock,
) ([]timelineBlock, int) {
	block, handled, visible := projectProtocolToolActivity(activity)
	if handled {
		if !visible {
			return blocks, 0
		}

		return append(blocks, block), block.position
	}

	if isSubagentToolName(activity.name) {
		block = projectSubagentToolActivity(activity, state.Subagents)

		return append(blocks, block), block.position
	}

	block = projectToolActivity(activity)

	return append(blocks, block), block.position
}

// projectVolatileBlocks projects everything that can change while the transcript
// stays put: tool activity still running, the live draft, and the session-level
// notices that trail the timeline.
func projectVolatileBlocks(state coding.State, tail []toolActivity) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(tail)+len(state.Diagnostics)+4)
	for _, activity := range tail {
		if block, handled, visible := projectProtocolToolActivity(activity); handled {
			if visible {
				blocks = append(blocks, block)
			}

			continue
		}
		if isSubagentToolName(activity.name) {
			blocks = append(blocks, projectSubagentToolActivity(activity, state.Subagents))

			continue
		}
		blocks = append(blocks, projectToolActivity(activity))
	}

	// A live turn can stream reasoning before its answer; the Thinking block
	// keeps one identity so its row count and anchor survive every delta.
	blocks = append(blocks, draftReasoningBlocks(state)...)

	draft := visibleDraftText(state.Draft)
	if draft != "" {
		blocks = append(blocks, timelineBlock{
			kind: blockDraft, id: state.DraftCandidate.Key(), body: draft,
			position: len(state.Transcript),
		})
	}

	if state.Changes != nil {
		blocks = append(blocks, projectWorkspaceChangeBlock(
			*state.Changes,
			len(state.Transcript),
		))
	}

	for _, diagnostic := range state.Diagnostics {
		// Working outside a Git repository is a supported setup, so the TUI does
		// not report the change summary as unavailable.
		if nonRepositoryDiagnostic(diagnostic) {
			continue
		}
		blocks = append(blocks, timelineBlock{
			kind:     blockDiagnostic,
			title:    diagnosticTitle(diagnostic),
			body:     diagnosticBody(diagnostic),
			position: len(state.Transcript),
		})
	}

	for _, reply := range state.IncompleteReplies {
		blocks = append(blocks, incompleteReplyBlock(reply, len(state.Transcript)))
	}

	if state.LastError != nil && state.Interaction.Outcome != coding.InteractionCanceled {
		blocks = append(blocks, timelineBlock{
			kind: blockError, body: state.LastError.Message,
			status: state.LastError.Code, position: len(state.Transcript),
		})
	}

	return groupExploreBlocks(blocks)
}

// incompleteReplyBlock renders one abandoned provisional answer as its own
// block. The text is model output the user would otherwise never see, so it is
// shown, but the label and the explicit sentence keep it from reading as part
// of the conversation.
func incompleteReplyBlock(reply coding.IncompleteReply, position int) timelineBlock {
	lines := make([]string, 0, 4)
	if reason := strings.TrimSpace(reply.Reason); reason != "" {
		lines = append(lines, "Reason: "+reason)
	}

	lines = append(lines, "Incomplete text retained below; it is not part of the conversation.")
	if reply.Bytes > len(reply.Text) {
		// The record holds only a bounded prefix of what was produced, so say
		// so instead of letting the shown text read as the whole fragment.
		lines = append(lines, fmt.Sprintf(
			"The retained text was cut short: %d of %d bytes kept.", len(reply.Text), reply.Bytes,
		))
	}
	if reply.Text != "" {
		lines = append(lines, "", reply.Text)
	}

	return timelineBlock{
		kind:     blockIncomplete,
		title:    "Interrupted reply",
		body:     strings.Join(lines, "\n"),
		position: position,
	}
}

// draftThinkingID is the identity of the in-flight turn's Thinking block. It is
// deliberately turn-independent: the growing block keeps one identity across
// every streamed delta, and the projection switches to the committed identity
// once the message lands in the transcript.
const draftThinkingID = "thinking:draft"

// blockIsLive reports whether a block is one of the streaming drafts. Both grow
// with every stream delta, so their Markdown shares one bounded live slot instead
// of entering the settled LRU.
func blockIsLive(block timelineBlock) bool {
	return block.kind == blockDraft || block.id == draftThinkingID
}

// blockIsUnsettled reports whether a block's rendering can still change under its
// own identity, so no record rendered from an earlier frame may serve it. It is
// wider than [blockIsLive]: a tool card arrives while its activity runs and changes
// again when its result lands, both under the one identity that activity carries.
// The transcript store's reuse therefore cannot rest on identity alone.
func blockIsUnsettled(block timelineBlock) bool {
	if blockIsLive(block) {
		return true
	}

	for index := range block.tools {
		if !block.tools[index].settled {
			return true
		}
	}

	return false
}

// thinkingBlockID names one committed reasoning section. The message position
// plus the part index is stable across re-renders and does not change when the
// text keeps growing, which a content hash would.
func thinkingBlockID(position, index int) string {
	return "thinking:" + strconv.Itoa(position) + ":" + strconv.Itoa(index)
}

// projectAssistantBlocks renders one assistant message as its Thinking blocks
// followed by the visible answer text. Reasoning that the provider withheld
// (Redacted) or returned without text produces no block at all, so a model that
// only returns encrypted state renders nothing rather than an empty header.
func projectAssistantBlocks(message ai.Message, position int, candidateID string) []timelineBlock {
	blocks := reasoningBlocks(message, position)
	if body := visibleMessageText(message); body != "" {
		blocks = append(blocks, timelineBlock{
			kind: blockAssistant, id: candidateID, body: body, position: position,
		})
	}

	return blocks
}

// reasoningBlocks projects the visible reasoning parts of one message, in part
// order, as Thinking blocks. It reads the parts in place; see
// [portableMessageParts].
func reasoningBlocks(message ai.Message, position int) []timelineBlock {
	parts, err := ai.MessagePartsView(message)
	if err != nil {
		return nil
	}

	var blocks []timelineBlock

	index := 0

	for _, part := range parts {
		value, ok := part.(ai.ReasoningPart)
		if !ok {
			continue
		}

		if !value.Redacted && strings.TrimSpace(value.Text) != "" {
			blocks = append(blocks, timelineBlock{
				kind: blockThinking, id: thinkingBlockID(position, index),
				body: value.Text, position: position,
			})
		}

		index++
	}

	return blocks
}

// draftReasoningBlocks aggregates the in-flight turn's reasoning deltas. A
// signature-only delta carries no text and contributes nothing.
func draftReasoningBlocks(state coding.State) []timelineBlock {
	var content strings.Builder
	for _, delta := range state.Draft {
		if delta.Kind == ai.StreamReasoningDelta {
			content.WriteString(delta.Text)
		}
	}
	body := strings.TrimSpace(content.String())
	if body == "" {
		return nil
	}

	return []timelineBlock{{
		kind: blockThinking, id: draftThinkingID, body: body,
		position: len(state.Transcript),
	}}
}

// withoutThinkingBlocks drops Thinking blocks. Inline mode hands history to the
// terminal's native scrollback, which renders no per-block styling, so the
// managed viewport is the only place reasoning is projected.
func withoutThinkingBlocks(blocks []timelineBlock) []timelineBlock {
	if len(blocks) == 0 {
		return blocks
	}

	filtered := make([]timelineBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.kind == blockThinking {
			continue
		}

		filtered = append(filtered, block)
	}

	return filtered
}

// Diagnostic identities the TUI treats specially by name.
const (
	diagnosticComponentChanges  = "changes"
	diagnosticCodeNotRepository = "not_repository"
)

// nonRepositoryDiagnostic reports the runtime diagnostic that says the workspace
// has no Git repository. It is a property of the workspace rather than a problem
// with the turn, and there is nothing for the reader to act on: direct
// apply_patch edits stay visible in Tool activity either way.
func nonRepositoryDiagnostic(diagnostic coding.IntegrationDiagnostic) bool {
	return diagnostic.Component == diagnosticComponentChanges &&
		diagnostic.Code == diagnosticCodeNotRepository
}

// visibleDiagnostics returns the integration diagnostics the TUI lists in its
// status view. The transcript projection applies the same filter inline, so a
// suppressed diagnostic cannot reappear on another surface.
func visibleDiagnostics(diagnostics []coding.IntegrationDiagnostic) []coding.IntegrationDiagnostic {
	visible := make([]coding.IntegrationDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if nonRepositoryDiagnostic(diagnostic) {
			continue
		}
		visible = append(visible, diagnostic)
	}

	return visible
}

// diagnosticTitle names the integration in plain words. The raw code stays a
// machine field and is never used as the body, so a message-less diagnostic
// still explains itself.
func diagnosticTitle(diagnostic coding.IntegrationDiagnostic) string {
	if title, ok := diagnosticTitles[diagnostic.Component+"\x00"+diagnostic.Code]; ok {
		return title
	}
	if title, ok := diagnosticComponentTitles[diagnostic.Component]; ok {
		if diagnostic.Code == "" {
			return title
		}

		return title + " · " + humanizeStatusCode(diagnostic.Code)
	}
	if diagnostic.Component == "" {
		return humanizeStatusCode(diagnostic.Code)
	}
	if diagnostic.Code == "" {
		return humanizeStatusCode(diagnostic.Component)
	}

	return humanizeStatusCode(diagnostic.Component) + " · " + humanizeStatusCode(diagnostic.Code)
}

// diagnosticBody keeps the message as the body and falls back to the humanized
// code only when the diagnostic carries no message of its own.
func diagnosticBody(diagnostic coding.IntegrationDiagnostic) string {
	if message := strings.TrimSpace(diagnostic.Message); message != "" {
		return message
	}
	if _, ok := diagnosticTitles[diagnostic.Component+"\x00"+diagnostic.Code]; ok &&
		diagnostic.Code != "" {
		return ""
	}

	return humanizeStatusCode(diagnostic.Code)
}

var diagnosticComponentTitles = map[string]string{
	"runtime":  "Agent runtime",
	"changes":  "Workspace changes",
	"observer": "Observer",
	"mcp":      "MCP",
	"hooks":    "Hooks",
	"team":     "Team",
}

var diagnosticTitles = map[string]string{
	"runtime\x00interaction_interrupted":      "Previous request was interrupted",
	"changes\x00capture_unstable":             "Workspace change summary unavailable",
	"mcp\x00refresh_failed":                   "MCP server list refresh failed",
	"mcp\x00connect_failed":                   "MCP server unavailable",
	"observer\x00observer_disabled":           "Event observer disabled",
	"observer\x00extension_observer_disabled": "Extension observer disabled",
	"observer\x00agent_observer_disabled":     "Agent observer disabled",
	"hooks\x00pending_trust":                  "Hooks are waiting for workspace trust",
}

func projectProtocolToolActivity(activity toolActivity) (timelineBlock, bool, bool) {
	switch activity.name {
	case planmode.EnterToolName, planmode.ExitToolName:
		block, visible := projectPlanModeToolActivity(activity)

		return block, true, visible
	case question.ToolName, question.TextToolName:
		block, visible := projectQuestionToolActivity(activity)

		return block, true, visible
	default:
		return timelineBlock{}, false, false
	}
}

// projectPlanModeToolActivity hides the model-facing plan tool result behind
// one semantic decision block. The enter call keeps its tool activity row.
func projectPlanModeToolActivity(activity toolActivity) (timelineBlock, bool) {
	if activity.name != planmode.ExitToolName || activity.state == toolStateRunning ||
		activity.state == toolStateInterrupted {
		return projectToolActivity(activity), true
	}
	title, ok := planModeDecisionTitle(activity.result)
	if !ok {
		return projectToolActivity(activity), true
	}

	return timelineBlock{
		kind: blockPlan, id: activity.id, title: title, position: activity.position,
	}, true
}

func planModeDecisionTitle(result string) (string, bool) {
	switch {
	case strings.HasPrefix(result, planmode.ExitApprovedResult),
		strings.HasPrefix(result, planmode.ExitApprovedEmptyResult):
		return planApprovedTitle, true
	case strings.HasPrefix(result, planmode.ExitReviseResult):
		return "Plan · Continue planning", true
	case strings.HasPrefix(result, planmode.ExitQuitResult):
		return "Plan · Abandoned", true
	default:
		return "", false
	}
}

func projectSubagentToolActivity(
	activity toolActivity,
	live []coding.SubagentState,
) timelineBlock {
	value, found := matchingSubagent(activity, live)
	if !found {
		value, found = projectDurableSubagent(activity)
	}
	if found {
		activity = enrichSubagentToolActivity(activity, value)
	}

	return projectToolActivity(activity)
}

func matchingSubagent(
	activity toolActivity,
	values []coding.SubagentState,
) (coding.SubagentState, bool) {
	for _, value := range values {
		if activity.id != "" && value.ParentToolCallID == activity.id {
			return value, true
		}
	}
	for _, value := range values {
		if value.ParentToolCallID == "" && activity.runID != "" &&
			value.ParentRunID == activity.runID {
			return value, true
		}
	}

	return coding.SubagentState{}, false
}

func enrichSubagentToolActivity(
	activity toolActivity,
	value coding.SubagentState,
) toolActivity {
	activity.class = toolClassSubagent
	activity.childSessionID = value.ChildSessionID
	activity.action = subagentActivityLabel(value.Role, value.State)
	activity.subject = oneLineSubagentTask(value.TaskPreview)
	activity.invocation = ""
	activity.update = ""
	activity.result = ""
	activity.body = ""
	activity.preview = []string{subagentMetadata(value)}

	switch value.State {
	case subagent.StateCreated, subagent.StateRunning:
		activity.state = toolStateRunning
	case subagent.StateSucceeded:
		activity.state = toolStateSucceeded
	case subagent.StateFailed:
		activity.state = toolStateFailed
	case subagent.StateCanceled, subagent.StateInterrupted:
		activity.state = toolStateInterrupted
	}

	return activity
}

func isSubagentToolName(name string) bool {
	return name == subagent.ToolName || name == subagent.SpawnToolName
}

func projectToolActivity(activity toolActivity) timelineBlock {
	return timelineBlock{
		kind: blockTool, id: activity.id, position: activity.position,
		tools: []toolActivity{activity},
	}
}

func projectQuestionToolActivity(activity toolActivity) (timelineBlock, bool) {
	if activity.name != question.ToolName && activity.name != question.TextToolName {
		return timelineBlock{}, false
	}
	if activity.state == toolStateRunning {
		return timelineBlock{}, false
	}

	block := timelineBlock{
		kind: blockQuestion, position: activity.position,
	}
	if (activity.state == toolStateFailed || activity.state == toolStateInterrupted) &&
		activity.result == question.RejectionToolResult {
		block.title = "Question canceled"
		block.body = "No answer was submitted."

		return block, true
	}
	if activity.state == toolStateFailed || activity.state == toolStateInterrupted {
		block.title = questionFailedTitle
		block.body = oneLineToolText(activity.result)
		if block.body == "" {
			block.body = "The structured question could not be opened."
		}

		return block, true
	}

	return projectAnsweredQuestionActivity(block, activity), true
}

func projectAnsweredQuestionActivity(
	block timelineBlock,
	activity toolActivity,
) timelineBlock {
	if activity.name == question.TextToolName {
		var result struct {
			Chat string `json:"chat"`
		}
		if json.Unmarshal([]byte(activity.result), &result) == nil && result.Chat != "" {
			block.title = "Answered question"
			block.body = result.Chat

			return block
		}

		block.title = questionAnsweredTitle
		block.body = "Free-form response submitted."

		return block
	}
	var spec question.Spec
	if err := json.Unmarshal(activity.arguments, &spec); err != nil ||
		question.ValidateSpec(spec) != nil {
		block.title = questionAnsweredTitle
		block.body = "Structured response submitted."

		return block
	}

	var result struct {
		Answers []question.Answer `json:"answers"`
		Chat    string            `json:"chat"`
	}
	if err := json.Unmarshal([]byte(activity.result), &result); err != nil {
		block.title = questionAnsweredTitle
		block.body = "Structured response submitted."

		return block
	}
	if result.Chat != "" {
		block.title = "Discussed question"
		block.body = result.Chat

		return block
	}
	if len(result.Answers) != len(spec.Questions) {
		block.title = questionAnsweredTitle
		block.body = "Structured response submitted."

		return block
	}

	lines := make([]string, 0, len(result.Answers))
	for index, answer := range result.Answers {
		value := answer.Custom
		if value == "" {
			value = strings.Join(answer.Selections, ", ")
		}
		if value == "" {
			value = "Answered"
		}
		lines = append(lines, spec.Questions[index].Header+": "+value)
	}
	block.title = "Answered questions"
	block.body = strings.Join(lines, "\n")

	return block
}

// isExploreBlock reports whether a block is an exploration tool card, which is
// the only kind consecutive blocks merge into one card.
func isExploreBlock(block timelineBlock) bool {
	return block.kind == blockTool && len(block.tools) > 0 &&
		block.tools[0].class == toolClassExplore
}

// isLoneExploreBlock reports whether a block is one single exploration call, the
// shape that may still absorb a following exploration block.
func isLoneExploreBlock(block timelineBlock) bool {
	return block.kind == blockTool && len(block.tools) == 1 &&
		block.tools[0].class == toolClassExplore
}

func groupExploreBlocks(blocks []timelineBlock) []timelineBlock {
	grouped := make([]timelineBlock, 0, len(blocks))
	for _, block := range blocks {
		if !isLoneExploreBlock(block) || len(grouped) == 0 {
			grouped = append(grouped, block)

			continue
		}

		previous := &grouped[len(grouped)-1]
		if !isExploreBlock(*previous) {
			grouped = append(grouped, block)

			continue
		}

		previous.tools = append(previous.tools, block.tools...)
		previous.id = block.id
	}

	return grouped
}

func oneLineSubagentTask(value string) string {
	return oneLineToolText(value)
}

func subagentMetadata(value coding.SubagentState) string {
	label := "Running"
	durationPrefix := " for "
	switch value.State {
	case subagent.StateSucceeded:
		label = "Completed"
		durationPrefix = " in "
	case subagent.StateFailed:
		label = labelFailed
		if value.Code != "" {
			label += ": " + humanizeStatusCode(value.Code)
		}
		durationPrefix = " after "
	case subagent.StateCanceled:
		label = "Canceled"
		durationPrefix = " after "
	case subagent.StateInterrupted:
		label = "Interrupted"
		durationPrefix = " after "
	case subagent.StateCreated, subagent.StateRunning:
		if activity := subagentActivitySummary(value.Activity); activity != "" {
			label = activity
		}
	}
	if value.DurationMillis > 0 {
		label += durationPrefix + formatInteractionDuration(value.DurationMillis)
	}

	facts := make([]string, 0, 2)
	if value.ToolCalls > 0 {
		facts = append(facts, fmt.Sprintf("%d tools", value.ToolCalls))
	}
	if tokens := value.Usage.InputTokens + value.Usage.OutputTokens; tokens > 0 {
		facts = append(facts, compactTokenCount(tokens)+" tokens")
	}

	if len(facts) == 0 {
		return label
	}

	return label + " · " + strings.Join(facts, " · ")
}

func subagentActivitySummary(value subagent.ActivitySummary) string {
	verb := ""
	switch value.Action {
	case subagent.ActivityActionRead:
		verb = "Read"
	case subagent.ActivityActionSearch:
		verb = "Search"
	case subagent.ActivityActionGlob:
		verb = "Glob"
	case subagent.ActivityActionList:
		verb = "List"
	case "":
		return ""
	default:
		return ""
	}
	if value.Target == "" {
		return verb
	}

	return verb + " " + value.Target
}

//nolint:gocyclo // The closed role/lifecycle matrix is the explicit product vocabulary.
func subagentActivityLabel(role subagent.Role, state subagent.State) string {
	running := state == subagent.StateCreated || state == subagent.StateRunning
	succeeded := state == subagent.StateSucceeded
	interrupted := state == subagent.StateCanceled || state == subagent.StateInterrupted

	switch role {
	case subagent.RoleExplore:
		switch {
		case running:
			return "Exploring"
		case succeeded:
			return "Explored"
		case interrupted:
			return "Explore interrupted"
		default:
			return "Explore failed"
		}
	case subagent.RolePlan:
		switch {
		case running:
			return "Planning"
		case succeeded:
			return "Planned"
		case interrupted:
			return "Plan interrupted"
		default:
			return "Plan failed"
		}
	case subagent.RoleReview:
		switch {
		case running:
			return "Reviewing"
		case succeeded:
			return "Reviewed"
		case interrupted:
			return "Review interrupted"
		default:
			return "Review failed"
		}
	default:
		return "Subagent"
	}
}

func humanizeStatusCode(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "_", " ")
}

func isTerminalSubagent(state subagent.State) bool {
	return state == subagent.StateSucceeded || state == subagent.StateFailed ||
		state == subagent.StateCanceled || state == subagent.StateInterrupted
}

func compactTokenCount(value int) string {
	if value < 1000 {
		return strconv.Itoa(value)
	}
	if value < 1000000 {
		return fmt.Sprintf("%.1fk", float64(value)/1000)
	}

	return fmt.Sprintf("%.1fm", float64(value)/1000000)
}

// appendMarker inserts one marker while keeping the list ordered by conversation
// position, which insertCompletionMarkers and splitCompletionMarkers rely on.
func appendMarker(markers []completionMarker, marker completionMarker) []completionMarker {
	index := len(markers)
	for index > 0 && markers[index-1].afterMessages > marker.afterMessages {
		index--
	}

	markers = append(markers, completionMarker{})
	copy(markers[index+1:], markers[index:len(markers)-1])
	markers[index] = marker

	return markers
}

func insertCompletionMarkers(
	blocks []timelineBlock,
	markers []completionMarker,
) []timelineBlock {
	if len(markers) == 0 {
		return blocks
	}

	result := make([]timelineBlock, 0, len(blocks)+len(markers))
	markerIndex := 0
	appendMarkersBefore := func(position int) {
		for markerIndex < len(markers) && markers[markerIndex].afterMessages < position {
			if block, ok := projectCompletionMarker(markers[markerIndex]); ok {
				result = append(result, block)
			}
			markerIndex++
		}
	}

	for _, block := range blocks {
		appendMarkersBefore(block.position)
		result = append(result, block)
	}
	for markerIndex < len(markers) {
		if block, ok := projectCompletionMarker(markers[markerIndex]); ok {
			result = append(result, block)
		}
		markerIndex++
	}

	return result
}

func projectCompletionMarker(marker completionMarker) (timelineBlock, bool) {
	if marker.notice != "" {
		return timelineBlock{
			kind:     blockDiagnostic,
			id:       "plan-notice:" + marker.interactionID,
			body:     marker.notice,
			position: marker.afterMessages,
		}, true
	}

	suffix := ""
	switch marker.outcome {
	case coding.InteractionSucceeded:
	case coding.InteractionCanceled:
		suffix = " · interrupted"
	case coding.InteractionFailed:
		suffix = " · failed"
	case coding.InteractionIncomplete:
		suffix = " · " + stopReasonPhrase(marker.stop)
	default:
		return timelineBlock{}, false
	}

	body := completionGlyph + " "
	if marker.model != "" {
		body += marker.model + " · "
	}
	body += formatInteractionDuration(marker.durationMillis) + suffix

	return timelineBlock{
		kind: blockCompletion, body: body, status: string(marker.outcome),
		position: marker.afterMessages,
	}, true
}

// stopReasonPhrase names why a run ended short in the user's vocabulary.
func stopReasonPhrase(reason agent.StopReason) string {
	switch reason {
	case agent.StopMaxTurns:
		return "turn limit reached"
	case agent.StopBudget:
		return "budget exhausted"
	case agent.StopWhen:
		return "stopped by rule"
	case agent.StopPaused:
		return "paused"
	case agent.StopTerminated:
		return "terminated"
	case agent.StopTruncated:
		return "output limit reached"
	case agent.StopEndTurn, "":
		return "incomplete"
	default:
		return humanizeStatusCode(string(reason))
	}
}

func formatInteractionDuration(milliseconds int64) string {
	if milliseconds <= 0 {
		return "0s"
	}
	if milliseconds < 1000 {
		return "<1s"
	}

	totalSeconds := milliseconds / 1000
	hours := totalSeconds / 3600
	minutes := totalSeconds % 3600 / 60
	seconds := totalSeconds % 60
	parts := make([]string, 0, 3)
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return strings.Join(parts, " ")
}

func visibleToolMessage(message ai.Message) string {
	return visibleToolParts(portableMessageParts(message))
}

func visibleToolParts(values []ai.Part) string {
	parts := make([]string, 0, len(values))
	for _, part := range values {
		switch part := part.(type) {
		case ai.TextPart:
			parts = append(parts, part.Text)
		case ai.ImagePart:
			parts = append(parts, "[image]")
		case ai.FilePart:
			parts = append(parts, "[file]")
		case ai.StructuredContentPart:
			// Structured payloads arrive as JSON objects; they are the only
			// content some MCP Tools return.
			parts = append(parts, string(part.Data))
		case ai.ResourceLinkPart:
			parts = append(parts, embeddedResourceLabel(part.Title, part.Name, part.URI))
		case ai.EmbeddedResourcePart:
			parts = append(parts, visibleEmbeddedResource(part))
		case ai.ToolResultPart:
			parts = append(parts, visibleToolParts(ai.ProviderParts(part.Content)))
		}
	}

	return strings.TrimSpace(strings.Join(parts, ""))
}

// visibleEmbeddedResource keeps inline text resources readable and names the
// binary ones without embedding their bytes.
func visibleEmbeddedResource(part ai.EmbeddedResourcePart) string {
	if part.Blob == nil {
		return part.Text
	}
	if strings.HasPrefix(strings.ToLower(part.MIMEType), "image/") {
		return "[image]"
	}

	return "[file]"
}

func embeddedResourceLabel(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}

	return "[resource]"
}

func visibleMessageText(message ai.Message) string {
	values := portableMessageParts(message)
	parts := make([]string, 0, len(values))
	for _, part := range values {
		switch part := part.(type) {
		case ai.TextPart:
			parts = append(parts, part.Text)
		case ai.ImagePart:
			parts = append(parts, "[image]")
		case ai.FilePart:
			label := strings.TrimSpace(part.Name)
			if label == "" {
				label = "file"
			}
			parts = append(parts, "["+label+"]")
		}
	}

	return strings.TrimSpace(strings.Join(parts, ""))
}

// portableMessageParts returns a message's parts for projection only. The
// projection never writes them, and it runs over the whole conversation on every
// cache miss, so it reads them in place instead of paying for a defensive copy:
// see [ai.MessagePartsView]. Anything that keeps or mutates the result must use
// [ai.MessageParts] instead.
func portableMessageParts(message ai.Message) []ai.Part {
	parts, err := ai.MessagePartsView(message)
	if err != nil {
		return nil
	}

	return parts
}

func visibleDraftText(deltas []coding.MessageDelta) string {
	var content strings.Builder
	for _, delta := range deltas {
		if delta.Kind == ai.StreamTextDelta {
			content.WriteString(delta.Text)
		}
	}

	return content.String()
}

func renderTimeline(
	blocks []timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	return renderTimelineContent(blocks, markdown, width, theme, noColor)
}

func renderTimelineContent(
	blocks []timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	return renderTimelineContentWithOptions(
		blocks,
		markdown,
		width,
		theme,
		noColor,
		timelineRenderOptions{},
	)
}

func renderTimelineContentWithOptions(
	blocks []timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
	options timelineRenderOptions,
) string {
	if len(blocks) == 0 {
		return ""
	}

	rendered := make([]string, 0, len(blocks))
	for _, block := range blocks {
		rendered = append(rendered, renderTimelineEntry(block, markdown, width, theme, noColor, options))
	}

	var content strings.Builder
	for index, value := range rendered {
		if index > 0 {
			content.WriteString(strings.Repeat("\n", conversationGapHeight+1))
		}
		content.WriteString(value)
	}

	return content.String()
}

// renderTimelineEntry renders one block exactly as the transcript renders it:
// user messages and completion markers have their own renderers, everything else
// goes through the shared block renderer. Per-record caching depends on this
// being the single definition of "how one entry looks".
//
// Conversation rows are inset to the Composer's text column; operator notices
// keep the full width because the banner is a bordered box that lines up with
// the Composer box rather than with the conversation inside it, and the printed
// reports are the same text inline mode hands to the terminal.
func renderTimelineEntry(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
	options timelineRenderOptions,
) string {
	if block.notice {
		return renderTimelineEntryCore(block, markdown, width, theme, noColor, options)
	}

	inset := timelineInset(width)

	return insetRows(
		renderTimelineEntryCore(block, markdown, max(1, width-2*inset), theme, noColor, options),
		inset,
	)
}

// renderTimelineEntryCore renders one block at the width it owns.
func renderTimelineEntryCore(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
	options timelineRenderOptions,
) string {
	if block.kind == blockUser {
		return renderUserMessage(block.body, width, theme, noColor)
	}
	if block.kind == blockCompletion {
		return renderCompletionMarker(block, theme, noColor)
	}

	return renderTimelineBlockWithOptions(block, markdown, width, theme, noColor, options)
}

// insetRows moves every rendered row in from the frame edge, which is what lines
// a row up with the Composer's text column. Blank rows stay empty instead of
// carrying trailing whitespace.
func insetRows(content string, inset int) string {
	if inset <= 0 || content == "" {
		return content
	}

	prefix := strings.Repeat(" ", inset)
	rows := strings.Split(content, "\n")
	for index, row := range rows {
		if row == "" {
			continue
		}

		rows[index] = prefix + row
	}

	return strings.Join(rows, "\n")
}

func renderTimelineBlock(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	return renderTimelineBlockWithOptions(
		block,
		markdown,
		width,
		theme,
		noColor,
		timelineRenderOptions{},
	)
}

func renderTimelineBlockWithOptions(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
	options timelineRenderOptions,
) string {
	if block.kind == blockTool {
		if options.expandToolResults {
			return renderExpandedToolActivityBlock(block, width, theme, noColor)
		}

		return renderToolActivityBlock(block, width, theme, noColor)
	}
	if block.kind == blockTeam {
		return renderTeamActivityBlock(block, width, theme, noColor)
	}
	if block.kind == blockChange {
		return renderWorkspaceChangeBlock(block, width, theme, noColor)
	}
	if block.kind == blockThinking {
		return renderThinkingBlock(block, markdown, width, theme, noColor)
	}
	if block.kind == blockIncomplete {
		return renderIncompleteReplyBlock(block, width, theme, noColor)
	}

	return renderRegularTimelineBlock(block, markdown, width, theme, noColor)
}

// renderIncompleteReplyBlock renders an abandoned provisional answer as plain
// wrapped prose under an error-toned label. It deliberately skips the Markdown
// engine: the retained text is a fragment, and rendering it as Markdown would
// give it the same look as a committed answer.
func renderIncompleteReplyBlock(block timelineBlock, width int, theme colorTheme, noColor bool) string {
	width = max(1, width)
	body := wrapPlainText(strings.TrimSpace(block.body), width)
	title := block.title
	if title != "" && !noColor {
		title = timelineTitleStyle(blockIncomplete, theme).Render(title)
	}
	if body == "" {
		return title
	}
	if title == "" {
		return body
	}

	return title + "\n" + body
}

// renderThinkingBlock renders one visible reasoning section as plain, dimmed
// prose led by the section glyph. The opaque Signature never reaches this
// function; only ReasoningPart.Text does.
//
// Reasoning is deliberately not run through the Markdown renderer. A live section
// is re-rendered on every streamed delta, and parsing it as Markdown allocates
// tens of thousands of times per render, so a long thought turns each frame into
// a CPU spike. Wrapping the same text costs a few hundred microseconds and a
// handful of allocations, and the reader sees it in one uniform color either way.
//
// A live section still grows, so it is assembled from the rows frozen at its last
// line break instead of re-wrapping and re-styling the whole thought; see
// [thinking_live.go]. A settled section never changes and is rendered once.
func renderThinkingBlock(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	body := sanitizeToolText(block.body)
	if body == "" {
		return ""
	}

	width = max(1, width)
	prefix := thinkingGlyph + " "
	prefixWidth := ansi.StringWidth(prefix)

	if width <= prefixWidth {
		// A frame this narrow has no room beside the glyph; keep the prose.
		content := ansi.Wrap(body, width, "")

		return styleThinkingRows(content, theme, noColor)
	}

	indent := strings.Repeat(" ", prefixWidth)
	limit := width - prefixWidth
	if markdown != nil && blockIsLive(block) {
		return markdown.renderLiveThinking(body, prefix, indent, limit, theme, noColor)
	}

	rows := thinkingRows(body, prefix, indent, limit, false)

	return styleThinkingRows(strings.Join(rows, "\n"), theme, noColor)
}

// styleThinkingRows applies the section's dim colour and the alignment lipgloss
// gives a rendered block.
func styleThinkingRows(content string, theme colorTheme, noColor bool) string {
	if noColor {
		return content
	}

	return lipgloss.NewStyle().Foreground(paletteFor(theme).muted).Render(content)
}

// renderMarkdownBlockBody renders one Markdown body, sharing live-version
// ownership between the streaming answer and its settled record while preserving
// the original text on a render error.
func renderMarkdownBlockBody(block timelineBlock, markdown *markdownRenderer, width int, theme colorTheme, noColor bool) string {
	if block.rendered {
		return block.body
	}

	var value string
	var err error
	if blockIsLive(block) {
		value, err = markdown.renderLive(kindName(block.kind), block.body, max(1, width), theme, noColor)
	} else {
		value, err = markdown.render(block.body, max(1, width), theme, noColor)
	}
	if err != nil {
		return block.body
	}

	return value
}

// markdownBlockBodyRows is [renderMarkdownBlockBody] for the managed transcript
// store, which consumes rows. A live answer is assembled as rows directly, so a
// frame does not join the whole frozen prefix into one string only to split it
// again. ok is false when the body must take the string path.
func markdownBlockBodyRows(block timelineBlock, markdown *markdownRenderer, width, inset int, theme colorTheme, noColor bool) (rowSegments, bool) {
	if block.rendered || markdown == nil || !blockIsLive(block) {
		return rowSegments{}, false
	}

	rows, err := markdown.renderLiveRows(kindName(block.kind), block.body, max(1, width), inset, theme, noColor)
	if err != nil {
		return rowSegments{}, false
	}

	return rows, true
}

// insetRowSlice is [insetRows] over rows that are already split, so a renderer
// that produces rows never has to join them into a string first.
func insetRowSlice(rows []string, inset int) []string {
	if inset <= 0 || len(rows) == 0 {
		return rows
	}

	prefix := strings.Repeat(" ", inset)
	insetRows := make([]string, len(rows))
	for index, row := range rows {
		if row == "" {
			continue
		}

		insetRows[index] = prefix + row
	}

	return insetRows
}

// thinkingBlockRows is the managed store's row path for the live Thinking block:
// it returns the frozen rows and the tail with the glyph, the dim colour, the
// block padding and the frame inset already applied. ok is false when the block
// must take the string path.
func thinkingBlockRows(block timelineBlock, markdown *markdownRenderer, width, inset int, theme colorTheme, noColor bool) (rowSegments, bool) {
	if markdown == nil || !blockIsLive(block) {
		return rowSegments{}, false
	}

	body := sanitizeToolText(block.body)
	if body == "" {
		return rowSegments{}, true
	}

	width = max(1, width)
	prefix := thinkingGlyph + " "
	prefixWidth := ansi.StringWidth(prefix)
	if width <= prefixWidth {
		// A frame this narrow has no room beside the glyph; keep the prose.
		return rowSegments{}, false
	}

	indent := strings.Repeat(" ", prefixWidth)

	return markdown.renderLiveThinkingRows(body, prefix, indent, width-prefixWidth, inset, theme, noColor), true
}

// liveEntryRows returns the rows of a live block for the managed transcript
// store, as a frozen prefix and a tail with the frame inset already applied, so
// the store never joins or re-insets the whole live body. ok is false for every
// block that must take the string path, and the caller falls back to
// [renderTimelineEntry].
func liveEntryRows(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) (rowSegments, bool) {
	if block.notice || block.title != "" {
		return rowSegments{}, false
	}

	inset := timelineInset(width)
	contentWidth := max(1, width-2*inset)

	var (
		rows rowSegments
		ok   bool
	)

	switch block.kind {
	case blockDraft, blockAssistant, blockPlan:
		rows, ok = markdownBlockBodyRows(block, markdown, contentWidth, inset, theme, noColor)
	case blockThinking:
		rows, ok = thinkingBlockRows(block, markdown, contentWidth, inset, theme, noColor)
	default:
		return rowSegments{}, false
	}
	if !ok {
		return rowSegments{}, false
	}

	// A body that renders as nothing returns the title, which is empty here.
	if rowSegmentsBlank(rows) {
		return rowSegments{}, true
	}

	return rows, true
}

// rowSegmentsBlank reports whether every row of both segments is blank.
func rowSegmentsBlank(rows rowSegments) bool {
	for _, row := range rows.frozen {
		if strings.TrimSpace(row) != "" {
			return false
		}
	}

	for _, row := range rows.tail {
		if strings.TrimSpace(row) != "" {
			return false
		}
	}

	return true
}

// renderTeamActivityBlock renders one Team activity line.
func renderTeamActivityBlock(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	content := strings.TrimSpace(block.title)
	if body := strings.TrimSpace(block.body); body != "" {
		if content != "" {
			content += " · "
		}
		content += body
	}
	content = ansi.Truncate(content, max(1, width), "…")
	if noColor {
		return content
	}

	return timelineTitleStyle(blockTeam, theme).Render(content)
}

func renderRegularTimelineBlock(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	if block.kind == blockError {
		return renderErrorBlock(block, width, theme, noColor)
	}

	body := block.body
	if block.kind == blockAssistant || block.kind == blockDraft || block.kind == blockPlan {
		body = renderMarkdownBlockBody(block, markdown, width, theme, noColor)
	}

	title := block.title
	if block.kind == blockDiagnostic {
		// Diagnostic titles already carry their human wording; the raw code
		// never becomes a second, repeated line.
		body = wrapPlainText(body, width)
		title = wrapPlainText(title, width)
	}
	if title != "" && !noColor {
		style := timelineTitleStyle(block.kind, theme)
		title = style.Render(title)
	}
	if strings.TrimSpace(body) == "" {
		return title
	}
	if title == "" {
		return body
	}

	return title + "\n" + body
}

// wrapPlainText wraps a non-Markdown block body so it never runs past the
// frame width, where the terminal would cut it without an ellipsis.
func wrapPlainText(value string, width int) string {
	value = strings.TrimRight(value, " ")
	if value == "" || width < 1 {
		return value
	}

	return strings.TrimRight(lipgloss.Wrap(value, width, ""), "\n")
}

func renderUserMessage(body string, width int, theme colorTheme, noColor bool) string {
	width = max(1, width)
	body = ansi.Strip(body)
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	content := inputArrow
	if width > inputPromptWidth {
		lines := strings.Split(lipgloss.Wrap(body, width-inputPromptWidth, ""), "\n")
		for index := range lines {
			prefix := strings.Repeat(" ", inputPromptWidth)
			if index == 0 {
				prefix = inputArrow + " "
			}
			lines[index] = prefix + lines[index]
		}
		content = strings.Join(lines, "\n")
	} else if body != "" {
		content += "\n" + lipgloss.Wrap(body, width, "")
	}
	if noColor {
		return content
	}

	return lipgloss.NewStyle().
		Foreground(paletteFor(theme).workspace).
		Render(content)
}

func renderErrorBlock(block timelineBlock, width int, theme colorTheme, noColor bool) string {
	width = max(1, width)
	body := strings.TrimSpace(block.body)
	if block.status != "" && body == "" {
		// The code only stands in when there is no message of its own.
		body = humanizeStatusCode(block.status)
	}
	if block.title != "" {
		if body == "" {
			body = block.title
		} else {
			body = block.title + "\n" + body
		}
	}

	barWidth := ansi.StringWidth(errorAccentBar) + 1
	if width > barWidth {
		body = lipgloss.Wrap(body, width-barWidth, "")
	}

	lines := strings.Split(body, "\n")
	if noColor {
		for index := range lines {
			lines[index] = strings.TrimRight(errorAccentBar+" "+lines[index], " ")
		}

		return strings.Join(lines, "\n")
	}

	palette := paletteFor(theme)
	bar := lipgloss.NewStyle().Foreground(palette.error).Render(errorAccentBar)
	text := lipgloss.NewStyle().Foreground(palette.muted)
	for index := range lines {
		if lines[index] == "" {
			lines[index] = bar

			continue
		}
		lines[index] = bar + " " + text.Render(lines[index])
	}

	return strings.Join(lines, "\n")
}

func renderCompletionMarker(block timelineBlock, theme colorTheme, noColor bool) string {
	if noColor {
		return block.body
	}

	glyph, rest, found := strings.Cut(block.body, " ")
	if !found {
		return completionGlyphStyle(block.status, theme).Render(block.body)
	}

	muted := lipgloss.NewStyle().Foreground(paletteFor(theme).muted)

	return completionGlyphStyle(block.status, theme).Render(glyph) + " " + muted.Render(rest)
}

func completionGlyphStyle(status string, theme colorTheme) lipgloss.Style {
	palette := paletteFor(theme)
	color := palette.muted
	switch coding.InteractionOutcome(status) {
	case coding.InteractionSucceeded:
		color = palette.idle
	case coding.InteractionCanceled:
		color = palette.muted
	case coding.InteractionFailed:
		color = palette.error
	case coding.InteractionIncomplete:
		color = palette.error
	}

	return lipgloss.NewStyle().Foreground(color)
}

func timelineTitleStyle(kind blockKind, theme colorTheme) lipgloss.Style {
	palette := paletteFor(theme)
	color := palette.diagnostic
	switch kind {
	case blockUser, blockQuestion:
		color = palette.session
	case blockAssistant, blockDraft, blockPlan:
		color = palette.model
	case blockTool, blockTeam:
		color = palette.idle
	case blockChange:
		color = palette.change
	case blockError:
		color = palette.error
	case blockCompletion:
		color = palette.muted
	case blockDiagnostic:
		color = palette.diagnostic
	case blockIncomplete:
		color = palette.error
	}

	return lipgloss.NewStyle().Bold(true).Foreground(color)
}
