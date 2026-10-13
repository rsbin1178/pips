package subagent

import (
	"context"
	"os"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readChildIndexEntry reads the parent-side record of one child: the durable
// entry a parent-side lookup reads instead of scanning the store.
func readChildIndexEntry(
	t *testing.T,
	fixture *managerFixture,
	childID string,
) session.ChildIndexEntry {
	t.Helper()

	parent := fixture.parent.Metadata()
	entry, err := session.ReadChildIndexEntry(
		fixture.repository.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, childID),
		parent.ID,
	)
	require.NoError(t, err)

	return entry
}

// materializeParent gives the parent conversation the durable record every real
// parent has before it delegates: a spawn only indexes a child whose parent
// already owns a session record.
func materializeParent(t *testing.T, fixture *managerFixture) {
	t.Helper()

	_, err := fixture.parent.Session().AppendMessage(ai.UserText("delegate the work"), nil)
	require.NoError(t, err)
}

// TestManagerIndexesTheChildItSpawns pins the parent-side record of a spawn: the
// child's identity snapshot, the running state while it owns its session, and the
// terminal state once it stops.
func TestManagerIndexesTheChildItSpawns(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	materializeParent(t, fixture)
	fixture.blockChildren()

	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Inspect the tree."}, nil,
	)
	require.NoError(t, err)
	<-fixture.started

	childID := execution.child.Metadata().ID
	entry := readChildIndexEntry(t, fixture, childID)
	assert.Equal(t, childID, entry.ChildSessionID)
	assert.Equal(t, fixture.parent.Metadata().ID, entry.ParentSessionID)
	assert.Equal(t, string(RoleExplore), entry.AgentID)
	assert.Equal(t, string(AgentKindBuiltin), entry.AgentKind)
	assert.Equal(t, session.ChildIndexStateRunning, entry.State)
	assert.True(t, entry.EndedAt.IsZero(), "a running child has not ended")

	execution.Cancel()
	result, err := execution.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, OutcomeCanceled, result.Outcome)

	entry = readChildIndexEntry(t, fixture, childID)
	assert.Equal(t, session.ChildIndexStateCanceled, entry.State)
	assert.False(t, entry.EndedAt.IsZero(), "a terminal child records when it stopped")
}

// TestManagerKeepsIndexedChildrenListable pins that the index does not replace
// the manager's own listing: the same child is durable, indexed, and listed.
func TestManagerKeepsIndexedChildrenListable(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	materializeParent(t, fixture)

	execution, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "Inspect the tree."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, result.Outcome)

	entry := readChildIndexEntry(t, fixture, result.ChildSessionID)
	assert.Equal(t, string(RoleExplore), entry.AgentID)
	assert.Equal(t, string(AgentKindBuiltin), entry.AgentKind)
	assert.Equal(t, session.ChildIndexStateSucceeded, entry.State)
	assert.False(t, entry.EndedAt.IsZero())

	values, err := fixture.manager.List(t.Context())
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, result.ChildSessionID, values[0].ChildSessionID)
	assert.Equal(t, StateSucceeded, values[0].State)
}

// TestManagerIndexesAResumedChild pins that a resume produces its own
// parent-side entry, exactly like a fresh spawn.
func TestManagerIndexesAResumedChild(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	materializeParent(t, fixture)

	source, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "First pass."}, nil,
	)
	require.NoError(t, err)
	first, err := source.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, first.Outcome)

	resumed, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Second pass.", ResumeFrom: first.ChildSessionID,
	}, nil)
	require.NoError(t, err)
	second, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, second.Outcome)
	require.NotEqual(t, first.ChildSessionID, second.ChildSessionID)

	entry := readChildIndexEntry(t, fixture, second.ChildSessionID)
	assert.Equal(t, fixture.parent.Metadata().ID, entry.ParentSessionID)
	assert.Equal(t, string(RoleExplore), entry.AgentID)
	assert.Equal(t, session.ChildIndexStateSucceeded, entry.State)

	// The resumed child is a child of the same parent, so the lineage still
	// lists both runs.
	values, err := fixture.manager.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, values, 2)
}

// TestManagerLifecycleSurvivesAnUnwritableChildIndex pins the best-effort rule
// end to end: an index that cannot be written changes no spawn, no transition,
// and no listing.
func TestManagerLifecycleSurvivesAnUnwritableChildIndex(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	materializeParent(t, fixture)

	first, err := fixture.start(
		t.Context(), Request{AgentID: "explore", Task: "First pass."}, nil,
	)
	require.NoError(t, err)
	firstResult, err := first.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, firstResult.Outcome)

	parent := fixture.parent.Metadata()
	indexDir := fixture.repository.Layout().SubagentsDir(parent.WorkspacePath, parent.ID)
	require.DirExists(t, indexDir)
	require.NoError(t, os.Chmod(indexDir, 0o500))       //nolint:gosec // the test owns this directory
	t.Cleanup(func() { _ = os.Chmod(indexDir, 0o700) }) //nolint:gosec // the test owns this directory

	second, err := fixture.start(
		t.Context(), Request{AgentID: "plan", Task: "Second pass."}, nil,
	)
	require.NoError(t, err)
	secondResult, err := second.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, secondResult.Outcome)

	values, err := fixture.manager.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, values, 2, "an unwritable index degrades to the scan")
}
