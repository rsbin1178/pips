//nolint:wsl_v5 // Projection branches keep title/body assembly adjacent.
package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
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
)

type timelineBlock struct {
	kind   blockKind
	id     string
	title  string
	body   string
	status string
}

//nolint:gocyclo // One projection pass makes every public Coding event visibly exhaustive.
func projectTimeline(state coding.State) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(state.Transcript)+len(state.Tools)+4)
	tools := make(map[string]coding.ToolState, len(state.Tools))
	for _, tool := range state.Tools {
		tools[tool.Call.ID] = tool
	}
	projectedTools := make(map[string]struct{}, len(state.Tools))

	for _, message := range state.Transcript {
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
				blocks = append(blocks, projectTool(tool))
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
			blocks = append(blocks, timelineBlock{kind: blockUser, title: "You", body: body})
		case ai.RoleAssistant:
			blocks = append(blocks, timelineBlock{
				kind: blockAssistant, title: appTitle, body: body,
			})
		case ai.RoleSystem:
			blocks = append(blocks, timelineBlock{
				kind: blockDiagnostic, title: "System", body: body,
			})
		case ai.RoleTool:
		}
	}

	for _, tool := range state.Tools {
		if _, exists := projectedTools[tool.Call.ID]; exists {
			continue
		}
		blocks = append(blocks, projectTool(tool))
	}

	draft := visibleDraftText(state.Draft)
	if draft != "" {
		blocks = append(blocks, timelineBlock{
			kind: blockDraft, title: appTitle, body: draft, status: "streaming",
		})
	}

	if state.Changes != nil {
		summary := fmt.Sprintf("%d workspace change(s)", len(state.Changes.Entries))
		if state.Changes.Truncated {
			summary += " · diff truncated"
		}
		blocks = append(blocks, timelineBlock{
			kind: blockChange, title: "Changes", body: summary,
		})
	}

	for _, diagnostic := range state.Diagnostics {
		message := strings.TrimSpace(diagnostic.Message)
		if message == "" {
			message = diagnostic.Code
		}
		blocks = append(blocks, timelineBlock{
			kind:   blockDiagnostic,
			title:  diagnostic.Component,
			body:   message,
			status: diagnostic.Code,
		})
	}

	if state.LastError != nil {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Error", body: state.LastError.Message,
			status: state.LastError.Code,
		})
	}

	return blocks
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
	if len(blocks) == 0 {
		return "Start a conversation. Ask about this codebase, request a change, or inspect a problem."
	}

	rendered := make([]string, 0, len(blocks))
	for _, block := range blocks {
		body := block.body
		if block.kind == blockAssistant || block.kind == blockDraft {
			value, err := markdown.render(body, max(1, width-2), theme, noColor)
			if err == nil {
				body = value
			}
		}

		title := block.title
		if block.status != "" {
			title += " · " + block.status
		}
		if !noColor {
			title = timelineTitleStyle(block.kind, theme).Render(title)
		}

		content := title
		if strings.TrimSpace(body) != "" {
			content += "\n" + body
		}
		rendered = append(rendered, content)
	}

	return strings.Join(rendered, "\n\n")
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
	case blockDiagnostic:
		if theme == themeLight {
			color = "#586069"
		}
	}

	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color))
}
