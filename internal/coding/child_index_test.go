//nolint:wsl_v5 // Runtime index fixtures keep the spawn and its durable record adjacent.
package coding

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeIndexesSpawnedChildBesideItsParent drives a real Runtime through one
// delegation and pins the durable parent-side record it leaves: the child's
// identity and terminal state, beside the child's own session in the parent's
// group.
func TestRuntimeIndexesSpawnedChildBesideItsParent(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"delegate-1",
			subagent.ToolName,
			`{"prompt":"Locate the runtime composition root.","description":"delegate",`+
				`"subagent_type":"explore","run_in_background":false}`,
		),
		runtimeTextResponse("Located the runtime composition root."),
		runtimeTextResponse("parent answer"),
	)
	runtime := openTestRuntime(t, model)
	parent := runtime.handle.Metadata()

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	childID := summaries[0].ChildSessionID
	assert.Equal(t, subagent.StateSucceeded, summaries[0].State)

	entry, err := session.ReadChildIndexEntry(
		runtime.repository.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, childID),
		parent.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, childID, entry.ChildSessionID)
	assert.Equal(t, parent.ID, entry.ParentSessionID)
	assert.Equal(t, "explore", entry.AgentID)
	assert.Equal(t, string(subagent.AgentKindBuiltin), entry.AgentKind)
	assert.Equal(t, session.ChildIndexStateSucceeded, entry.State)
	assert.False(t, entry.EndedAt.IsZero())

	// The first listing completed the index, so the marker sits beside the entry
	// and the child's own session stays a sibling of its parent's.
	assert.FileExists(t,
		runtime.repository.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID),
	)
	assert.FileExists(t,
		runtime.repository.Layout().MetadataFile(parent.WorkspacePath, childID),
	)

	// A child session is still hidden from the session picker: the index adds a
	// parent-side lookup, not a second listing surface.
	conversations, err := runtime.repository.List(t.Context())
	require.NoError(t, err)
	for _, meta := range conversations {
		assert.NotEqual(t, childID, meta.ID, "a child never appears in the session list")
	}
}
