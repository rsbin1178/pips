package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metadataFixture(t *testing.T) (string, MetadataRecord) {
	t.Helper()

	dir := t.TempDir()
	record := NewMetadataRecord(
		"s-meta",
		WorkspaceRef{ID: "ws-id", Path: "/workspace/meta"},
		KindConversation,
		// Deliberately in the past so a later update is observably newer.
		time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
	)

	return filepath.Join(dir, MetadataFileName), record
}

func TestMetadataRecordRoundTrip(t *testing.T) {
	t.Parallel()

	path, record := metadataFixture(t)
	record.Title = TitleRecord{Text: "Layered store", Manual: true}
	record.Summary = "a summary"
	record.LastTurnSummary = "a turn summary"
	record.LastRecap = "a recap"
	record.Model = "openai/gpt-5"
	record.Git = &GitRef{Root: "/workspace/meta", Remotes: []string{"git@example.com:x/y.git"}, HeadBranch: "main"}
	record.Counts = Counts{Entries: 12, Messages: 9, Turns: 3, ToolCalls: 4}

	require.NoError(t, WriteMetadataRecord(path, record))

	read, err := ReadMetadataRecord(path)
	require.NoError(t, err)
	assert.Equal(t, record, read)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The atomic publish leaves no staging file behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, MetadataFileName, entries[0].Name())
}

func TestMetadataRecordRejectsUnusableInput(t *testing.T) {
	t.Parallel()

	valid := func() MetadataRecord {
		_, record := metadataFixture(t)

		return record
	}

	t.Run("unsupported schema", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Schema = "pips.coding.session/v1"
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("unsupported version", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Version = 1
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("empty workspace identity", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Workspace.ID = " "
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("unknown kind", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Kind = "other"
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("missing creation time", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.CreatedAt = time.Time{}
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("unsupported journal version", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Format.JournalVersion = JournalVersion + 1
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})

	t.Run("oversized text", func(t *testing.T) {
		t.Parallel()

		record := valid()
		record.Summary = strings.Repeat("x", maxMetadataTextRunes+1)
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)
	})
}

// TestMetadataRecordKindLineage pins what each kind must carry. A subagent may
// omit the identity snapshot, which is what legacy callers created.
func TestMetadataRecordKindLineage(t *testing.T) {
	t.Parallel()

	_, base := metadataFixture(t)

	t.Run("conversation needs nothing extra", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, ValidateMetadataRecord(base))
	})

	t.Run("subagent needs a parent and an agent", func(t *testing.T) {
		t.Parallel()

		record := base
		record.Kind = KindSubagent
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata, "no parent")

		record.Parent = &ParentRef{SessionID: "s-parent"}
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata, "no agent")

		record.Agent = "explorer"
		require.NoError(t, ValidateMetadataRecord(record), "an identity snapshot is optional")

		record.Subagent = &SubagentRef{AgentID: "explorer", Kind: "explore"}
		require.NoError(t, ValidateMetadataRecord(record))
	})

	t.Run("team worker needs lineage", func(t *testing.T) {
		t.Parallel()

		record := base
		record.Kind = KindTeamWorker
		require.ErrorIs(t, ValidateMetadataRecord(record), ErrMetadata)

		record.TeamWorker = &TeamWorkerRef{ParentSessionID: "s-lead", TeamID: "t-1"}
		require.NoError(t, ValidateMetadataRecord(record))
	})
}

func TestReadMetadataRecordRejectsUnusableFiles(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		_, err := ReadMetadataRecord(filepath.Join(t.TempDir(), MetadataFileName))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()

		path, record := metadataFixture(t)
		require.NoError(t, WriteMetadataRecord(path, record))

		// A field this build does not know is refused rather than silently dropped.
		data, err := os.ReadFile(path) //nolint:gosec // test-owned path
		require.NoError(t, err)

		body := strings.TrimSuffix(string(data), "\n")
		tampered := []byte(body[:len(body)-1] + `,"future":1}` + "\n")
		require.NoError(t, os.WriteFile(path, tampered, 0o600)) //nolint:gosec // test-owned path

		_, err = ReadMetadataRecord(path)
		require.ErrorIs(t, err, ErrMetadata)
	})

	t.Run("oversized", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), MetadataFileName)
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxMetadataRecordBytes+1)), 0o600))

		_, err := ReadMetadataRecord(path)
		require.Error(t, err)
		assert.NotErrorIs(t, err, os.ErrNotExist)
	})
}

func TestUpdateMetadataRecord(t *testing.T) {
	t.Parallel()

	path, record := metadataFixture(t)
	require.NoError(t, WriteMetadataRecord(path, record))

	require.NoError(t, UpdateMetadataRecord(path, func(current *MetadataRecord) error {
		current.Model = "openai/gpt-5"
		current.Counts.Entries = 7

		return nil
	}))

	updated, err := ReadMetadataRecord(path)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5", updated.Model)
	assert.Equal(t, 7, updated.Counts.Entries)
	assert.True(t, updated.UpdatedAt.After(record.UpdatedAt), "an update refreshes the activity time")
	assert.Equal(t, record.CreatedAt, updated.CreatedAt, "an update never moves the creation time")

	// A failed mutation leaves the stored record untouched.
	before := updated
	mutationErr := errors.New("no")
	err = UpdateMetadataRecord(path, func(current *MetadataRecord) error {
		current.Model = "should-not-land"

		return mutationErr
	})
	require.ErrorIs(t, err, mutationErr)

	after, err := ReadMetadataRecord(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// TestMetadataBelongsToWorkspace pins the ownership rule: the canonical path is
// the key, and the identity stays as a fallback for records that predate the
// path. A volume renumbering changes the identity without the user changing
// anything, so path matching is what keeps a session attributable.
func TestMetadataBelongsToWorkspace(t *testing.T) {
	t.Parallel()

	_, record := metadataFixture(t)
	meta := projectRecord(record, "/sessions/group/s-meta")

	t.Run("the canonical path matches", func(t *testing.T) {
		t.Parallel()

		assert.True(t, meta.BelongsToWorkspace("/workspace/meta", ""))
		assert.True(t, meta.BelongsToWorkspace("/workspace/meta/", "other-id"), "the path wins over a stale identity")
		assert.False(t, meta.BelongsToWorkspace("/workspace/other", ""))
	})

	t.Run("the identity matches when the path is unknown", func(t *testing.T) {
		t.Parallel()

		assert.True(t, meta.BelongsToWorkspace("", "ws-id"))
		assert.True(t, meta.BelongsToWorkspace("/elsewhere", "ws-id"))
		assert.False(t, meta.BelongsToWorkspace("", "other-id"))
	})

	t.Run("neither key matches", func(t *testing.T) {
		t.Parallel()

		assert.False(t, meta.BelongsToWorkspace("", ""))
		assert.False(t, meta.BelongsToWorkspace("/workspace/other", "other-id"))
	})
}
