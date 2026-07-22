//nolint:wsl_v5 // Projection branches keep title/body assembly adjacent.
package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
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
)

type timelineBlock struct {
	kind     blockKind
	id       string
	title    string
	body     string
	status   string
	position int
}

type completionMarker struct {
	interactionID  string
	afterMessages  int
	outcome        coding.InteractionOutcome
	durationMillis int64
}

//nolint:gocyclo // One projection pass makes every public Coding event visibly exhaustive.
func projectTimeline(state coding.State) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(state.Transcript)+len(state.Tools)+4)
	tools := make(map[string]coding.ToolState, len(state.Tools))
	for _, tool := range state.Tools {
		tools[tool.Call.ID] = tool
	}
	projectedTools := make(map[string]struct{}, len(state.Tools))

	for messageIndex, message := range state.Transcript {
		position := messageIndex + 1
		if message.Role == ai.RoleTool {
			for _, part := range message.Parts {
				result, ok := part.(ai.ToolResultPart)
				if !ok {
					continue
				}
				tool, exists := tools[result.ToolCallID]
				if !exists {
					continue
				}
				block := projectTool(tool)
				block.position = position
				blocks = append(blocks, block)
				projectedTools[result.ToolCallID] = struct{}{}
			}

			continue
		}

		body := visibleMessageText(message)
		if body == "" {
			continue
		}

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

	for _, tool := range state.Tools {
		if _, exists := projectedTools[tool.Call.ID]; exists {
			continue
		}
		block := projectTool(tool)
		block.position = len(state.Transcript)
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

	return blocks
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

func projectTool(tool coding.ToolState) timelineBlock {
	return timelineBlock{
		kind:   blockTool,
		id:     tool.Call.ID,
		title:  tool.Call.Name,
		status: string(tool.Status),
	}
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
	body := block.body
	if block.kind == blockAssistant || block.kind == blockDraft {
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
		title = timelineTitleStyle(block.kind, theme).Render(title)
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
	case blockDiagnostic:
		if theme == themeLight {
			color = "#586069"
		}
	}

	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color))
}
