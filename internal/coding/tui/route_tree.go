//nolint:wsl_v5 // Tree-route navigation and rendering share the same route state.
package tui

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

type treeRouteDataMsg struct {
	generation uint64
	tree       coding.SessionTree
	err        error
}

func (m *Model) openTreeRoute(forkMode bool) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{kind: routeTree, forkMode: forkMode})
}

func (m *Model) activateTreeRoute(forkMode bool) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.routeSeq++
	m.route = routeState{kind: routeTree, loading: true, generation: m.routeSeq, forkMode: forkMode}
	m.composer.Blur()
	generation := m.route.generation

	load := func() tea.Msg {
		value, err := m.controller.Tree(m.ctx)

		return treeRouteDataMsg{generation: generation, tree: value, err: err}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

func (m *Model) updateTreeRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}

	values := m.filteredTreeNodes()
	if moved, _ := m.updateTreeRouteScrollKey(key, values); moved {
		return m, nil
	}
	switch key {
	case keyBackspace:
		m.route.query = trimLastRune(m.route.query)
		m.route.cursor = 0
	case "f":
		m.route.forkMode = true
	case keyEnter, "s":
		if len(values) == 0 || m.route.loading || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}
		entryID := values[m.route.cursor].ID
		if m.route.forkMode {
			return m, m.runControl(operationFork, entryID, modelcatalog.Selection{})
		}
		summarize := key == "s"
		m.route = routeState{}

		return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.Navigate(ctx, entryID, summarize)
		})
	default:
		if message.Key().Text != "" {
			m.route.query += message.Key().Text
			m.route.cursor = 0
		}
	}

	return m, nil
}

// updateTreeRouteScrollKey applies one navigation key and reports whether it
// owned the key, so the tree keeps selection scrolling in one place.
func (m *Model) updateTreeRouteScrollKey(key string, values []coding.SessionNode) (bool, tea.Cmd) {
	last := max(0, len(values)-1)
	page := m.treeRouteBodyHeight()

	switch key {
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case keyPageUp:
		m.route.cursor = max(0, m.route.cursor-page)
	case keyPageDown:
		m.route.cursor = min(last, m.route.cursor+page)
	case keyHome:
		m.route.cursor = 0
	case keyEnd:
		m.route.cursor = last
	default:
		return false, nil
	}

	if key == keyHome {
		m.route.offset = 0
	} else {
		m.route.offset = m.treeRouteScrollOffset(values)
	}

	return true, nil
}

func (m *Model) treeRouteBodyHeight() int {
	header, footer := m.treeRouteChrome()

	return max(1, m.height-lipgloss.Height(header)-lipgloss.Height(footer)-1)
}

// treeRouteScrollOffset keeps the cursor row inside the scrolled body window.
func (m *Model) treeRouteScrollOffset(values []coding.SessionNode) int {
	body := m.treeRouteBodyHeight()
	if m.route.cursor < m.route.offset {
		return m.route.cursor
	}
	if m.route.cursor >= m.route.offset+body {
		return m.route.cursor - body + 1
	}

	return min(m.route.offset, max(0, len(values)-body))
}

func (m *Model) filteredTreeNodes() []coding.SessionNode {
	query := strings.ToLower(strings.TrimSpace(m.route.query))
	filtered := make([]coding.SessionNode, 0, len(m.route.tree.Nodes))
	for _, node := range m.route.tree.Nodes {
		if query == "" || strings.Contains(strings.ToLower(node.ID), query) ||
			strings.Contains(strings.ToLower(node.Label), query) ||
			strings.Contains(string(node.Kind), query) {
			filtered = append(filtered, node)
		}
	}

	return filtered
}

// treeRouteContent renders the tree body only. Header and key hint are pinned
// by treeRouteView so the selection can never scroll them away.
func (m *Model) treeRouteContent() string {
	if m.route.loading {
		return m.activityNotice("Loading session tree…")
	}
	values := m.filteredTreeNodes()
	if len(values) == 0 {
		return "No matching nodes."
	}

	start, end := m.treeRouteWindow(values)
	lines := make([]string, 0, end-start+1)
	for index := start; index < end; index++ {
		lines = append(lines, m.treeRouteRow(values[index], index == m.route.cursor))
	}
	if m.route.tree.Truncated {
		lines = append(lines, fmt.Sprintf(
			"Showing %d of %d nodes (bounded).",
			len(m.route.tree.Nodes), m.route.tree.TotalNodes,
		))
	}

	return strings.Join(lines, "\n")
}

// treeRouteWindow keeps the cursor row inside the visible body and returns the
// half-open range of node indexes to render.
func (m *Model) treeRouteWindow(values []coding.SessionNode) (int, int) {
	heights := make([]int, len(values))
	for index := range heights {
		heights[index] = 1
	}
	maximum := m.treeRouteBodyHeight()

	return selectionWindowByHeight(heights, m.route.cursor, maximum)
}

func (m *Model) treeRouteRow(node coding.SessionNode, selected bool) string {
	line := "  " + treeRowLabel(node)
	if !m.options.NoColor {
		style := lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted)
		if node.Current {
			style = style.Bold(true).Foreground(paletteFor(m.theme).session)
		}
		if selected {
			style = style.Bold(true).Foreground(paletteFor(m.theme).workspace)
		}

		return ansi.Truncate(style.Render("› "+treeRowLabel(node)), max(1, m.width), "…")
	}

	cursor := "  "
	if selected {
		cursor = "› "
	}

	return ansi.Truncate(cursor+line[2:], max(1, m.width), "…")
}

// treeRowLabel names a node by its human meaning: what happened, when, and any
// label the user attached. The opaque node ID stays available in /resume.
func treeRowLabel(node coding.SessionNode) string {
	parts := []string{treeNodeKindLabel(node.Kind)}
	if node.Label != "" {
		parts = append(parts, truncateText(oneLineToolText(node.Label), 48))
	}
	if !node.CreatedAt.IsZero() {
		parts = append(parts, formatRelativeTime(node.CreatedAt))
	}
	if node.OnActivePath {
		parts = append(parts, "on path")
	}
	if node.Current {
		parts = append(parts, "current")
	}

	indent := strings.Repeat("  ", min(node.Depth, 12))

	return indent + treeConnector(node.Depth) + strings.Join(parts, " · ")
}

func treeNodeKindLabel(kind coding.SessionNodeKind) string {
	switch kind {
	case coding.SessionNodeMessage:
		return "Message"
	case coding.SessionNodeModelChange:
		return "Model changed"
	case coding.SessionNodeCompaction:
		return "Compacted"
	case coding.SessionNodeBranchSummary:
		return "Branch summary"
	case coding.SessionNodeCustom:
		return "Custom"
	case coding.SessionNodeLabel:
		return "Label"
	case coding.SessionNodeName:
		return "Renamed"
	default:
		return humanizeStatusCode(string(kind))
	}
}

// treeRouteChrome is the pinned header and hint text for the tree route.
func (m *Model) treeRouteChrome() (string, string) {
	title := "Session tree"
	if m.route.forkMode {
		title = "Fork session from node"
	}
	header := title + " · filter: " + m.route.query
	hint := "↑/↓ choose · type to search · Enter navigate · s summary · f fork · Esc close"
	if m.route.forkMode {
		hint = "↑/↓ choose · type to search · Enter fork · Esc cancel"
	}

	return header, hint
}

func (m *Model) treeRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)
	header, hint := m.treeRouteChrome()

	body := m.treeRouteContent()
	if m.route.err != nil {
		body += "\n\nError: " + safeError(m.route.err)
	}
	if !m.options.NoColor {
		header = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).session).Render(header)
		hint = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(hint)
	}
	footer := m.sessionPickerSeparator(width) + "\n" +
		ansi.Truncate(hint, width, "…")
	bodyHeight := max(1, height-lipgloss.Height(header)-lipgloss.Height(footer))

	window := fitScrollableContent(body, width, bodyHeight, m.route.offset)
	if padding := bodyHeight - lipgloss.Height(window); padding > 0 {
		window += strings.Repeat("\n", padding)
	}

	view := m.presentationView(truncateHeight(header+"\n"+window+"\n"+footer, height))

	return view
}

// formatRelativeTime renders a compact age for a durable node timestamp.
func formatRelativeTime(value time.Time) string {
	elapsed := time.Since(value)
	switch {
	case elapsed < time.Minute:
		return labelJustNow
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	default:
		return value.Format("2006-01-02")
	}
}

func treeConnector(depth int) string {
	if depth == 0 {
		return "─ "
	}

	return "└ "
}

func shortDisplayID(value string) string {
	const limit = 12
	if len([]rune(value)) <= limit {
		return value
	}

	runes := []rune(value)

	return string(runes[:limit]) + "…"
}
