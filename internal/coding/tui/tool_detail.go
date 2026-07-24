//nolint:wsl_v5 // Detail sections keep disclosure guards adjacent to each payload.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding/tools"
)

type toolDetailView struct {
	title   string
	content string
}

func (m *Model) openToolDetailRoute(detail toolDetailView) {
	m.route = routeState{kind: routeToolDetail, toolDetail: &detail}
	m.composer.Blur()
}

func (m *Model) updateToolDetailRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if key == keyCtrlT || key == keyEscape || key == keyCtrlC {
		m.route = routeState{}

		return m, m.composer.Focus()
	}

	visible := max(1, m.height)
	lineCount := strings.Count(m.toolDetailRouteContent(), "\n") + 1
	maximum := max(0, lineCount-visible)
	switch key {
	case "up", "k":
		m.route.offset = max(0, m.route.offset-1)
	case keyDown, "j":
		m.route.offset = min(maximum, m.route.offset+1)
	case "pgup":
		m.route.offset = max(0, m.route.offset-visible)
	case "pgdown":
		m.route.offset = min(maximum, m.route.offset+visible)
	case "home":
		m.route.offset = 0
	case "end":
		m.route.offset = maximum
	}

	return m, nil
}

func (m *Model) toolDetailRouteContent() string {
	if m.route.toolDetail == nil {
		return "Tool details\n\nNo Tool activity is available."
	}

	detail := m.route.toolDetail

	return detail.title + "\n\n" + detail.content +
		"\n\n↑/↓ or PgUp/PgDn scroll · Ctrl+T/Esc close"
}

func (m *Model) toolDetailRouteView() tea.View {
	content := fitScrollableContent(
		m.toolDetailRouteContent(),
		max(1, m.width),
		max(1, m.height),
		m.route.offset,
	)
	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func newToolDetailView(block timelineBlock) toolDetailView {
	title := "Tool details"
	if len(block.tools) > 0 {
		switch block.tools[0].class {
		case toolClassExplore:
			title = "Exploration details"
		case toolClassShell:
			title = "Shell details"
		case toolClassPatch:
			title = "Workspace update details"
		case toolClassGeneric:
			title = "Tool call details"
		}
	}

	sections := make([]string, 0, len(block.tools))
	for index, activity := range block.tools {
		sections = append(sections, renderToolDetailSection(index+1, activity))
	}

	return toolDetailView{
		title:   title,
		content: strings.Join(sections, "\n\n"),
	}
}

func renderToolDetailSection(index int, activity toolActivity) string {
	heading := activity.action
	if activity.subject != "" {
		heading += " " + activity.subject
	}
	if activity.class == toolClassGeneric {
		heading = activity.name
	}
	if heading == "" {
		heading = activity.name
	}

	lines := []string{
		fmt.Sprintf("%d. %s", index, heading),
		"Status: " + toolActivityStateLabel(activity.state),
		"Arguments:",
		redactToolArguments(activity.arguments),
	}
	if facts := toolResultFacts(activity); facts != "" {
		lines = append(lines, "Facts: "+facts)
	}
	if update := truncateText(sanitizeToolText(activity.update), maximumToolDetailBytes); update != "" {
		lines = append(lines, "Progress:", update)
	}
	if result := toolDetailResult(activity); result != "" {
		lines = append(lines, "Result:", result)
	}

	return strings.Join(lines, "\n")
}

func toolDetailResult(activity toolActivity) string {
	if containsSensitiveToolText(activity.body) {
		return "[sensitive result omitted]"
	}

	return truncateText(sanitizeToolText(activity.body), maximumToolDetailBytes)
}

func toolActivityStateLabel(state toolActivityState) string {
	switch state {
	case toolStateRunning:
		return "running"
	case toolStateSucceeded:
		return "completed"
	case toolStateFailed:
		return toolStateTextFailed
	case toolStateInterrupted:
		return "interrupted"
	default:
		return "unknown"
	}
}

func toolResultFacts(activity toolActivity) string {
	if !activity.hasHeader {
		return ""
	}

	facts := make([]string, 0, 8)
	appendCount := func(value int, label string) {
		if value > 0 {
			facts = append(facts, fmt.Sprintf("%d %s", value, label))
		}
	}
	appendCount(activity.header.Counts.Lines, "lines")
	appendCount(activity.header.Counts.Entries, "entries")
	appendCount(activity.header.Counts.Files, "files")
	appendCount(activity.header.Counts.Matches, "matches")
	appendCount(activity.header.Counts.Scanned, "scanned")
	appendCount(activity.header.Counts.Skipped, "skipped")
	if activity.header.Truncated {
		facts = append(facts, "truncated")
	}
	if execution := activity.header.Execution; execution != nil {
		facts = append(facts, shellExecutionFacts(*execution)...)
	}
	if code := humanizeStatusCode(activity.header.Code); code != "" && !activity.header.OK {
		facts = append(facts, code)
	}

	return strings.Join(facts, " · ")
}

func shellExecutionFacts(execution tools.ResultExecution) []string {
	facts := make([]string, 0, 3)
	if execution.Status != "" {
		facts = append(facts, execution.Status)
	}
	if execution.ExitCode != nil {
		facts = append(facts, fmt.Sprintf("exit %d", *execution.ExitCode))
	}
	if execution.DurationMS > 0 {
		facts = append(facts, formatInteractionDuration(execution.DurationMS))
	}

	return facts
}
