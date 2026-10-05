package tui

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolProjectionReusesAndInvalidatesVisibleInputs(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"command":"pwd"}`)},
		Status: coding.ToolStatusCompleted, Result: codingToolResultFor("call-1", "shell", "first result"),
	}}

	var cache toolProjectionCache

	first := projectToolActivitiesCached(state, nil, &cache)
	require.Len(t, first, 1)
	require.NotEmpty(t, first[0].preview)

	again := projectToolActivitiesCached(state.Clone(), nil, &cache)
	assert.Equal(t, first, again)
	assert.Same(t, &first[0].preview[0], &again[0].preview[0], "unchanged preview is reused")

	state.Tools[0].Result = codingToolResultFor("call-1", "shell", "other result")
	updated := projectToolActivitiesCached(state, nil, &cache)
	assert.Equal(t, projectToolActivities(state, nil), updated)
	assert.Contains(t, updated[0].body, "other result")

	state.Tools = nil
	assert.Empty(t, projectToolActivitiesCached(state, nil, &cache))
	assert.Empty(t, cache.entries, "removed calls release their cached output")
}
