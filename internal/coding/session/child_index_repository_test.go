//nolint:wsl_v5 // Durable index fixtures keep store writes and their assertions adjacent.
package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIndexedParent returns a stored conversation whose session record exists, so
// its children can be indexed beside it.
func newIndexedParent(t *testing.T) (*session.Repository, session.Metadata) {
	t.Helper()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = parent.Session().AppendMessage(ai.UserText("delegate work"), nil)
	require.NoError(t, err)
	meta := parent.Metadata()
	require.NoError(t, parent.Close())

	return repo, meta
}

// newIndexedChild stores one completed child session and closes it, so the
// fixture reads it exactly the way a listing does.
func newIndexedChild(
	t *testing.T,
	repo *session.Repository,
	parent session.Metadata,
	agent string,
) session.Metadata {
	t.Helper()

	child, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: parent.WorkspaceID, WorkspacePath: parent.WorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parent.ID, ParentRunID: "run-parent",
		Agent: agent, SubagentIdentity: testSubagentIdentity(agent),
	})
	require.NoError(t, err)
	_, err = child.Session().AppendMessage(ai.UserText("inspect "+agent), nil)
	require.NoError(t, err)
	meta := child.Metadata()
	require.NoError(t, child.Close())

	return meta
}

// forgetChildIndex removes a parent's whole index directory, which is the shape a
// store written before this phase has.
func forgetChildIndex(t *testing.T, repo *session.Repository, parent session.Metadata) {
	t.Helper()

	require.NoError(t, os.RemoveAll(
		repo.Layout().SubagentsDir(parent.WorkspacePath, parent.ID),
	))
}

// forgetChildIndexMarker drops one parent's completeness marker, which forces the
// next read onto the store-wide scan.
func forgetChildIndexMarker(t *testing.T, repo *session.Repository, parent session.Metadata) {
	t.Helper()

	err := os.Remove(repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID))
	if err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func readChildIndexEntry(
	t *testing.T,
	repo *session.Repository,
	parent session.Metadata,
	childID string,
) session.ChildIndexEntry {
	t.Helper()

	entry, err := session.ReadChildIndexEntry(
		repo.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, childID), parent.ID,
	)
	require.NoError(t, err)

	return entry
}

func childIDs(metas []session.Metadata) []string {
	ids := make([]string, 0, len(metas))
	for _, meta := range metas {
		ids = append(ids, meta.ID)
	}

	return ids
}

// TestRepositoryChildIndexIsWrittenWithTheChildSession pins the create-time
// shape: a child's first entry publishes the parent-side record before the
// child's own, and a spawn never writes the completeness marker.
func TestRepositoryChildIndexIsWrittenWithTheChildSession(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	child := newIndexedChild(t, repo, parent, "explore")

	entry := readChildIndexEntry(t, repo, parent, child.ID)
	assert.Equal(t, child.ID, entry.ChildSessionID)
	assert.Equal(t, parent.ID, entry.ParentSessionID)
	assert.Equal(t, "run-parent", entry.ParentRunID)
	assert.Equal(t, "explore", entry.AgentID)
	assert.Equal(t, "builtin", entry.AgentKind)
	assert.Empty(t, entry.State,
		"a child that was only created carries no state; the lifecycle mirror writes it")
	assert.True(t, entry.CreatedAt.Equal(child.CreatedAt))
	assert.NoFileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID),
		"only a completed scan writes the marker")

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{child.ID}, childIDs(children))
	assert.FileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID),
		"the first read backfills the index")
}

// TestRepositoryChildIndexFallsBackToTheScanAndThenUsesTheIndex pins both read
// paths and the documented stale-index behaviour: a child that appears without an
// entry after the marker is written stays invisible until the marker is dropped,
// and the read that drops it re-derives the index.
func TestRepositoryChildIndexFallsBackToTheScanAndThenUsesTheIndex(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	first := newIndexedChild(t, repo, parent, "explore")

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{first.ID}, childIDs(children))
	assert.FileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID))

	// A store written before this phase has no index directory at all: the first
	// read scans, backfills, and only then claims completeness.
	forgetChildIndex(t, repo, parent)
	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{first.ID}, childIDs(children))
	assert.FileExists(t,
		repo.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, first.ID),
		"the backfill rewrote the missing entry",
	)
	assert.Empty(t, readChildIndexEntry(t, repo, parent, first.ID).State,
		"a backfilled entry carries no state: the scan that wrote it cannot know one")
	assert.FileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID))

	// A child that appears without an entry after the marker is written is not
	// visible through the index. That divergence is loud rather than silent: the
	// parent-journal cross-check reports it, and removing the marker recovers.
	second := newIndexedChild(t, repo, parent, "plan")
	require.NoError(t, os.RemoveAll(
		repo.Layout().ChildIndexEntryDir(parent.WorkspacePath, parent.ID, second.ID),
	))
	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{first.ID}, childIDs(children),
		"an unindexed child stays invisible until the marker is dropped")

	forgetChildIndexMarker(t, repo, parent)
	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{second.ID, first.ID}, childIDs(children),
		"the fallback scan re-derives every child")

	// The backfill made the index complete again, so the next read takes it.
	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{second.ID, first.ID}, childIDs(children))
}

// TestRepositoryListSubagentsMatchesTheScanForEveryIndexFixture pins the
// equivalence invariant over the anomaly fixtures: whatever the index holds, the
// indexed read returns exactly what the store-wide scan returns, field by field.
func TestRepositoryListSubagentsMatchesTheScanForEveryIndexFixture(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup func(t *testing.T, repo *session.Repository, parent session.Metadata)
		want  int
	}{
		"no index at all": {
			setup: func(*testing.T, *session.Repository, session.Metadata) {},
			want:  1,
		},
		"a legacy child with no entry": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				newIndexedChild(t, repo, parent, "plan")
				forgetChildIndex(t, repo, parent)
			},
			want: 2,
		},
		"an entry whose child session is gone": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				gone := newIndexedChild(t, repo, parent, "plan")
				require.NoError(t, os.RemoveAll(
					repo.Layout().SessionDir(parent.WorkspacePath, gone.ID),
				))
			},
			want: 1,
		},
		"an entry that belongs to another parent": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				other, err := repo.Create(t.Context(), session.CreateOptions{
					WorkspaceID: parent.WorkspaceID, WorkspacePath: parent.WorkspacePath,
				})
				require.NoError(t, err)
				_, err = other.Session().AppendMessage(ai.UserText("other parent"), nil)
				require.NoError(t, err)
				otherMeta := other.Metadata()
				require.NoError(t, other.Close())

				foreign := newIndexedChild(t, repo, otherMeta, "plan")
				require.NoError(t, os.Rename(
					repo.Layout().ChildIndexEntryDir(parent.WorkspacePath, otherMeta.ID, foreign.ID),
					repo.Layout().ChildIndexEntryDir(parent.WorkspacePath, parent.ID, foreign.ID),
				))
			},
			want: 1,
		},
		"a Team Worker": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				worker, err := repo.Create(t.Context(), session.CreateOptions{
					WorkspaceID: parent.WorkspaceID, WorkspacePath: parent.WorkspacePath,
					Kind: session.KindTeamWorker,
					TeamWorker: &session.TeamWorkerLineage{
						ParentSessionID: parent.ID, TeamID: "team-one", MemberID: "member-one",
						TaskID: "task-one", AttemptID: "attempt-one",
						ContinuationID: "continuation-one",
					},
				})
				require.NoError(t, err)
				_, err = worker.Session().AppendMessage(ai.UserText("worker task"), nil)
				require.NoError(t, err)
				require.NoError(t, worker.Close())
			},
			want: 1,
		},
		"a child whose record is unreadable": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				damaged := newIndexedChild(t, repo, parent, "plan")
				require.NoError(t, os.WriteFile(
					repo.Layout().MetadataFile(parent.WorkspacePath, damaged.ID),
					[]byte("not a session record"), 0o600,
				))
			},
			want: 1,
		},
		"an index directory that exists but is empty": {
			setup: func(t *testing.T, repo *session.Repository, parent session.Metadata) {
				t.Helper()

				require.NoError(t, os.MkdirAll(
					repo.Layout().SubagentsDir(parent.WorkspacePath, parent.ID), 0o700,
				))
			},
			want: 1,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo, parent := newIndexedParent(t)
			newIndexedChild(t, repo, parent, "explore")
			test.setup(t, repo, parent)

			forgetChildIndexMarker(t, repo, parent)
			scanned, err := repo.ListSubagents(t.Context(), parent)
			require.NoError(t, err)
			require.Len(t, scanned, test.want)

			indexed, err := repo.ListSubagents(t.Context(), parent)
			require.NoError(t, err)
			assert.Equal(t, scanned, indexed, "the indexed read matches the scan")
		})
	}
}

// TestRepositoryListSubagentsRepairsEntriesTheChildRecordContradicts pins the
// repair rule: the child's own record wins, and an index that lost an entry stops
// claiming completeness.
func TestRepositoryListSubagentsRepairsEntriesTheChildRecordContradicts(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	child := newIndexedChild(t, repo, parent, "explore")

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	require.Len(t, children, 1)

	// Rewrite the child's record so it no longer names this parent.
	recordPath := repo.Layout().MetadataFile(parent.WorkspacePath, child.ID)
	record, err := session.ReadMetadataRecord(recordPath)
	require.NoError(t, err)
	record.Parent.SessionID = "s-other-parent"
	require.NoError(t, session.WriteMetadataRecord(recordPath, record))

	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Empty(t, children)
	assert.NoFileExists(t,
		repo.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, child.ID),
		"a contradicting entry is removed",
	)
	assert.NoFileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID),
		"removing an entry drops the marker",
	)
}

// TestRepositoryListSubagentsKeepsEntriesWhoseChildRecordIsUnreadable pins the
// other half of the rule: an unreadable record proves nothing, so the entry stays
// for the spawn or the repair that will fix it.
func TestRepositoryListSubagentsKeepsEntriesWhoseChildRecordIsUnreadable(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	child := newIndexedChild(t, repo, parent, "explore")

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	require.Len(t, children, 1)

	recordPath := repo.Layout().MetadataFile(parent.WorkspacePath, child.ID)
	require.NoError(t, os.WriteFile(recordPath, []byte("not a session record"), 0o600))

	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Empty(t, children, "an unreadable record is excluded, exactly as the scan excludes it")
	assert.FileExists(t,
		repo.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, child.ID),
		"an unreadable record leaves the entry alone",
	)
	assert.FileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID))
}

// TestRepositoryUpdateChildIndexStateRefreshesTheEntry pins the lifecycle cache:
// the journal owns the truth and this only mirrors it onto the parent's record.
func TestRepositoryUpdateChildIndexStateRefreshesTheEntry(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	child := newIndexedChild(t, repo, parent, "explore")
	endedAt := time.Now().UTC().Truncate(time.Millisecond)

	assert.Empty(t, readChildIndexEntry(t, repo, parent, child.ID).State,
		"the lifecycle mirror is the only writer of a state")

	require.NoError(t, repo.UpdateChildIndexState(child, session.ChildIndexStateSucceeded, endedAt))
	entry := readChildIndexEntry(t, repo, parent, child.ID)
	assert.Equal(t, session.ChildIndexStateSucceeded, entry.State)
	assert.True(t, entry.EndedAt.Equal(endedAt))

	// An unknown state is a programming error, not a lifecycle transition.
	require.ErrorIs(t,
		repo.UpdateChildIndexState(child, "finished", endedAt),
		session.ErrInvalid,
	)
	require.ErrorIs(t,
		repo.UpdateChildIndexState(session.Metadata{}, session.ChildIndexStateFailed, endedAt),
		session.ErrInvalid,
	)

	// A child with no entry has nothing to refresh, and that is not an error the
	// lifecycle may observe.
	legacy := newIndexedChild(t, repo, parent, "plan")
	forgetChildIndex(t, repo, parent)
	require.Error(t, repo.UpdateChildIndexState(legacy, session.ChildIndexStateFailed, endedAt))
}

// TestRepositoryListSubagentsSurvivesAnUnwritableIndex pins the best-effort rule
// at the store level: a parent whose index directory cannot be written still
// lists and still spawns, and never claims a completeness it does not have.
func TestRepositoryListSubagentsSurvivesAnUnwritableIndex(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	first := newIndexedChild(t, repo, parent, "explore")
	indexDir := repo.Layout().SubagentsDir(parent.WorkspacePath, parent.ID)
	require.DirExists(t, indexDir)

	require.NoError(t, os.Chmod(indexDir, 0o500))       //nolint:gosec // the test owns this directory
	t.Cleanup(func() { _ = os.Chmod(indexDir, 0o700) }) //nolint:gosec // the test owns this directory

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{first.ID}, childIDs(children))
	assert.NoFileExists(t, repo.Layout().SubagentIndexFile(parent.WorkspacePath, parent.ID),
		"an index that cannot be completed is never marked complete",
	)

	second := newIndexedChild(t, repo, parent, "plan")
	children, err = repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{second.ID, first.ID}, childIDs(children),
		"an unwritable index degrades to the scan instead of failing the listing",
	)
	assert.NoFileExists(t,
		repo.Layout().ChildIndexEntryFile(parent.WorkspacePath, parent.ID, second.ID),
		"the failed entry write is not fatal",
	)
}

// TestRepositoryForkSubagentIndexesTheResumedChild pins that a resume produces its
// own parent-side entry, exactly like a fresh child.
func TestRepositoryForkSubagentIndexesTheResumedChild(t *testing.T) {
	t.Parallel()

	repo, parent := newIndexedParent(t)
	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: parent.WorkspaceID, WorkspacePath: parent.WorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parent.ID, ParentRunID: "run-parent",
		Agent: "explore", SubagentIdentity: testSubagentIdentity("explore"),
	})
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.UserText("first task"), nil)
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.AssistantText("first answer"), nil)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	reopened, err := repo.Open(t.Context(), session.OpenOptions{
		ID: source.Metadata().ID, WorkspaceID: parent.WorkspaceID,
	})
	require.NoError(t, err)
	identity := testSubagentIdentity("explore")
	identity.PlanDigest = strings.Repeat("c", 64)
	resumed, err := repo.ForkSubagent(t.Context(), reopened, session.ForkSubagentOptions{
		ParentSessionID: parent.ID, ParentRunID: "run-parent",
		Agent: "explore", SubagentIdentity: identity,
	})
	require.NoError(t, err)
	resumedMeta := resumed.Metadata()
	require.NoError(t, resumed.Close())
	require.NoError(t, reopened.Close())

	entry := readChildIndexEntry(t, repo, parent, resumedMeta.ID)
	assert.Equal(t, parent.ID, entry.ParentSessionID)
	assert.Equal(t, "run-parent", entry.ParentRunID)
	assert.Equal(t, "explore", entry.AgentID)
	assert.Empty(t, entry.State, "a resumed child is indexed exactly like a fresh one, with no state yet")

	children, err := repo.ListSubagents(t.Context(), parent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{source.Metadata().ID, resumedMeta.ID}, childIDs(children))
}
