//nolint:wsl_v5 // Tree-route navigation and rendering share the same route state.
package tui

import (
	"context"
	"fmt"
	"iter"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
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
	switch key {
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
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

func (m *Model) treeRouteContent() string {
	title := "Session tree"
	if m.route.forkMode {
		title = "Fork session from node"
	}
	lines := []string{title, "", "Filter: " + m.route.query, ""}
	if m.route.loading {
		return strings.Join(append(lines, m.activityNotice("Loading session tree…")), "\n")
	}
	values := m.filteredTreeNodes()
	if len(values) == 0 {
		lines = append(lines, "No matching nodes.")
	}
	for index, node := range values {
		cursor := "  "
		if index == m.route.cursor {
			cursor = "> "
		}
		path := " "
		if node.OnActivePath {
			path = "*"
		}
		current := ""
		if node.Current {
			current = " [current]"
		}
		label := ""
		if node.Label != "" {
			label = "  " + node.Label
		}
		indent := strings.Repeat("  ", min(node.Depth, 12))
		lines = append(lines, fmt.Sprintf(
			"%s%s%s%s  %s  %s%s%s",
			cursor, path, indent, treeConnector(node.Depth), shortDisplayID(node.ID),
			node.Kind, label, current,
		))
	}
	if m.route.tree.Truncated {
		lines = append(lines, "", fmt.Sprintf(
			"Showing %d of %d nodes (bounded).",
			len(m.route.tree.Nodes), m.route.tree.TotalNodes,
		))
	}
	if m.route.forkMode {
		lines = append(lines, "", "↑/↓ choose · type search · Enter fork · Esc cancel")
	} else {
		lines = append(lines, "", "Enter navigate · s navigate with summary · f fork mode · Esc close")
	}

	return strings.Join(lines, "\n")
}

func (m *Model) treeRouteView() tea.View {
	content := m.treeRouteContent()
	if m.route.err != nil {
		content += "\n\nError: " + safeError(m.route.err)
	}
	content = fitScrollableContent(content, max(1, m.width), max(1, m.height), m.route.offset)

	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
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
