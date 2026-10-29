package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedToolState is the shape the reducer produces after a tool result
// commits: the transcript owns the part and the ToolState shares its content.
func sharedToolState(id, name string, arguments ai.JSON, content []ai.Part) coding.State {
	part := ai.ToolResultPart{ToolCallID: id, Name: name, Content: content}

	state := readyState()
	state.Transcript = []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: id, Name: name, Args: arguments}),
		ai.ToolMessage{Parts: []ai.ToolResultPart{part}},
	}
	state.Tools = []coding.ToolState{{
		RunID: "run-1", Turn: 1,
		Call:   coding.ToolCall{ID: id, Name: name, Arguments: arguments},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolMessage{Parts: []ai.ToolResultPart{part}},
	}}

	return state
}

// TestSharedToolResultFeedsTheToolCardAndDetail pins that a ToolState sharing
// the transcript's result part still renders the card and the detail page.
func TestSharedToolResultFeedsTheToolCardAndDetail(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("z", 4096)
	state := sharedToolState("call-share", "read", ai.JSON(`{"path":"big.txt"}`),
		[]ai.Part{ai.Text(body)})

	blocks := projectTimeline(state)
	require.Len(t, blocks, 1)
	assert.Equal(t, blockTool, blocks[0].kind)

	detail := newToolDetailView(blocks[0])
	text := detailRowsText(detail.rows)
	assert.Contains(t, text, "big.txt")
	assert.Contains(t, text, body[:64], "the shared result reaches the detail page")
}

// TestSharedToolResultParsesTheSubagentChildSession covers the reader that
// decodes the child identity out of a completed spawn result.
func TestSharedToolResultParsesTheSubagentChildSession(t *testing.T) {
	t.Parallel()

	state := sharedToolState("call-sub", subagent.ToolName, ai.JSON(`{"prompt":"inspect","subagent_type":"explore"}`),
		[]ai.Part{ai.Text(`{"schema":"` + subagent.ResultSchema + `","role":"explore","child_session_id":"child-9"}`)})

	require.Len(t, state.Tools, 1)
	assert.Equal(t, "child-9", subagentChildSessionID(state.Tools[0], nil))

	blocks := projectTimeline(state)
	require.NotEmpty(t, blocks)
	assert.Equal(t, "child-9", blocks[0].tools[0].childSessionID)
}

// TestSharedToolResultJSONRoundTrip pins that sharing does not change what the
// State marshals to.
func TestSharedToolResultJSONRoundTrip(t *testing.T) {
	t.Parallel()

	state := sharedToolState("call-share", "read", ai.JSON(`{"path":"big.txt"}`),
		[]ai.Part{ai.Text(strings.Repeat("z", 1024))})

	shared, err := json.Marshal(state)
	require.NoError(t, err)
	cloned, err := json.Marshal(state.Clone())
	require.NoError(t, err)
	assert.JSONEq(t, string(shared), string(cloned))
}

// TestReleasedToolProjectionRebuildsFromTheTranscript pins what the Tools byte
// window must not take with it. The reducer releases a completed ToolState (see
// coding.TestToolsByteWindowReleasesTheProjectionNotTheTranscript) while the
// call and result messages stay durable, so the card and the detail page have to
// come back out of the transcript alone.
func TestReleasedToolProjectionRebuildsFromTheTranscript(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("z", 4096)
	arguments := ai.JSON(`{"path":"big.txt"}`)
	content := []ai.Part{ai.Text(body)}

	projected := sharedToolState("call-released", "read", arguments, content)
	released := sharedToolState("call-released", "read", arguments, content)
	released.Tools = nil

	require.Len(t, projected.Tools, 1)

	model := readyModel(t, true)
	before := projectTimeline(projected)
	after := projectTimeline(released)

	require.Len(t, after, 1)
	assert.Equal(t, blockTool, after[0].kind)
	require.Len(t, after[0].tools, 1)
	assert.True(t, after[0].tools[0].settled,
		"a durable result with no live projection is final, so the store may reuse its rows")

	// Only the live-only run/turn metadata differs: the transcript does not carry
	// it, and it feeds the child-session lookup and the projection cache key
	// rather than the card the reader sees.
	assert.Equal(t, model.renderTimelineBlocks(before), model.renderTimelineBlocks(after),
		"the released card renders identically from the transcript")

	assert.Equal(t, detailRowsText(newToolDetailView(before[0]).rows),
		detailRowsText(newToolDetailView(after[0]).rows), "the detail page rebuilds from the transcript")
}
