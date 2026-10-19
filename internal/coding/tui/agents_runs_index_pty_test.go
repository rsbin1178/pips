//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads, and assertions follow physical order.
package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	agentsRunsIndexEnv  = "PIPS_TUI_AGENTS_RUNS_INDEX_HELPER"
	agentsRunsIndexTest = "TestPTYAgentsRunsListsDurableChild"
)

// TestPTYAgentsRunsListsDurableChild drives a real terminal to the Agent list and
// pins that the Runs tab is where a durable child is read: the tab is the active
// one, and each row carries the child's own task, identity, and lifecycle state.
//
//nolint:paralleltest // The PTY helper owns one child process, like every sibling scenario here.
func TestPTYAgentsRunsListsDurableChild(t *testing.T) {
	if os.Getenv(agentsRunsIndexEnv) == "1" {
		created := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.agents = []subagent.Summary{
				{
					ChildSessionID: "s-indexed-running", Role: subagent.RoleExplore,
					State: subagent.StateRunning, TaskPreview: "INDEXED-RUNNING",
					Model: "openai/indexed", CreatedAt: created,
				},
				{
					ChildSessionID: "s-indexed-terminal", Role: subagent.RolePlan,
					State: subagent.StateSucceeded, TaskPreview: "INDEXED-TERMINAL",
					Model: "openai/indexed", CreatedAt: created.Add(time.Minute),
					Duration: 2 * time.Second,
				},
			}
		})
	}

	const cols, rows = 100, 24

	harness := routeClickHarness(t, agentsRunsIndexEnv, agentsRunsIndexTest, rows, cols)
	openRoutePTY(t, harness, cols, rows, "/agents", "Agents · Runs")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "[Runs]")
	}, 30*time.Second, 10*time.Millisecond,
		"the Runs tab is the active one:\n%s", harness.frame(cols, rows))
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows), "INDEXED-RUNNING", "INDEXED-TERMINAL")
	}, 30*time.Second, 10*time.Millisecond,
		"both child rows paint:\n%s", harness.frame(cols, rows))

	frame := harness.frame(cols, rows)
	assert.Contains(t, frame, "explore subagent · running",
		"the row carries the child's identity and state")
	assert.Contains(t, frame, "plan subagent · succeeded")
	requireInsideWidth(t, harness, cols, rows)
}
