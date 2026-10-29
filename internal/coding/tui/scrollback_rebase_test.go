package tui

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebaseWindowBasesMovesCursorsAndBookkeeping(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{NoColor: true})
	model.scrollback = scrollbackCursor{
		messages: 100, tools: 10,
		toolIDs: map[string]struct{}{"old-1": {}, "call-1": {}},
	}
	model.state.Tools = []coding.ToolState{{Call: coding.ToolCall{ID: "call-1"}}}
	model.completionMarkers = []completionMarker{
		{interactionID: "old", afterMessages: 3},
		{interactionID: "kept", afterMessages: 40},
	}
	model.scrollback.completions = map[string]struct{}{"old": {}, "kept": {}}
	model.questionPreviews = map[string]questionPreview{
		"old":  {position: 3},
		"kept": {position: 60},
	}

	model.rebaseWindowBases(30, 2)

	assert.Equal(t, 70, model.scrollback.messages)
	assert.Equal(t, 8, model.scrollback.tools)
	assert.Equal(t, 30, model.scrollback.transcriptBase)
	assert.Equal(t, 2, model.scrollback.toolsBase)
	assert.Equal(t, []completionMarker{{interactionID: "kept", afterMessages: 10}},
		model.completionMarkers)
	assert.NotContains(t, model.scrollback.completions, "old")
	assert.Contains(t, model.scrollback.completions, "kept")
	assert.NotContains(t, model.questionPreviews, "old")
	assert.Equal(t, 30, model.questionPreviews["kept"].position)
	assert.NotContains(t, model.scrollback.toolIDs, "old-1")
	assert.Contains(t, model.scrollback.toolIDs, "call-1")
	assert.True(t, model.history.more, "an unloaded older page became available")
}

func TestRebaseWindowBasesReloadsHistoryAcrossATrim(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{NoColor: true})
	model.history = historyState{
		messages: []ai.Message{ai.UserText("loaded")}, start: 100, more: false,
	}

	model.rebaseWindowBases(5, 0)
	assert.True(t, model.history.more)
	require.Empty(t, model.history.messages,
		"a history window that ended at the old base is reloaded above the new one")
	assert.Zero(t, model.history.start)
}

func TestRebaseWindowBasesClampsCursors(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{NoColor: true})
	model.scrollback = scrollbackCursor{messages: 2, tools: 1}
	model.rebaseWindowBases(10, 5)

	assert.Zero(t, model.scrollback.messages)
	assert.Zero(t, model.scrollback.tools)
}

func TestPruneSubagentCursorsKeepsOnlyProjectedChildren(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{NoColor: true})
	model.state.Subagents = []coding.SubagentState{{ChildSessionID: "live"}}
	model.scrollback.subagentStarted = map[string]string{"live": "task", "old": "task"}
	model.scrollback.subagentTerminal = map[string]struct{}{"live": {}, "old": {}}

	model.pruneSubagentCursors()

	assert.Contains(t, model.scrollback.subagentStarted, "live")
	assert.NotContains(t, model.scrollback.subagentStarted, "old")
	assert.Contains(t, model.scrollback.subagentTerminal, "live")
	assert.NotContains(t, model.scrollback.subagentTerminal, "old")
}
