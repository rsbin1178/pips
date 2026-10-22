//nolint:wsl_v5 // View fixtures and their branches stay adjacent.
package harness_test

import (
	"errors"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewStore is a StoreView whose Entries method fails the test if called: a
// Session over it must share the history instead of asking for a copy.
type viewStore struct {
	entries []harness.Entry
	appends int
}

func (s *viewStore) Metadata() harness.SessionMetadata {
	return harness.SessionMetadata{ID: "view-store"}
}

func (s *viewStore) Append(entry harness.Entry) error {
	s.entries = append(s.entries, entry)
	s.appends++

	return nil
}

func (s *viewStore) Entries() ([]harness.Entry, error) {
	return nil, errors.New("Entries must not be called for a StoreView")
}

func (s *viewStore) EntryView() []harness.Entry {
	return s.entries
}

func TestSessionSharesStoreViewHistory(t *testing.T) {
	t.Parallel()

	store := &viewStore{}
	session, err := harness.NewSession(store)
	require.NoError(t, err)

	first, err := session.AppendMessage(ai.UserText("first"), nil)
	require.NoError(t, err)
	second, err := session.AppendMessage(ai.AssistantText("second"), nil)
	require.NoError(t, err)
	require.Equal(t, 2, store.appends)

	view := session.EntriesView()
	require.Len(t, view, 2)
	assert.Equal(t, first, view[0].ID)
	assert.Equal(t, second, view[1].ID)
	assert.Same(t, &view[0], &store.entries[0], "the view is the store's retained history")
	assert.Equal(t, []string{first, second}, entryIDs(session.Path()))

	contextValue, err := session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText("first"), ai.AssistantText("second")}, contextValue.Messages)

	// A defensive read still owns its copy.
	copied := session.Entries()
	copied[0].ID = "changed"
	assert.Equal(t, first, session.EntriesView()[0].ID)
}

func TestSessionContextAndPathFollowAppendsAndNavigation(t *testing.T) {
	t.Parallel()

	store := harness.NewMemoryStore("projections")
	session, err := harness.NewSession(store)
	require.NoError(t, err)

	root, err := session.AppendMessage(ai.UserText("root"), nil)
	require.NoError(t, err)
	branch, err := session.AppendMessage(ai.AssistantText("branch"), nil)
	require.NoError(t, err)
	require.Equal(t, []string{root, branch}, entryIDs(session.Path()))

	contextValue, err := session.Context()
	require.NoError(t, err)
	require.Equal(t, ai.Messages{ai.UserText("root"), ai.AssistantText("branch")}, contextValue.Messages)

	// An ordinary append extends both views.
	_, err = session.AppendMessage(ai.UserText("next"), nil)
	require.NoError(t, err)
	contextValue, err = session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{
		ai.UserText("root"), ai.AssistantText("branch"), ai.UserText("next"),
	}, contextValue.Messages)

	// Navigating to an earlier entry re-points the branch; a stale cache must
	// not leak the abandoned tail.
	require.NoError(t, session.MoveTo(root, "abandoned branch"))

	contextValue, err = session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{
		ai.UserText("root"), ai.UserText(harness.BranchSummaryPrefix + "abandoned branch"),
	}, contextValue.Messages)
	assert.Len(t, session.Path(), 2)
	assert.NotContains(t, entryIDs(session.Path()), branch)
}

func TestSessionContextTracksCompactionAndCheckpoints(t *testing.T) {
	t.Parallel()

	session, err := harness.NewSession(harness.NewMemoryStore("replacements"))
	require.NoError(t, err)

	first, err := session.AppendMessage(ai.UserText("old"), nil)
	require.NoError(t, err)
	keep, err := session.AppendMessage(ai.AssistantText("kept"), nil)
	require.NoError(t, err)

	// Materialize the cache before the replacement so extension must invalidate.
	_, err = session.Context()
	require.NoError(t, err)

	_, err = session.AppendCompaction("summary", keep, 10)
	require.NoError(t, err)
	contextValue, err := session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{
		ai.UserText(harness.CompactionPrefix + "summary"), ai.AssistantText("kept"),
	}, contextValue.Messages)

	checkpoint := harness.ContextCheckpoint{
		Version:   harness.ContextCheckpointVersion,
		ArchiveID: "archive_0123456789-abcd",
		Messages:  ai.Messages{ai.UserText("seed")},
	}
	_, err = session.AppendContextCheckpoint(session.LeafID(), checkpoint, 5)
	require.NoError(t, err)
	contextValue, err = session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText("seed")}, contextValue.Messages)

	_, err = session.AppendMessage(ai.AssistantText("after seed"), nil)
	require.NoError(t, err)
	contextValue, err = session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText("seed"), ai.AssistantText("after seed")}, contextValue.Messages)
	// The raw branch still stores the ancestors a checkpoint excluded from
	// model context; only the reconstructed context is replaced.
	assert.Contains(t, entryIDs(session.Path()), first)
}

func entryIDs(entries []harness.Entry) []string {
	ids := make([]string, len(entries))
	for index, entry := range entries {
		ids[index] = entry.ID
	}

	return ids
}
