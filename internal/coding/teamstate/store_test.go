//nolint:wsl_v5 // Durable journal fixtures keep actions and assertions adjacent.
package teamstate_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreCommitCASReplayAndClone(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	first := snapshot("team-one", "s-parent", 1)
	created, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)
	assert.Equal(t, team.Revision(1), created.Revision)

	replayed, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)
	assert.Equal(t, created, replayed)

	different := first
	different.State = teamstate.StateActive
	_, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: different,
	})
	require.ErrorIs(t, err, teamstate.ErrIdempotency)
	_, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "stale", ExpectedRevision: 0, Snapshot: different,
	})
	require.ErrorIs(t, err, teamstate.ErrConflict)

	second := first
	second.Revision = 2
	second.State = teamstate.StateActive
	second.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	updated, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "activate", ExpectedRevision: 1, Snapshot: second,
	})
	require.NoError(t, err)
	assert.Equal(t, team.Revision(2), updated.Revision)

	replayed, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)
	assert.Equal(t, team.Revision(1), replayed.Revision)

	loaded, err := store.Load(t.Context(), "team-one")
	require.NoError(t, err)
	loaded.Members[0].CapabilityProfileFingerprint = strings.Repeat("f", 64)
	again, err := store.Load(t.Context(), "team-one")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 64), again.Members[0].CapabilityProfileFingerprint)

	info, err := os.Stat(filepath.Join(store.Dir(), "team-one.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(store.Dir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	for _, state := range []teamstate.State{
		teamstate.StateIntegrated,
		teamstate.StateClosedWithoutIntegration,
		teamstate.StateCancelled,
		teamstate.StateFailed,
	} {
		assert.True(t, teamstate.IsTerminal(state))
	}
	for _, state := range []teamstate.State{
		teamstate.StateAdmitted,
		teamstate.StateActive,
		teamstate.StateInterrupted,
		teamstate.StateIntegrationPending,
	} {
		assert.False(t, teamstate.IsTerminal(state))
	}
}

func TestStoreConcurrentExpectedRevisionHasOneWinner(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	first := snapshot("team-race", "s-parent", 1)
	_, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)

	var wait sync.WaitGroup
	errorsByWriter := make(chan error, 2)
	for _, command := range []team.CommandID{"writer-one", "writer-two"} {
		wait.Go(func() {
			next := first
			next.Revision = 2
			next.State = teamstate.StateActive
			next.UpdatedAt = next.UpdatedAt.Add(time.Minute)
			_, commitErr := store.Commit(t.Context(), teamstate.Mutation{
				CommandID: command, ExpectedRevision: 1, Snapshot: next,
			})
			errorsByWriter <- commitErr
		})
	}
	wait.Wait()
	close(errorsByWriter)

	var succeeded, conflicted int
	for commitErr := range errorsByWriter {
		switch {
		case commitErr == nil:
			succeeded++
		case errors.Is(commitErr, teamstate.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected commit error: %v", commitErr)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, conflicted)
}

func TestStoreReadOnlyProjectionIgnoresButDoesNotRepairTornTail(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	first := snapshot("team-torn", "s-parent", 1)
	_, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)
	path := filepath.Join(store.Dir(), "team-torn.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test path.
	require.NoError(t, err)
	_, err = file.WriteString(`{"schema":`)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	before, err := os.ReadFile(path) //nolint:gosec // test path.
	require.NoError(t, err)

	loaded, err := store.Load(t.Context(), "team-torn")
	require.NoError(t, err)
	assert.Equal(t, team.Revision(1), loaded.Revision)
	listed, err := store.ListByParent(t.Context(), "s-parent", 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	afterRead, err := os.ReadFile(path) //nolint:gosec // test path.
	require.NoError(t, err)
	assert.Equal(t, before, afterRead)

	second := first
	second.Revision = 2
	second.State = teamstate.StateActive
	second.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	_, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "activate", ExpectedRevision: 1, Snapshot: second,
	})
	require.NoError(t, err)
	repaired, err := os.ReadFile(path) //nolint:gosec // test path.
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(repaired), "\n"))
	assert.True(t, strings.HasSuffix(string(repaired), "\n"))
}

func TestStoreRejectsCompleteRecordWithoutNewlineAndUnknownSchema(t *testing.T) {
	t.Parallel()

	t.Run("complete without newline", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		first := snapshot("team-no-newline", "s-parent", 1)
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0, Snapshot: first,
		})
		require.NoError(t, err)
		path := filepath.Join(store.Dir(), "team-no-newline.jsonl")
		data, err := os.ReadFile(path) //nolint:gosec // test path.
		require.NoError(t, err)
		require.NoError(t, os.WriteFile( //nolint:gosec // Test-controlled private path.
			path,
			data[:len(data)-1],
			0o600,
		))

		_, err = store.Load(t.Context(), "team-no-newline")
		require.ErrorIs(t, err, teamstate.ErrCorrupt)
	})

	t.Run("unknown schema", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		first := snapshot("team-schema", "s-parent", 1)
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0, Snapshot: first,
		})
		require.NoError(t, err)
		path := filepath.Join(store.Dir(), "team-schema.jsonl")
		data, err := os.ReadFile(path) //nolint:gosec // test path.
		require.NoError(t, err)
		data = []byte(strings.Replace(string(data),
			"pips.coding.team-resource/v1alpha1", "pips.coding.team-resource/v9", 1))
		require.NoError(t, os.WriteFile(path, data, 0o600)) //nolint:gosec // Test path.

		_, err = store.Load(t.Context(), "team-schema")
		require.ErrorIs(t, err, teamstate.ErrCorrupt)
	})

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		first := snapshot("team-field", "s-parent", 1)
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0, Snapshot: first,
		})
		require.NoError(t, err)
		path := filepath.Join(store.Dir(), "team-field.jsonl")
		data, err := os.ReadFile(path) //nolint:gosec // test path.
		require.NoError(t, err)
		data = []byte(strings.Replace(string(data), `{"schema":`, `{"unknown":true,"schema":`, 1))
		require.NoError(t, os.WriteFile(path, data, 0o600)) //nolint:gosec // Test path.

		_, err = store.Load(t.Context(), "team-field")
		require.ErrorIs(t, err, teamstate.ErrCorrupt)
	})

	t.Run("non torn garbage", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		first := snapshot("team-garbage", "s-parent", 1)
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0, Snapshot: first,
		})
		require.NoError(t, err)
		path := filepath.Join(store.Dir(), "team-garbage.jsonl")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test path.
		require.NoError(t, err)
		_, err = file.WriteString("not-json")
		require.NoError(t, err)
		require.NoError(t, file.Close())

		_, err = store.Load(t.Context(), "team-garbage")
		require.ErrorIs(t, err, teamstate.ErrCorrupt)
	})
}

func TestStoreRejectsUnsafeRepositoryAndJournal(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	insecure := filepath.Join(base, "insecure")
	require.NoError(t, os.Mkdir(insecure, 0o755)) //nolint:gosec // Insecure fixture.
	require.NoError(t, os.Chmod(insecure, 0o755)) //nolint:gosec // Insecure fixture.
	_, err := teamstate.New(insecure, teamstate.Limits{})
	require.ErrorIs(t, err, teamstate.ErrUnsafeFile)

	store := newStore(t)
	foreign := filepath.Join(base, "foreign")
	require.NoError(t, os.WriteFile(foreign, []byte("private"), 0o600))
	require.NoError(t, os.Symlink(foreign, filepath.Join(store.Dir(), "team-link.jsonl")))
	_, err = store.Load(t.Context(), "team-link")
	require.ErrorIs(t, err, teamstate.ErrUnsafeFile)
	content, err := os.ReadFile(foreign) //nolint:gosec // test path.
	require.NoError(t, err)
	assert.Equal(t, "private", string(content))

	unsafeFile := filepath.Join(store.Dir(), "team-directory.jsonl")
	require.NoError(t, os.Mkdir(unsafeFile, 0o700))
	_, err = store.Load(t.Context(), "team-directory")
	require.ErrorIs(t, err, teamstate.ErrUnsafeFile)
}

func TestStoreRejectsRepositoryReplacementAndInsecureFileMode(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	directory := filepath.Join(parent, "resources")
	store, err := teamstate.New(directory, teamstate.Limits{})
	require.NoError(t, err)
	first := snapshot("team-replaced", "s-parent", 1)
	_, err = store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)
	path := filepath.Join(directory, "team-replaced.jsonl")
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // deliberately insecure fixture.
	_, err = store.Load(t.Context(), "team-replaced")
	require.ErrorIs(t, err, teamstate.ErrUnsafeFile)
	require.NoError(t, os.Chmod(path, 0o600))

	original := filepath.Join(parent, "resources-original")
	require.NoError(t, os.Rename(directory, original))
	require.NoError(t, os.Mkdir(directory, 0o700))
	_, err = store.Load(t.Context(), "team-replaced")
	require.ErrorIs(t, err, teamstate.ErrUnsafeFile)
}

func TestStoreRejectsOversizeAndImmutableAttemptLineageChanges(t *testing.T) {
	t.Parallel()

	t.Run("oversize", func(t *testing.T) {
		t.Parallel()

		limits := teamstate.DefaultLimits()
		limits.MaxRecordBytes = 128
		limits.MaxFileBytes = 1 << 10
		store, err := teamstate.New(filepath.Join(t.TempDir(), "resources"), limits)
		require.NoError(t, err)
		_, err = store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0,
			Snapshot: snapshot("team-large", "s-parent", 1),
		})
		require.ErrorIs(t, err, teamstate.ErrLimit)
	})

	t.Run("immutable attempt", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		first := snapshot("team-attempt", "s-parent", 1)
		first.Attempts = []teamstate.AttemptResource{{
			TaskID: "task-one", AttemptID: "attempt-one", MemberID: "lead",
			ContinuationID: "continuation-one", State: teamstate.AttemptPlanned,
			Cleanup: teamstate.CleanupRetain,
		}}
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "create", ExpectedRevision: 0, Snapshot: first,
		})
		require.NoError(t, err)

		changed := first
		changed.Revision = 2
		changed.UpdatedAt = changed.UpdatedAt.Add(time.Minute)
		changed.Attempts = append([]teamstate.AttemptResource(nil), first.Attempts...)
		changed.Attempts[0].ContinuationID = "continuation-other"
		_, err = store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "change-lineage", ExpectedRevision: 1, Snapshot: changed,
		})
		require.ErrorIs(t, err, teamstate.ErrInvalid)

		removed := first
		removed.Revision = 2
		removed.UpdatedAt = removed.UpdatedAt.Add(time.Minute)
		removed.Attempts = nil
		_, err = store.Commit(t.Context(), teamstate.Mutation{
			CommandID: "remove-attempt", ExpectedRevision: 1, Snapshot: removed,
		})
		require.ErrorIs(t, err, teamstate.ErrInvalid)
	})
}

func TestStoreListByParentIsBoundedAndStable(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	for index, id := range []team.ID{"team-a", "team-b", "team-c"} {
		parent := "s-parent"
		if id == "team-c" {
			parent = "s-other"
		}
		value := snapshot(id, parent, 1)
		if id == "team-c" {
			value.Parent.WorkspaceID = "workspace-other"
		}
		value.UpdatedAt = value.UpdatedAt.Add(time.Duration(index) * time.Minute)
		_, err := store.Commit(t.Context(), teamstate.Mutation{
			CommandID:        team.CommandID("create-" + string(id)),
			ExpectedRevision: 0,
			Snapshot:         value,
		})
		require.NoError(t, err)
	}

	values, err := store.ListByParent(t.Context(), "s-parent", 1)
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, team.ID("team-b"), values[0].TeamID)
	_, err = store.ListByParent(t.Context(), "s-parent", 0)
	require.ErrorIs(t, err, teamstate.ErrInvalid)

	index, err := store.ListByWorkspace(t.Context(), "workspace-parent", 2)
	require.NoError(t, err)
	require.Len(t, index, 2)
	assert.Equal(t, team.ID("team-b"), index[0].TeamID)
	assert.Equal(t, "s-parent", index[0].ParentSessionID)
	assert.Equal(t, team.ID("team-a"), index[1].TeamID)
	encoded, err := json.Marshal(index)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "/workspace")
	assert.NotContains(t, string(encoded), "/repository")

	_, err = store.ListByWorkspace(t.Context(), "workspace-parent", 0)
	require.ErrorIs(t, err, teamstate.ErrInvalid)
}

func newStore(t *testing.T) *teamstate.Store {
	t.Helper()
	store, err := teamstate.New(filepath.Join(t.TempDir(), "resources"), teamstate.Limits{})
	require.NoError(t, err)

	return store
}

func snapshot(id team.ID, parentSessionID string, revision team.Revision) teamstate.Snapshot {
	at := time.Date(2026, time.July, 27, 1, 2, 3, 0, time.UTC)
	return teamstate.Snapshot{
		TeamID:   id,
		Revision: revision,
		State:    teamstate.StateAdmitted,
		Parent: teamstate.ParentResource{
			SessionID:   parentSessionID,
			WorkspaceID: "workspace-parent",
			Workspace:   teamstate.FileIdentity{Path: "/workspace", Device: 1, Inode: 2},
		},
		Repository: teamstate.RepositoryResource{
			CommonDir: teamstate.FileIdentity{Path: "/repository/.git", Device: 1, Inode: 3},
			BaseOID:   strings.Repeat("a", 40),
			BranchRef: "refs/heads/main",
			Admission: teamstate.AdmissionClean,
		},
		Members: []teamstate.MemberResource{{
			MemberID: "lead", CapabilityProfileFingerprint: strings.Repeat("b", 64),
		}},
		Cleanup:   teamstate.CleanupRetain,
		CreatedAt: at,
		UpdatedAt: at,
	}
}
