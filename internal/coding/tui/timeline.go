//nolint:wsl_v5 // Projection branches keep title/body assembly adjacent.
package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/subagent"
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
)

const (
	completionGlyph       = "▣"
	errorAccentBar        = "▌"
	questionAnsweredTitle = "Question answered"
	questionFailedTitle   = "Question failed"
)

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
}

type timelineRenderOptions struct {
	expandToolResults bool
}

type completionMarker struct {
	interactionID  string
	afterMessages  int
	outcome        coding.InteractionOutcome
	stop           agent.StopReason
	durationMillis int64
	model          string
}

func projectTimeline(state coding.State) []timelineBlock {
	return projectTimelineExcluding(state, nil)
}

//nolint:gocyclo,cyclop // One pass keeps messages, Tools, and child identities in durable order.
func projectTimelineExcluding(
	state coding.State,
	excludedTools map[string]struct{},
) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(state.Transcript)+len(state.Tools)+4)
	activities := projectToolActivities(state, excludedTools)
	syntheticMessages := make(map[int]struct{}, len(state.SyntheticMessages))
	for _, index := range state.SyntheticMessages {
		syntheticMessages[index] = struct{}{}
	}
	activityIndex := 0
	for messageIndex, message := range state.Transcript {
		position := messageIndex + 1
		body := visibleMessageText(message)
		candidateID := ""
		if messageIndex < len(state.MessageCandidates) {
			candidateID = state.MessageCandidates[messageIndex].Key()
		}
		_, synthetic := syntheticMessages[messageIndex]
		if body != "" && !synthetic {
			switch message.Role {
			case ai.RoleUser:
				blocks = append(blocks, timelineBlock{
					kind: blockUser, body: body, position: position,
				})
			case ai.RoleAssistant:
				blocks = append(blocks, timelineBlock{
					kind: blockAssistant, id: candidateID, body: body, position: position,
				})
			case ai.RoleSystem:
				blocks = append(blocks, timelineBlock{
					kind: blockDiagnostic, title: "System", body: body, position: position,
				})
			case ai.RoleTool:
			}
		}

		for activityIndex < len(activities) && activities[activityIndex].position <= position {
			activity := activities[activityIndex]
			activityIndex++
			if block, handled, visible := projectProtocolToolActivity(activity, state.PlanProposals); handled {
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
	}

	for activityIndex < len(activities) {
		activity := activities[activityIndex]
		activityIndex++
		if block, handled, visible := projectProtocolToolActivity(activity, state.PlanProposals); handled {
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
		if diagnostic.Component == "changes" && diagnostic.Code == "not_repository" {
			blocks = append(blocks, timelineBlock{
				kind:  blockDiagnostic,
				title: "Git change summary unavailable",
				body: "This workspace is not a Git repository. Direct apply_patch edits remain visible " +
					"in their tool activity; repository-wide shell and generator changes cannot be attributed.",
				position: len(state.Transcript),
			})
			continue
		}
		message := strings.TrimSpace(diagnostic.Message)
		if message == "" {
			message = diagnostic.Code
		}
		blocks = append(blocks, timelineBlock{
			kind:     blockDiagnostic,
			title:    diagnostic.Component,
			body:     message,
			status:   diagnostic.Code,
			position: len(state.Transcript),
		})
	}

	if state.LastError != nil && state.Interaction.Outcome != coding.InteractionCanceled {
		blocks = append(blocks, timelineBlock{
			kind: blockError, body: state.LastError.Message,
			status: state.LastError.Code, position: len(state.Transcript),
		})
	}

	return groupExploreBlocks(blocks)
}

func projectProtocolToolActivity(
	activity toolActivity,
	proposals []coding.PlanProposal,
) (timelineBlock, bool, bool) {
	switch activity.name {
	case planreview.ToolName, planreview.PresentToolName:
		block, visible := projectPlanReviewToolActivity(activity, proposals)
		return block, true, visible
	case question.ToolName, question.TextToolName:
		block, visible := projectQuestionToolActivity(activity)
		return block, true, visible
	default:
		return timelineBlock{}, false, false
	}
}

func projectPlanReviewToolActivity(
	activity toolActivity,
	proposals []coding.PlanProposal,
) (timelineBlock, bool) {
	if activity.name != planreview.ToolName && activity.name != planreview.PresentToolName {
		return timelineBlock{}, false
	}
	if activity.state == toolStateRunning {
		return timelineBlock{}, false
	}
	if activity.name == planreview.PresentToolName {
		for _, proposal := range proposals {
			if proposal.ToolCallID != activity.id || proposal.Status == coding.PlanProposalPending {
				continue
			}

			title := "Plan · Continue planning"
			if proposal.Status == coding.PlanProposalApproved {
				title = "Plan · Approved"
			}

			return timelineBlock{
				kind: blockPlan, id: proposal.ID, title: title, body: proposal.Content,
				position: activity.position,
			}, true
		}

		return timelineBlock{}, false
	}

	block := timelineBlock{kind: blockQuestion, position: activity.position}
	if activity.state == toolStateFailed || activity.state == toolStateInterrupted {
		block.title = "Plan submission failed"
		block.body = oneLineToolText(activity.result)
		if block.body == "" {
			block.body = "The submitted Plan could not be reviewed."
		}

		return block, true
	}
	if strings.HasPrefix(activity.result, planreview.ApprovalToolResult) {
		block.title = "Plan approved"
		block.body = "The exact submitted revision was approved for an idle switch to Agent Mode."

		return block, true
	}

	block.title = "Planning continued"
	block.body = "The Plan was returned for another revision."

	return block, true
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

func groupExploreBlocks(blocks []timelineBlock) []timelineBlock {
	grouped := make([]timelineBlock, 0, len(blocks))
	for _, block := range blocks {
		isExplore := block.kind == blockTool && len(block.tools) == 1 &&
			block.tools[0].class == toolClassExplore
		if !isExplore || len(grouped) == 0 {
			grouped = append(grouped, block)

			continue
		}

		previous := &grouped[len(grouped)-1]
		if previous.kind != blockTool || len(previous.tools) == 0 ||
			previous.tools[0].class != toolClassExplore {
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
		label = "Failed"
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
	suffix := ""
	switch marker.outcome {
	case coding.InteractionSucceeded:
	case coding.InteractionCanceled:
		suffix = " · interrupted"
	case coding.InteractionFailed:
		suffix = " · failed"
	case coding.InteractionIncomplete:
		suffix = " · incomplete (" + string(marker.stop) + ")"
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
	parts := make([]string, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part := part.(type) {
		case ai.TextPart:
			parts = append(parts, part.Text)
		case ai.ImagePart:
			parts = append(parts, "[image]")
		case ai.FilePart:
			parts = append(parts, "[file]")
		case ai.ToolResultPart:
			parts = append(parts, visibleToolMessage(ai.Message{Parts: part.Content}))
		}
	}

	return strings.TrimSpace(strings.Join(parts, ""))
}

func visibleMessageText(message ai.Message) string {
	parts := make([]string, 0, len(message.Parts))
	for _, part := range message.Parts {
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
		if block.kind == blockUser {
			rendered = append(rendered, renderUserMessage(block.body, width, theme, noColor))

			continue
		}

		if block.kind == blockCompletion {
			rendered = append(rendered, renderCompletionMarker(block, theme, noColor))

			continue
		}

		rendered = append(rendered, renderTimelineBlockWithOptions(
			block,
			markdown,
			width,
			theme,
			noColor,
			options,
		))
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

	return renderRegularTimelineBlock(block, markdown, width, theme, noColor)
}

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
	if !block.rendered && (block.kind == blockAssistant || block.kind == blockDraft || block.kind == blockPlan) {
		if value, err := markdown.render(body, max(1, width), theme, noColor); err == nil {
			body = value
		}
	}

	title := block.title
	if block.status != "" {
		if title != "" {
			title += " · "
		}
		title += block.status
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
	if block.status != "" {
		first, rest, wrapped := strings.Cut(body, "\n")
		if first == "" {
			first = block.status
		} else {
			first += " (" + block.status + ")"
		}
		body = first
		if wrapped {
			body += "\n" + rest
		}
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
	color := "#7D8B99"
	switch kind {
	case blockUser:
		color = "#5FAFFF"
	case blockAssistant, blockDraft, blockPlan:
		color = "#AF87FF"
	case blockTool, blockTeam:
		color = "#5FD7AF"
	case blockQuestion:
		color = "#5FAFFF"
	case blockChange:
		color = "#87D75F"
	case blockError:
		color = "#FF5F5F"
	case blockCompletion:
	case blockDiagnostic:
		if theme == themeLight {
			color = "#586069"
		}
	}

	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color))
}
