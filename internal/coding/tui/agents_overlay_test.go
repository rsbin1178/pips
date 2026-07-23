//nolint:wsl_v5 // Detail fixtures keep transcript setup next to disclosure assertions.
package tui

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
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
	}
	model := readyModel(t, true)

	content := model.agentDetailContent(detail)
	assert.Contains(t, content, "• Explored")
	assert.Contains(t, content, "Read model.go")
	assert.Contains(t, content, "Inspection complete.")
	assert.NotContains(t, content, reasoning)
	assert.NotContains(t, content, "[tool")
	assert.NotContains(t, content, `{"path"`)
	assert.NotContains(t, content, "pips.coding.tool_result")
}
