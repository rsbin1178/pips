//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, click replay and assertions follow physical order.
package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/require"
)

// routeClickHelperEnv names the environment variable each helper branch keys on.
const (
	routeClickResumeEnv  = "PIPS_TUI_ROUTECLICK_RESUME_HELPER"
	routeClickSkillsEnv  = "PIPS_TUI_ROUTECLICK_SKILLS_HELPER"
	routeClickMCPEnv     = "PIPS_TUI_ROUTECLICK_MCP_HELPER"
	routeClickTreeEnv    = "PIPS_TUI_ROUTECLICK_TREE_HELPER"
	routeClickAgentsEnv  = "PIPS_TUI_ROUTECLICK_AGENTS_HELPER"
	routeClickTabEnv     = "PIPS_TUI_ROUTECLICK_TAB_HELPER"
	routeClickResumeTest = "TestPTYRouteListClickResumeSelects"
	routeClickAgentsTest = "TestPTYRouteListClickAgentsSelects"
)

// runRouteClickHelper is the child half of every pointer test: it runs the app
// against the fixture the parent asked for, and never returns.
func runRouteClickHelper(t *testing.T, setup func(*overlayController)) {
	t.Helper()

	controller := newOverlayController(readyState())
	setup(controller)
	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		ExitOutput:     config.ExitOutputResumeHint,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

// routeClickHarness starts the helper and waits for the alternate screen.
func routeClickHarness(t *testing.T, envVar, testName string, rows, cols uint16) *ptyHarness {
	t.Helper()

	h := newPTYHarness(t, envVar, testName, rows, cols)
	require.Eventually(t, func() bool {
		return h.screenMatches(int(cols), int(rows), func(e *vt.Emulator) bool {
			return e.IsAltScreen()
		})
	}, 30*time.Second, 10*time.Millisecond)

	return h
}

// openRoutePTY types a command and waits for the route to paint its title.
func openRoutePTY(t *testing.T, h *ptyHarness, cols, rows int, command, title string) {
	t.Helper()

	h.write(command + "\r")
	require.Eventually(t, func() bool {
		return h.screenMatches(cols, rows, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), title)
		})
	}, 30*time.Second, 10*time.Millisecond, "the route opens:\n%s", h.frame(cols, rows))
}

// clickFrameText sends a left press on the first frame row carrying needle. The
// route paints its chrome before its data arrives, so the row is waited for
// rather than read once.
func clickFrameText(t *testing.T, h *ptyHarness, cols, rows int, needle string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return frameLineWith(h, cols, rows, needle) != ""
	}, 30*time.Second, 10*time.Millisecond, "%q reaches the frame:\n%s", needle, h.frame(cols, rows))

	row, column := -1, -1
	for index, line := range strings.Split(h.frame(cols, rows), "\n") {
		if position := strings.Index(line, needle); position >= 0 {
			row, column = index, position

			break
		}
	}
	require.GreaterOrEqual(t, row, 0, "%q is on screen:\n%s", needle, h.frame(cols, rows))
	require.Greater(t, column, 0)

	h.write(sgrMouse(0, column+1, row+1, false))
}

// frameLineWith returns the first frame line carrying needle.
func frameLineWith(h *ptyHarness, cols, rows int, needle string) string {
	for _, line := range strings.Split(h.frame(cols, rows), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}

	return ""
}

// requireSelectedRow waits until the row carrying needle is the one the cursor
// marks, which is what a click moves.
func requireSelectedRow(t *testing.T, h *ptyHarness, cols, rows int, needle string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return h.screenMatches(cols, rows, func(e *vt.Emulator) bool {
			for _, line := range strings.Split(e.String(), "\n") {
				if strings.Contains(line, needle) && strings.HasPrefix(line, "› ") {
					return true
				}
			}

			return false
		})
	}, 30*time.Second, 10*time.Millisecond, "the clicked row is selected:\n%s", h.frame(cols, rows))
}

// requireInsideWidth pins that the painted frame never exceeds the terminal.
func requireInsideWidth(t *testing.T, h *ptyHarness, cols, rows int) {
	t.Helper()

	for _, line := range strings.Split(h.frame(cols, rows), "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), cols, "the frame stays inside the terminal width")
	}
}

func TestPTYRouteListClickResumeSelects(t *testing.T) {
	if os.Getenv(routeClickResumeEnv) == "1" {
		created := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.sessions = []session.Metadata{
				{ID: "s-0000000000000001", Preview: "ROW-ALPHA", CreatedAt: created},
				{ID: "s-0000000000000002", Preview: "ROW-BETA", CreatedAt: created.Add(time.Minute)},
			}
		})
	}

	h := routeClickHarness(t, routeClickResumeEnv, routeClickResumeTest, 24, 100)
	openRoutePTY(t, h, 100, 24, "/resume", "Resume session")
	clickFrameText(t, h, 100, 24, "ROW-BETA")
	requireSelectedRow(t, h, 100, 24, "ROW-BETA")
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "Resume session")
		})
	}, 30*time.Second, 10*time.Millisecond, "a click never resumes the Session:\n%s", h.frame(100, 24))
	requireInsideWidth(t, h, 100, 24)
}

func TestPTYRouteListClickSkillsSelects(t *testing.T) {
	const testName = "TestPTYRouteListClickSkillsSelects"
	if os.Getenv(routeClickSkillsEnv) == "1" {
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.skillSnapshot = coding.SkillSnapshot{Skills: []coding.SkillSummary{
				{ID: "skill-a", Name: "SKILL-ALPHA", Description: "First skill", Source: "project"},
				{ID: "skill-b", Name: "SKILL-BETA", Description: "Second skill", Source: "project"},
			}}
		})
	}

	h := routeClickHarness(t, routeClickSkillsEnv, testName, 24, 100)
	openRoutePTY(t, h, 100, 24, "/skills", "Project Skills")
	clickFrameText(t, h, 100, 24, "SKILL-BETA")
	requireSelectedRow(t, h, 100, 24, "SKILL-BETA")
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "[ ] SKILL-BETA")
		})
	}, 30*time.Second, 10*time.Millisecond, "a click never toggles the Skill:\n%s", h.frame(100, 24))
	requireInsideWidth(t, h, 100, 24)
}

func TestPTYRouteListClickMCPSelects(t *testing.T) {
	const testName = "TestPTYRouteListClickMCPSelects"
	if os.Getenv(routeClickMCPEnv) == "1" {
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.mcpSnapshot = coding.MCPSnapshot{
				GenerationID: 1, Settled: true,
				Servers: []codingmcp.ServerStatus{
					{ID: "MCP-ALPHA", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio, State: codingmcp.ServerStateConnected},
					{ID: "MCP-BETA", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio, State: codingmcp.ServerStateConnected},
				},
			}
		})
	}

	h := routeClickHarness(t, routeClickMCPEnv, testName, 24, 100)
	openRoutePTY(t, h, 100, 24, "/mcp", "MCP Servers")
	clickFrameText(t, h, 100, 24, "MCP-BETA")
	requireSelectedRow(t, h, 100, 24, "MCP-BETA")
	requireInsideWidth(t, h, 100, 24)
}

func TestPTYRouteListClickTreeSelects(t *testing.T) {
	const testName = "TestPTYRouteListClickTreeSelects"
	if os.Getenv(routeClickTreeEnv) == "1" {
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.tree = coding.SessionTree{Nodes: []coding.SessionNode{
				{ID: "node-a", Kind: coding.SessionNodeMessage, Label: "TREE-ALPHA", OnActivePath: true},
				{ID: "node-b", Kind: coding.SessionNodeMessage, Label: "TREE-BETA"},
			}}
		})
	}

	h := routeClickHarness(t, routeClickTreeEnv, testName, 24, 100)
	openRoutePTY(t, h, 100, 24, "/tree", "Session tree")
	clickFrameText(t, h, 100, 24, "TREE-BETA")
	requireSelectedRow(t, h, 100, 24, "TREE-BETA")
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "Session tree")
		})
	}, 30*time.Second, 10*time.Millisecond, "a click never navigates or forks:\n%s", h.frame(100, 24))
	requireInsideWidth(t, h, 100, 24)
}

func TestPTYRouteListClickAgentsSelects(t *testing.T) {
	if os.Getenv(routeClickAgentsEnv) == "1" {
		created := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.agents = []subagent.Summary{
				{ChildSessionID: "child-a", State: subagent.StateSucceeded, TaskPreview: "AGENT-ALPHA", CreatedAt: created},
				{ChildSessionID: "child-b", State: subagent.StateSucceeded, TaskPreview: "AGENT-BETA", CreatedAt: created.Add(time.Minute)},
			}
		})
	}

	h := routeClickHarness(t, routeClickAgentsEnv, routeClickAgentsTest, 24, 100)
	openRoutePTY(t, h, 100, 24, "/agents", "Agents · Runs")
	clickFrameText(t, h, 100, 24, "AGENT-BETA")
	requireSelectedRow(t, h, 100, 24, "AGENT-BETA")
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "Agents · Runs")
		})
	}, 30*time.Second, 10*time.Millisecond, "a click never opens the selected child:\n%s", h.frame(100, 24))
	requireInsideWidth(t, h, 100, 24)
}

func TestPTYAgentsTabClick(t *testing.T) {
	const testName = "TestPTYAgentsTabClick"
	if os.Getenv(routeClickTabEnv) == "1" {
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.agentLibrary = coding.AgentLibrary{Entries: []coding.AgentLibraryEntry{
				{ID: "lib-a", Name: "LIB-ALPHA", Kind: "agent", Scope: "project", Source: "workspace", Available: true},
			}}
		})
	}

	h := routeClickHarness(t, routeClickTabEnv, testName, 24, 100)
	openRoutePTY(t, h, 100, 24, "/agents", "Agents · Runs")
	// The Runs tab is the active one, so the bar paints it bracketed and the
	// footer's "Ctrl+L Library" hint is the only other place "Library" appears.
	bar := frameLineWith(h, 100, 24, "[Runs]")
	require.Contains(t, bar, "Library", "the tab bar paints both titles:\n%s", h.frame(100, 24))
	column := strings.Index(bar, "Library")
	row := -1
	for index, line := range strings.Split(h.frame(100, 24), "\n") {
		if line == bar {
			row = index

			break
		}
	}
	require.GreaterOrEqual(t, row, 0)
	h.write(sgrMouse(0, column+1, row+1, false))

	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "Agents · Library") &&
				strings.Contains(e.String(), "LIB-ALPHA")
		})
	}, 30*time.Second, 10*time.Millisecond, "the clicked tab switches the view:\n%s", h.frame(100, 24))
	requireInsideWidth(t, h, 100, 24)
}

// TestPTYRouteListClickNarrowWidth pins that a click still lands on the painted
// row on a narrow terminal, and that the frame stays inside the width there.
func TestPTYRouteListClickNarrowWidth(t *testing.T) {
	for _, cols := range []int{34, 40} {
		h := routeClickHarness(t, routeClickResumeEnv, routeClickResumeTest, 24, uint16(cols))
		openRoutePTY(t, h, cols, 24, "/resume", "Resume session")
		clickFrameText(t, h, cols, 24, "ROW-BETA")
		requireSelectedRow(t, h, cols, 24, "ROW-BETA")
		requireInsideWidth(t, h, cols, 24)
	}
	// The /agents tab row is the only new painted control, so the narrow case
	// covers it too: the bar clips to the width and the rows below it stay
	// addressable.
	for _, cols := range []int{34, 40} {
		h := routeClickHarness(t, routeClickAgentsEnv, routeClickAgentsTest, 24, uint16(cols))
		openRoutePTY(t, h, cols, 24, "/agents", "Agents · Runs")
		clickFrameText(t, h, cols, 24, "AGENT-BETA")
		requireSelectedRow(t, h, cols, 24, "AGENT-BETA")
		requireInsideWidth(t, h, cols, 24)
	}
}
