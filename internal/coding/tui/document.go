//nolint:wsl_v5 // Markdown section assembly keeps each block branch next to its output.
package tui

import "strings"

// conversationBlocks returns the projected conversation in display order for an
// export. It is the managed projection without the operator-facing notices
// (banner, /help, /status): those are UI reports rather than conversation, and
// only the fullscreen path keeps them in memory, so including them would make the
// same session export different documents per presentation mode.
func (m *Model) conversationBlocks() []timelineBlock {
	return m.managedTimelineBlocks(false, false)
}

// conversationMarkdown renders the projected conversation as plain Markdown.
// Bodies are the unrendered text the projection already carries, so an export
// never has to strip the viewport's own styling back out of a rendered frame.
func conversationMarkdown(blocks []timelineBlock) string {
	sections := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if section := markdownBlock(block); section != "" {
			sections = append(sections, section)
		}
	}

	return strings.Join(sections, "\n\n")
}

// conversationHeader names the session an exported document came from. It is
// deliberately timestamp-free so a saved export is stable for a given session
// state.
func conversationHeader(sessionID, workspace, provider, modelID string) string {
	lines := []string{"# Pips conversation", ""}
	for _, field := range []struct{ label, value string }{
		{"Session", sessionID},
		{"Workspace", workspace},
		{"Model", modelRef(provider, modelID)},
	} {
		value := strings.TrimSpace(sanitizeInspectionText(field.value))
		if value != "" {
			lines = append(lines, field.label+": "+value)
		}
	}

	return strings.Join(lines, "\n")
}

func modelRef(provider, modelID string) string {
	if provider == "" {
		return modelID
	}
	if modelID == "" {
		return provider
	}

	return provider + "/" + modelID
}

// markdownBlock renders one block as its own section, or nothing when the block
// carries no text a reader would want.
func markdownBlock(block timelineBlock) string {
	switch block.kind {
	case blockUser:
		return markdownSection("User", block.body)
	case blockAssistant, blockDraft:
		return markdownSection("Assistant", block.body)
	case blockPlan:
		return markdownSection("Plan", block.body)
	case blockQuestion:
		return markdownSection(markdownHeading("Question", block.title), block.body)
	case blockDiagnostic:
		return markdownSection(markdownHeading("Notice", block.title), block.body)
	case blockError:
		return markdownSection(markdownHeading("Error", block.title), markdownErrorBody(block))
	case blockCompletion:
		return markdownSection("Run", block.body)
	case blockTeam:
		return markdownSection(markdownHeading("Team", block.title), block.body)
	case blockTool:
		return markdownToolSection(block)
	case blockChange:
		return markdownSection("Changes", markdownWorkspaceChange(block))
	default:
		return ""
	}
}

// markdownHeading prefers a block's own title, because several block kinds use
// it to say what happened rather than only which surface produced the entry.
func markdownHeading(fallback, title string) string {
	if value := strings.TrimSpace(sanitizeInspectionText(title)); value != "" {
		return fallback + " · " + value
	}

	return fallback
}

// markdownErrorBody falls back to the humanized code when an error carries no
// message of its own, mirroring what the viewport renders.
func markdownErrorBody(block timelineBlock) string {
	if strings.TrimSpace(block.body) != "" {
		return block.body
	}

	return humanizeStatusCode(block.status)
}

func markdownSection(heading, body string) string {
	heading = strings.TrimSpace(sanitizeInspectionText(heading))
	body = strings.TrimSpace(sanitizeInspectionText(body))
	if heading == "" || body == "" {
		return ""
	}

	return "## " + heading + "\n\n" + body
}

// markdownToolSection lists the calls a tool card stands for and puts each
// result in a code fence, so a result that itself contains Markdown or a fence
// survives the round trip.
func markdownToolSection(block timelineBlock) string {
	entries := make([]string, 0, len(block.tools))
	for _, activity := range block.tools {
		if entry := markdownToolCall(activity); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return ""
	}

	return markdownSection("Tool", strings.Join(entries, "\n\n"))
}

func markdownToolCall(activity toolActivity) string {
	invocation := strings.TrimSpace(sanitizeInspectionText(activity.invocation))
	if invocation == "" {
		invocation = strings.TrimSpace(sanitizeInspectionText(activity.name))
	}
	if invocation == "" {
		return ""
	}

	line := markdownCodeSpan(invocation)
	if state := markdownToolState(activity.state); state != "" {
		line += " — " + state
	}

	body := strings.TrimSpace(sanitizeInspectionText(activity.body))
	if body == "" {
		return line
	}

	return line + "\n\n" + markdownCodeFence(body)
}

// markdownToolState reports only the states that change how a result reads; a
// successful call needs no label.
func markdownToolState(state toolActivityState) string {
	switch state {
	case toolStateRunning:
		return "running"
	case toolStateFailed:
		return toolStateTextFailed
	case toolStateInterrupted:
		return "interrupted"
	default:
		return ""
	}
}

func markdownWorkspaceChange(block timelineBlock) string {
	if block.workspaceChanges == nil {
		return ""
	}

	value := *block.workspaceChanges
	lines := []string{workspaceChangeSummary(value)}
	for _, entry := range value.Entries {
		lines = append(
			lines,
			"- "+workspaceChangeGlyph(entry.Kind)+" "+workspaceChangePath(entry),
		)
	}

	return strings.Join(lines, "\n")
}

// markdownCodeSpan wraps value in a backtick run longer than any inside it, which
// is what keeps an invocation containing a backtick from breaking the span.
func markdownCodeSpan(value string) string {
	longest := 0

	run := 0
	for _, character := range value {
		if character != '`' {
			run = 0

			continue
		}

		run++

		longest = max(longest, run)
	}

	delimiter := strings.Repeat("`", longest+1)

	padding := ""
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		padding = " "
	}

	return delimiter + padding + value + padding + delimiter
}

// markdownCodeFence fences value with a delimiter longer than any fence the value
// itself starts a line with.
func markdownCodeFence(value string) string {
	longest := 0
	for line := range strings.SplitSeq(value, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}

		run := 0
		for _, character := range trimmed {
			if character != '`' {
				break
			}

			run++
		}

		longest = max(longest, run)
	}

	delimiter := strings.Repeat("`", max(3, longest+1))

	return delimiter + "\n" + value + "\n" + delimiter
}
