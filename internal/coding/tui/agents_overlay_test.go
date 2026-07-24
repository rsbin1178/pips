//nolint:wsl_v5 // Detail fixtures keep transcript setup next to disclosure assertions.
package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
					ai.TextPart{Text: "I will inspect the TUI projection."},
					ai.ReasoningPart{Text: reasoning},
					call,
				},
			},
			codingToolResultFor(call.ID, call.Name, "file contents"),
			ai.AssistantText(`{"summary":"Located the TUI projection.","evidence":[],"unknowns":[]}`),
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

	content := model.subagentRouteContent(detail)
	assert.Contains(t, content, "• Explored")
	assert.Contains(t, content, "❯ Inspect the TUI.")
	assert.Contains(t, content, "I will inspect the TUI projection.")
	assert.Contains(t, content, "Read model.go")
	assert.Contains(t, content, "file contents")
	assert.Equal(t, 1, strings.Count(content, "Read model.go"))
	assert.Contains(t, content, "Located the TUI projection.")
	assert.Contains(t, content, "agents_overlay.go:1-40")
	assert.Contains(t, content, "▣ 0s")
	assert.NotContains(t, content, "\nActivity\n")
	assert.NotContains(t, content, "\nResult\n")
	assert.NotContains(t, content, "\nDetails\n")
	assert.NotContains(t, content, reasoning)
	assert.NotContains(t, content, "[tool")
	assert.NotContains(t, content, `{"path"`)
	assert.NotContains(t, content, `"evidence"`)
	assert.NotContains(t, content, "pips.coding.tool_result")
}

func TestSubagentDetailExpandsEveryToolResultInChronologicalFlow(t *testing.T) {
	t.Parallel()

	transcript := []ai.Message{ai.UserText("Inspect every package.")}
	for index := range compactExploreRows + 2 {
		call := ai.ToolCallPart{
			ID:   fmt.Sprintf("call-%d", index),
			Name: toolNameList,
			Args: ai.JSON(fmt.Sprintf(`{"path":"package-%d"}`, index)),
		}
		transcript = append(
			transcript,
			ai.Message{Role: ai.RoleAssistant, Parts: []ai.Part{call}},
			codingToolResultFor(
				call.ID,
				call.Name,
				fmt.Sprintf("package-%d/file.go", index),
			),
		)
	}

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect every package",
		},
		Transcript: transcript,
	}
	model := readyModel(t, true)

	compact := model.renderTimelineBlocks(projectSubagentTimeline(detail))
	content := model.subagentRouteContent(detail)

	assert.Contains(t, compact, "more actions (ctrl+t for details)")
	assert.NotContains(t, compact, "package-7/file.go")
	assert.Contains(t, content, "List package-0")
	assert.Contains(t, content, "package-0/file.go")
	assert.Contains(t, content, "List package-7")
	assert.Contains(t, content, "package-7/file.go")
	assert.NotContains(t, content, "more actions (ctrl+t for details)")
}

func TestSubagentDetailShowsToolFailureOutputInline(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"missing.go"}`),
	}
	result := ai.ToolResultText(
		call.ID,
		call.Name,
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"read","code":"not_found"}`+
			"\n\nmissing.go: no such file",
	)
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateFailed, Code: "execution_failed",
			TaskPreview: "Read a missing file",
		},
		Transcript: []ai.Message{
			ai.UserText("Read missing.go."),
			{Role: ai.RoleAssistant, Parts: []ai.Part{call}},
			result,
		},
	}

	content := readyModel(t, true).subagentRouteContent(detail)
	assert.Contains(t, content, "Read missing.go · not found")
	assert.Contains(t, content, "missing.go: no such file")
	assert.Contains(t, content, "▌ Explore failed\n▌ execution failed")
}

func TestSubagentDetailExpandedToolResultIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"config.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleReview,
			State: subagent.StateRunning,
		},
		Transcript: []ai.Message{
			{Role: ai.RoleAssistant, Parts: []ai.Part{call}},
			codingToolResultFor(call.ID, call.Name, "API_KEY=must-not-render"),
		},
	}

	plainModel := readyModel(t, true)
	plainModel.width = 24
	coloredModel := readyModel(t, false)
	coloredModel.width = 24

	plain := plainModel.subagentRouteContent(detail)
	colored := coloredModel.subagentRouteContent(detail)

	assert.Contains(t, plain, "[sensitive")
	assert.Contains(t, plain, "result omitted]")
	assert.NotContains(t, plain, "must-not-render")
	assert.Equal(t, plain, ansi.Strip(colored))
	assert.Equal(t, lipgloss.Height(plain), lipgloss.Height(colored))
	for line := range strings.SplitSeq(plain, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 24)
	}
}

func TestSubagentDetailUsesMainTimelineInformationFlow(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"main.go"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect main.go",
			Model: "openai/model", Turns: 1, ToolCalls: 1,
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect main.go completely."),
			{Role: ai.RoleAssistant, Parts: []ai.Part{
				ai.TextPart{Text: "I will read the file first."}, call,
			}},
			codingToolResultFor(call.ID, call.Name, "package main"),
		},
	}
	model := readyModel(t, true)

	wantFlow := model.renderTimelineBlocks(projectSubagentTimeline(detail))
	content := model.subagentRouteContent(detail)

	assert.Contains(t, content, wantFlow)
	assert.Less(t, strings.Index(content, "❯ Inspect main.go completely."),
		strings.Index(content, "I will read the file first."))
	assert.Less(t, strings.Index(content, "I will read the file first."),
		strings.Index(content, "Read main.go"))
}

func TestSubagentDetailShowsChronologicalCurrentActivity(t *testing.T) {
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
		Transcript: []ai.Message{
			ai.UserText("Plan the activity view in full."),
			ai.AssistantText("I will inspect the current detail projection first."),
		},
	}

	content := readyModel(t, true).subagentRouteContent(detail)
	assert.NotContains(t, content, "Subagent · plan · running")
	assert.Contains(t, content, "❯ Plan the activity view in full.")
	assert.Contains(t, content, "I will inspect the current detail projection first.")
	assert.Contains(t, content, "Search InspectSubagent in internal/coding")
	assert.NotContains(t, content, "\nActivity\n")
	assert.NotContains(t, content, "Structured result")
}

func TestSubagentDetailKeepsEveryChronologicalToolAction(t *testing.T) {
	t.Parallel()

	first := ai.ToolCallPart{
		ID: "call-1", Name: toolNameRead, Args: ai.JSON(`{"path":"first.go"}`),
	}
	second := ai.ToolCallPart{
		ID: "call-2", Name: toolNameSearch,
		Args: ai.JSON(`{"pattern":"Second","path":"internal/coding"}`),
	}
	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleExplore,
			State: subagent.StateRunning, TaskPreview: "Inspect chronology",
		},
		Transcript: []ai.Message{
			ai.UserText("Inspect chronology."),
			{Role: ai.RoleAssistant, Parts: []ai.Part{ai.TextPart{Text: "First I will read."}, first}},
			codingToolResultFor(first.ID, first.Name, "first result"),
			{Role: ai.RoleAssistant, Parts: []ai.Part{ai.TextPart{Text: "Next I will search."}, second}},
			codingToolResultFor(second.ID, second.Name, "second result"),
		},
	}

	content := readyModel(t, true).subagentRouteContent(detail)
	assert.Contains(t, content, "First I will read.")
	assert.Contains(t, content, "Read first.go")
	assert.Contains(t, content, "Next I will search.")
	assert.Contains(t, content, "Search Second in internal/coding")
	assert.Less(t, strings.Index(content, "First I will read."), strings.Index(content, "Read first.go"))
	assert.Less(t, strings.Index(content, "Read first.go"), strings.Index(content, "Next I will search."))
	assert.Less(t, strings.Index(content, "Next I will search."), strings.Index(content, "Search Second"))
}

func TestSubagentRouteUsesMainSurfaceWithoutPanelChrome(t *testing.T) {
	t.Parallel()

	detail := subagent.Detail{
		Summary: subagent.Summary{
			ChildSessionID: "child-1", Role: subagent.RoleReview,
			State: subagent.StateRunning, Model: "openai/model",
		},
		Transcript: []ai.Message{
			ai.UserText("Review the timeline."),
			ai.AssistantText("I will inspect it now."),
		},
	}
	model := readyModel(t, true)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1", detail: &detail,
	}

	view := model.View()
	assert.Contains(t, view.Content, "❯ Review the timeline.")
	assert.Contains(t, view.Content, "I will inspect it now.")
	assert.Contains(t, view.Content, "pips  ·  review subagent  ·  openai/model  ·  running")
	assert.NotContains(t, view.Content, "Subagent · review · running")
	assert.NotContains(t, view.Content, "╭")
	assert.NotContains(t, view.Content, "╰")
	assert.Nil(t, view.Cursor)
}

func TestSubagentRouteEscapeReturnsToAgentList(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.route = routeState{
		kind: routeSubagent, childSessionID: "child-1",
	}

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	assert.Equal(t, routeAgents, model.route.kind)
}

func TestSubagentDetailRefreshIsCoalescedAndRejectsStaleResults(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	oldDetail := subagent.Detail{Summary: subagent.Summary{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: "old task",
	}}
	model.route = routeState{
		kind: routeSubagent, generation: 7, childSessionID: "child-1",
		detail: &oldDetail,
	}

	first := model.refreshSubagentRoute()
	require.NotNil(t, first)
	assert.True(t, model.route.refreshing)
	assert.Nil(t, model.refreshSubagentRoute())
	assert.True(t, model.route.refreshPending)

	newDetail := oldDetail
	newDetail.Summary.TaskPreview = "new task"
	_, followUp := model.Update(subagentRouteDataMsg{
		generation: 7, childSessionID: "child-1",
		background: true, hasDetail: true, detail: newDetail,
	})
	require.NotNil(t, followUp)
	assert.True(t, model.route.refreshing)
	assert.False(t, model.route.refreshPending)
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)

	stale := newDetail
	stale.Summary.TaskPreview = "stale task"
	model.Update(subagentRouteDataMsg{
		generation: 6, childSessionID: "child-1",
		background: true, hasDetail: true, detail: stale,
	})
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)

	model.Update(subagentRouteDataMsg{
		generation: 7, childSessionID: "child-1",
		background: true, err: errors.New("temporary inspect failure"),
	})
	assert.Equal(t, "new task", model.route.detail.Summary.TaskPreview)
	assert.EqualError(t, model.route.refreshErr, "temporary inspect failure")
}

func TestSubagentDetailRefreshFiltersUnrelatedChildren(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	detail := subagent.Detail{Summary: subagent.Summary{ChildSessionID: "child-1"}}
	model.route = routeState{
		kind: routeSubagent, generation: 1, childSessionID: "child-1",
		detail: &detail,
	}

	command := model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-2"},
	}})
	assert.Nil(t, command)
	assert.False(t, model.route.refreshing)

	command = model.invalidateAgentDetail(streamItem{event: coding.Event{
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-1"},
	}})
	require.NotNil(t, command)
	assert.True(t, model.route.refreshing)
}
