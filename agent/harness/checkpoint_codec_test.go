//nolint:wsl_v5 // Codec and publication tests preserve action/assertion groups.
package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextCheckpointJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := CreateJSONL(path, "checkpoint", nil)
	require.NoError(t, err)
	session, err := NewSession(store)
	require.NoError(t, err)
	parent := appendCheckpointMessage(t, session, ai.UserText("original raw history"), nil)
	seed := testCheckpoint()
	id, err := session.AppendContextCheckpoint(parent, seed, 10000)
	require.NoError(t, err)
	entry, ok := session.Entry(id)
	require.True(t, ok)
	encoded, err := MarshalEntry(entry)
	require.NoError(t, err)
	decoded, err := UnmarshalEntry(encoded)
	require.NoError(t, err)
	assert.Equal(t, entry, decoded)
	assert.Contains(t, string(encoded), `"seed_messages"`)
	assert.NotContains(t, string(encoded), `"first_kept_id"`)
	require.NoError(t, store.Close())

	raw, err := os.ReadFile(path) //nolint:gosec // Private temporary test session.
	require.NoError(t, err)
	assert.Contains(t, string(raw), string(encoded)+"\n")
	assert.Contains(t, string(raw), `"version":1`, "the JSONL transport header stays at version 1")
	reopenedStore, err := OpenJSONL(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopenedStore.Close()) })
	reopened, err := NewSession(reopenedStore)
	require.NoError(t, err)
	visible, err := reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, seed.Messages, visible.Messages)
	assert.Equal(t, EstimateContextUsage(session.Path()), EstimateContextUsage(reopened.Path()))
	prefix, err := ReadJSONLPrefix(path, JSONLPrefixLimits{})
	require.NoError(t, err)
	require.Len(t, prefix.Entries, 2)
	assert.Equal(t, entry, prefix.Entries[1])
}

func TestContextCheckpointStrictCodec(t *testing.T) {
	t.Parallel()
	seed := testCheckpoint()
	entry := Entry{Kind: KindContextCheckpoint, ID: "checkpoint", Time: time.Now().UTC(), Checkpoint: &seed}
	encoded, err := MarshalEntry(entry)
	require.NoError(t, err)
	wire := string(encoded)
	_, err = UnmarshalEntry([]byte(strings.Replace(wire, `"version":1`, `"version":1,"fixed_tokens":0`, 1)))
	require.NoError(t, err, "optional fixed overhead accepts an explicit zero")
	tests := []struct{ name, wire string }{
		{"unknown version", strings.Replace(wire, `"version":1`, `"version":2`, 1)},
		{"unknown checkpoint field", strings.Replace(wire, `"version":1`, `"version":1,"unexpected":true`, 1)},
		{"unknown message field", strings.Replace(wire, `"role":"user"`, `"role":"user","unexpected":true`, 1)},
		{"unknown part field", strings.Replace(wire, `"type":"text"`, `"type":"text","unexpected":true`, 1)},
		{"invalid role", strings.Replace(wire, `"role":"user"`, `"role":"future"`, 1)},
		{"role authority", strings.Replace(wire, `"role":"user"`, `"role":"system"`, 1)},
		{"duplicate version", strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1)},
		{"duplicate role", strings.Replace(wire, `"role":"user"`, `"role":"user","role":"user"`, 1)},
		{"duplicate boundary", strings.Replace(wire, `"id":"checkpoint"`, `"id":"checkpoint","parent_id":"old","parent_id":""`, 1)},
		{"legacy field", strings.Replace(wire, `"kind":"context_checkpoint"`, `"kind":"context_checkpoint","first_kept_id":"old"`, 1)},
		{"mismatched kind", strings.Replace(wire, `"kind":"context_checkpoint"`, `"kind":"name"`, 1)},
		{"missing payload", `{"kind":"context_checkpoint","id":"checkpoint","time":"2026-10-01T00:00:00Z"}`},
		{"negative count", strings.Replace(wire, `"kind":"context_checkpoint"`, `"kind":"context_checkpoint","tokens_before":-1`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalEntry([]byte(test.wire))
			require.Error(t, err)
		})
	}

	entry.FirstKeptID = "legacy-boundary"
	_, err = MarshalEntry(entry)
	require.ErrorIs(t, err, ErrInvalidEntry)
	entry.FirstKeptID = ""
	entry.Kind = KindMessage
	_, err = MarshalEntry(entry)
	require.ErrorIs(t, err, ErrInvalidEntry)
}

func TestContextCheckpointRejectsOldReaderShape(t *testing.T) {
	t.Parallel()
	seed := testCheckpoint()
	encoded, err := MarshalEntry(Entry{Kind: KindContextCheckpoint, ID: "checkpoint", Checkpoint: &seed})
	require.NoError(t, err)
	// This models the old closed envelope without weakening the production
	// decoder. Old-reader compatibility is deliberately not a promise.
	var oldReader struct {
		Kind     Kind      `json:"kind"`
		ID       string    `json:"id"`
		ParentID string    `json:"parent_id,omitempty"`
		Time     time.Time `json:"time"`
		Summary  string    `json:"summary,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	require.Error(t, decoder.Decode(&oldReader))
}

func TestContextCheckpointJSONLSizeFailureDoesNotAdvanceLeaf(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := CreateJSONL(path, "full", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	session, err := NewSession(store)
	require.NoError(t, err)
	leaf := appendCheckpointMessage(t, session, ai.UserText("original"), nil)
	before := session.Entries()
	require.NoError(t, store.file.Truncate(maxSessionFileSize-1))
	_, err = session.AppendContextCheckpoint(leaf, testCheckpoint(), 100)
	require.ErrorContains(t, err, "session exceeds")
	assert.Equal(t, leaf, session.LeafID())
	assert.Equal(t, before, session.Entries())
	info, err := store.file.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(maxSessionFileSize-1), info.Size(), "oversize rejection happens before writing")
}

func TestContextCheckpointForkRequiresArchivePublication(t *testing.T) {
	t.Parallel()
	repo := Repo{Dir: t.TempDir()}
	store, err := repo.Create("source", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source, err := NewSession(store)
	require.NoError(t, err)
	old := appendCheckpointMessage(t, source, ai.UserText("raw"), nil)
	seed := testCheckpoint()
	_, err = source.AppendContextCheckpoint(old, seed, 100)
	require.NoError(t, err)
	seed.ArchiveID = "second-archive"
	_, err = source.AppendContextCheckpoint(source.LeafID(), seed, 10)
	require.NoError(t, err)
	_, err = source.AppendContextCheckpoint(source.LeafID(), seed, 10)
	require.NoError(t, err)
	before := source.Entries()

	_, err = repo.ForkSession(source, "", "missing", nil)
	require.ErrorIs(t, err, ErrForkArchivesRequired)
	_, err = os.Stat(filepath.Join(repo.Dir, "missing.jsonl"))
	require.ErrorIs(t, err, os.ErrNotExist)
	publicationErr := errors.New("archive publication failed")
	_, err = repo.ForkSession(source, "", "failed", nil, ForkOptions{PrepareArchives: func(ids []string) error {
		assert.Equal(t, []string{testCheckpoint().ArchiveID, seed.ArchiveID}, ids)
		_, statErr := os.Stat(filepath.Join(repo.Dir, "failed.jsonl"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
		return publicationErr
	}})
	require.ErrorIs(t, err, publicationErr)
	_, err = os.Stat(filepath.Join(repo.Dir, "failed.jsonl"))
	require.ErrorIs(t, err, os.ErrNotExist)
	staged, err := filepath.Glob(filepath.Join(repo.Dir, ".fork-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, staged)

	calls := 0
	forked, err := repo.ForkSession(source, "", "fork", nil, ForkOptions{PrepareArchives: func(ids []string) error {
		calls++
		assert.Equal(t, []string{testCheckpoint().ArchiveID, seed.ArchiveID}, ids)
		_, statErr := os.Stat(filepath.Join(repo.Dir, "fork.jsonl"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
		return nil
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, forked.Close()) })
	assert.Equal(t, 1, calls)
	forkSession, err := NewSession(forked)
	require.NoError(t, err)
	visible, err := forkSession.Context()
	require.NoError(t, err)
	assert.Equal(t, seed.Messages, visible.Messages)
	assert.Equal(t, before, source.Entries())
	assert.Equal(t, before, forkSession.Entries())

	// An ancestor-only fork has no archive dependency, even though later
	// checkpoints remain stored in the source graph.
	ancestor, err := repo.ForkSession(source, old, "ancestor", nil)
	require.NoError(t, err)
	require.NoError(t, ancestor.Close())
}
