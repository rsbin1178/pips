package tui

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletedToolDetailReadsTheDurableResult pins the P0-4 split: a
// completed ToolState carries no live progress body, and the detail view is
// still built from its durable result.
func TestCompletedToolDetailReadsTheDurableResult(t *testing.T) {
	t.Parallel()

	result := ai.ToolResultText("call-result", "shell", "durable output")
	state := coding.State{Tools: []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-result", Name: "shell",
			Arguments: ai.JSON(`{"command":"printf durable"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: result,
	}}}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	require.Len(t, blocks[0].tools, 1)
	assert.Empty(t, blocks[0].tools[0].update)

	detail := newToolDetailView(blocks[0]).text()
	assert.Contains(t, detail, "durable output")
}
