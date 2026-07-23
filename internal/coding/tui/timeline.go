//nolint:wsl_v5 // Projection branches keep title/body assembly adjacent.
package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockDraft
	blockTool
	blockDiagnostic
	blockChange
	blockError
	blockCompletion
	blockSubagent
)

type timelineBlock struct {
	kind     blockKind
	id       string
	title    string
	body     string
	meta     string
	status   string
	position int
	rendered bool
	tools    []toolActivity
}

type completionMarker struct {
	interactionID  string
	afterMessages  int
	outcome        coding.InteractionOutcome
	durationMillis int64
}

func projectTimeline(state coding.State) []timelineBlock {
	return projectTimelineExcludingTools(state, nil)
}

// projectTimelineExcludingTools is used by the append-only scrollback path to
// suppress transcript Tool results whose compact card was already committed.
//
//nolint:gocyclo // One projection pass makes every public Coding event visibly exhaustive.
func projectTimelineExcludingTools(
	state coding.State,
	excluded map[string]struct{},
) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(state.Transcript)+len(state.Tools)+len(state.Subagents)+4)
	activities := projectToolActivities(state, excluded)
	activityIndex := 0
	for messageIndex, message := range state.Transcript {
		position := messageIndex + 1
		body := visibleMessageText(message)
		if body != "" {
			switch message.Role {
			case ai.RoleUser:
				blocks = append(blocks, timelineBlock{
					kind: blockUser, body: body, position: position,
				})
			case ai.RoleAssistant:
				blocks = append(blocks, timelineBlock{
					kind: blockAssistant, body: body, position: position,
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
			if activity.name == subagent.ToolName {
				if block, ok := projectSubagentToolActivity(activity, state.Subagents); ok {
					blocks = append(blocks, block)
				}
				continue
			}
			blocks = append(blocks, projectToolActivity(activity))
		}
	}

	for activityIndex < len(activities) {
		activity := activities[activityIndex]
		activityIndex++
		if activity.name == subagent.ToolName {
			if block, ok := projectSubagentToolActivity(activity, state.Subagents); ok {
				blocks = append(blocks, block)
			}
			continue
		}
		blocks = append(blocks, projectToolActivity(activity))
	}

	for _, child := range state.Subagents {
		block := projectSubagent(child, len(state.Transcript))
		for _, activity := range activities {
			if activity.name == subagent.ToolName && activity.runID == child.ParentRunID {
				block.tools = []toolActivity{activity}

				break
			}
		}
		blocks = append(blocks, block)
	}

	draft := visibleDraftText(state.Draft)
	if draft != "" {
		blocks = append(blocks, timelineBlock{
			kind: blockDraft, body: draft,
			position: len(state.Transcript),
		})
	}

	if state.Changes != nil {
		summary := fmt.Sprintf("%d workspace change(s)", len(state.Changes.Entries))
		if state.Changes.Truncated {
			summary += " · diff truncated"
		}
		blocks = append(blocks, timelineBlock{
			kind: blockChange, title: "Changes", body: summary,
			position: len(state.Transcript),
		})
	}

	for _, diagnostic := range state.Diagnostics {
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
			kind: blockError, title: "Error", body: state.LastError.Message,
			status: state.LastError.Code, position: len(state.Transcript),
		})
	}

	return groupExploreBlocks(blocks)
}

func projectSubagentToolActivity(
	activity toolActivity,
	live []coding.SubagentState,
) (timelineBlock, bool) {
	for _, child := range live {
		if activity.runID != "" && child.ParentRunID == activity.runID {
			return timelineBlock{}, false
		}
	}

	value, ok := projectDurableSubagent(activity)
	if !ok {
		return timelineBlock{}, false
	}
	for _, child := range live {
		if value.ChildSessionID != "" && child.ChildSessionID == value.ChildSessionID {
			return timelineBlock{}, false
		}
	}

	block := projectSubagent(value, activity.position)
	block.tools = []toolActivity{activity}

	return block, true
}

func projectToolActivity(activity toolActivity) timelineBlock {
	return timelineBlock{
		kind: blockTool, id: activity.id, position: activity.position,
		tools: []toolActivity{activity},
	}
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

func projectSubagent(value coding.SubagentState, position int) timelineBlock {
	return timelineBlock{
		kind: blockSubagent, id: value.ChildSessionID,
		title: subagentActivityLabel(value.Role, value.State), body: oneLineSubagentTask(value.TaskPreview),
		meta: subagentMetadata(value), status: string(value.State), position: position,
	}
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

func subagentStateGlyph(state subagent.State) string {
	switch state {
	case subagent.StateSucceeded:
		return "•"
	case subagent.StateFailed:
		return "✗"
	case subagent.StateCanceled, subagent.StateInterrupted:
		return "!"
	case subagent.StateCreated, subagent.StateRunning:
		return "✻"
	default:
		return "·"
	}
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
	duration := formatInteractionDuration(marker.durationMillis)
	body := ""
	switch marker.outcome {
	case coding.InteractionSucceeded:
		body = "[✻ Worked for " + duration + "]"
	case coding.InteractionCanceled:
		body = "[Interrupted after " + duration + "]"
	case coding.InteractionFailed:
		body = "[Failed after " + duration + "]"
	default:
		return timelineBlock{}, false
	}

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
			content := block.body
			if !noColor {
				content = completionStyle(block.status, theme).Render(content)
			}
			rendered = append(rendered, content)

			continue
		}

		rendered = append(rendered, renderTimelineBlock(block, markdown, width, theme, noColor))
	}

	separator := strings.Repeat("\n", conversationGapHeight+1)

	return strings.Join(rendered, separator)
}

func renderTimelineBlock(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	if block.kind == blockSubagent {
		return renderSubagentBlock(block, width, theme, noColor)
	}
	if block.kind == blockTool {
		return renderToolActivityBlock(block, width, theme, noColor)
	}

	return renderRegularTimelineBlock(block, markdown, width, theme, noColor)
}

func renderRegularTimelineBlock(
	block timelineBlock,
	markdown *markdownRenderer,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	body := block.body
	if !block.rendered && (block.kind == blockAssistant || block.kind == blockDraft) {
		if value, err := markdown.render(body, max(1, width-2), theme, noColor); err == nil {
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

func renderSubagentBlock(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	width = max(1, width)
	prefix := subagentStateGlyph(subagent.State(block.status)) + " " + block.title
	heading := prefix
	if block.body != "" {
		heading += " · " + block.body
	}
	metadata := "  " + block.meta
	if noColor {
		return ansi.Truncate(heading, width, "…") + "\n" + ansi.Truncate(metadata, width, "…")
	}

	palette := paletteFor(theme)
	heading = subagentTitleStyle(block.status, theme).Render(prefix)
	if block.body != "" {
		heading += lipgloss.NewStyle().Foreground(palette.workspace).Render(" · " + block.body)
	}
	metadata = lipgloss.NewStyle().Foreground(palette.muted).Render(metadata)

	return ansi.Truncate(heading, width, "…") + "\n" + ansi.Truncate(metadata, width, "…")
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

	return userMessageStyle(theme).
		Width(width).
		Render(content)
}

func userMessageStyle(theme colorTheme) lipgloss.Style {
	palette := paletteFor(theme)

	return lipgloss.NewStyle().
		Foreground(palette.userMessageText).
		Background(palette.userMessageBackground)
}

func completionStyle(status string, theme colorTheme) lipgloss.Style {
	palette := paletteFor(theme)
	color := palette.muted
	switch coding.InteractionOutcome(status) {
	case coding.InteractionSucceeded:
		color = palette.idle
	case coding.InteractionCanceled:
		color = palette.warning
	case coding.InteractionFailed:
		color = palette.error
	}

	return lipgloss.NewStyle().Foreground(color)
}

func timelineTitleStyle(kind blockKind, theme colorTheme) lipgloss.Style {
	color := "#7D8B99"
	switch kind {
	case blockUser:
		color = "#5FAFFF"
	case blockAssistant, blockDraft:
		color = "#AF87FF"
	case blockTool:
		color = "#5FD7AF"
	case blockChange:
		color = "#87D75F"
	case blockError:
		color = "#FF5F5F"
	case blockCompletion:
	case blockSubagent:
		color = "#AF87FF"
		if theme == themeLight {
			color = "#8250DF"
		}
	case blockDiagnostic:
		if theme == themeLight {
			color = "#586069"
		}
	}

	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color))
}

func subagentTitleStyle(status string, theme colorTheme) lipgloss.Style {
	palette := paletteFor(theme)
	color := palette.model
	switch subagent.State(status) {
	case subagent.StateCreated, subagent.StateRunning:
	case subagent.StateSucceeded:
		color = palette.idle
	case subagent.StateFailed:
		color = palette.error
	case subagent.StateCanceled, subagent.StateInterrupted:
		color = palette.warning
	}

	return lipgloss.NewStyle().Bold(true).Foreground(color)
}
