//nolint:wsl_v5 // Detail fixtures keep transcript setup next to disclosure assertions.
package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentDetailUsesSemanticToolProjection(t *testing.T) {
	t.Parallel()

	const reasoning = "private child reasoning"
	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"model.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateSucceeded, TaskPreview: "Inspect the TUI",
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect the TUI."),
			{
				Role: ai.RoleAssistant,
				Parts: []ai.Part{
					ai.ReasoningPart{Text: reasoning},
					call,
				},
			},
			codingToolResultFor(call.ID, call.Name, "file contents"),
			ai.AssistantText("Inspection complete."),
		},
		Activity: subagent.Activity{
			Phase: subagent.ActivityPhaseFinalizing,
			Tools: []subagent.ToolActivity{{
				RunID: "run-1", Turn: 1, Call: call,
				Status: subagent.ToolStatusCompleted,
				Result: codingToolResultFor(call.ID, call.Name, "file contents"),
			}},
		},
		Result: subagent.ExploreResult{
			Summary: "Located the TUI projection.",
			Evidence: []subagent.Evidence{{
				Path:      "internal/coding/tui/agents_overlay.go",
				StartLine: 1, EndLine: 40, Claim: "Owns the detail surface.",
			}},
			Unknowns: []string{},
		},
	}
	model := readyModel(t, true)

	content := model.agentDetailContent(detail)
	assert.Contains(t, content, "• Explored")
	assert.Contains(t, content, "Read model.go")
	assert.Equal(t, 1, strings.Count(content, "Read model.go"))
	assert.Contains(t, content, "Located the TUI projection.")
	assert.Contains(t, content, "agents_overlay.go:1-40")
	assert.NotContains(t, content, "Inspection complete.")
	assert.NotContains(t, content, reasoning)
	assert.NotContains(t, content, "[tool")
	assert.NotContains(t, content, `{"path"`)
	assert.NotContains(t, content, "pips.coding.tool_result")
}

func TestSubagentDetailPrioritizesCurrentActivity(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-7 * time.Second)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RolePlan,
			State: subagent.StateRunning, TaskPreview: "Plan the activity view",
			Model: "openai/model", Turns: 2, ToolCalls: 1,
		},
		Activity: subagent.Activity{
			Phase: subagent.ActivityPhaseWorking, StartedAt: startedAt,
			Tools: []subagent.ToolActivity{{
				RunID: "run-1", Turn: 2,
				Call: ai.ToolCallPart{
					ID: "call-1", Name: toolNameSearch,
					Args: ai.JSON(`{"pattern":"InspectSubagent","path":"internal/coding"}`),
				},
				Status: subagent.ToolStatusRunning,
			}},
		},
	}

	content := readyModel(t, true).agentDetailContent(detail)
	assert.Contains(t, content, "✻ Planning")
	assert.Contains(t, content, "Task\n  Plan the activity view")
	assert.Contains(t, content, "Now")
	assert.Contains(t, content, "Search InspectSubagent in internal/coding")
	assert.NotContains(t, content, "Transcript")
	assert.NotContains(t, content, "Structured result")
}

func TestSubagentDetailRefreshIsCoalescedAndRejectsStaleResults(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	oldDetail := subagent.Detail{Summary: subagent.Summary{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: "old task",
	}}
	model.overlay = overlayState{
		kind: overlayAgents, generation: 7, childSessionID: "child-1",
		agentDetail: &oldDetail,
	}

	first := model.refreshAgentDetail()
	require.NotNil(t, first)
	assert.True(t, model.overlay.refreshing)
	assert.Nil(t, model.refreshAgentDetail())
	assert.True(t, model.overlay.refreshPending)

	newDetail := oldDetail
	newDetail.Summary.TaskPreview = "new task"
	_, followUp := model.Update(overlayDataMsg{
		kind: overlayAgents, generation: 7, childSessionID: "child-1",
		background: true, hasDetail: true, detail: newDetail,
	})
	require.NotNil(t, followUp)
	assert.True(t, model.overlay.refreshing)
	assert.False(t, model.overlay.refreshPending)
	assert.Equal(t, "new task", model.overlay.agentDetail.Summary.TaskPreview)

	stale := newDetail
	stale.Summary.TaskPreview = "stale task"
	model.Update(overlayDataMsg{
		kind: overlayAgents, generation: 6, childSessionID: "child-1",
		background: true, hasDetail: true, detail: stale,
	})
	assert.Equal(t, "new task", model.overlay.agentDetail.Summary.TaskPreview)

	model.Update(overlayDataMsg{
		kind: overlayAgents, generation: 7, childSessionID: "child-1",
		background: true, err: errors.New("temporary inspect failure"),
	})
	assert.Equal(t, "new task", model.overlay.agentDetail.Summary.TaskPreview)
	assert.EqualError(t, model.overlay.refreshErr, "temporary inspect failure")
}

func TestSubagentDetailRefreshFiltersUnrelatedChildren(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	detail := subagent.Detail{Summary: subagent.Summary{ChildSessionID: "child-1"}}
	model.overlay = overlayState{
		kind: overlayAgents, generation: 1, childSessionID: "child-1",
		agentDetail: &detail,
	}

	command := model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-2"},
	}})
	assert.Nil(t, command)
	assert.False(t, model.overlay.refreshing)

	command = model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-1"},
	}})
	require.NotNil(t, command)
	assert.True(t, model.overlay.refreshing)
}
