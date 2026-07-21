//nolint:wsl_v5 // Disclosure assertions follow rendered fixtures directly.
package tui

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineNeverProjectsReasoningOrSignatures(t *testing.T) {
	t.Parallel()

	const secret = "reasoning-signature-secret"
	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("question"),
			ai.Assistant(
				ai.Text("visible answer"),
				ai.ReasoningPart{Text: secret, Signature: secret},
			),
		},
		Draft: []coding.MessageDelta{
			{Kind: ai.StreamReasoningDelta, Text: secret, Signature: secret},
			{Kind: ai.StreamTextDelta, Text: "visible draft"},
		},
		Tools: []coding.ToolState{{
			Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
			Status: coding.ToolStatusRunning,
		}},
	}

	blocks := projectTimeline(state)
	rendered := renderTimeline(
		blocks,
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "question")
	assert.Contains(t, rendered, "visible answer")
	assert.Contains(t, rendered, "visible draft")
	assert.Contains(t, rendered, "read_file · running")
	assert.NotContains(t, rendered, secret)
}

func TestTimelineSummarizesChangesAndDiagnostics(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Changes: &coding.WorkspaceChanged{
			Entries:   []coding.WorkspaceChange{{Path: "main.go"}},
			Truncated: true,
		},
		Diagnostics: []coding.IntegrationDiagnostic{{
			Component: "mcp",
			Code:      "disabled",
			Message:   "server unavailable",
		}},
	}

	rendered := renderTimeline(
		projectTimeline(state),
		newMarkdownRenderer(8),
		80,
		themeDark,
		true,
	)
	assert.Contains(t, rendered, "1 workspace change(s) · diff truncated")
	assert.Contains(t, rendered, "mcp · disabled")
	assert.NotContains(t, rendered, "diff --git")
}

func TestTimelinePlacesCompletedToolBeforeFollowingAnswer(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{
			ai.UserText("read it"),
			ai.ToolResultText("call-1", "read_file", "file content"),
			ai.AssistantText("final answer"),
		},
		Tools: []coding.ToolState{{
			Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
			Status: coding.ToolStatusCompleted,
		}},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 3)
	assert.Equal(t, []blockKind{blockUser, blockTool, blockAssistant}, []blockKind{
		blocks[0].kind,
		blocks[1].kind,
		blocks[2].kind,
	})
}
