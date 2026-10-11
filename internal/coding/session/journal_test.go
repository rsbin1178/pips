package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func journalPath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "s-journal", JournalFileName)
}

func TestJournalAppendAndRead(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	journal, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)

	when := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	require.NoError(t, journal.Append(
		JournalRecord{Time: when, Type: "run.started", Payload: json.RawMessage(`{"a":1}`)},
		JournalRecord{Type: "turn.completed", Payload: json.RawMessage(`{"turn":1}`)},
	))
	require.Equal(t, uint64(2), journal.Sequence())
	require.NoError(t, journal.Close())
	require.NoError(t, journal.Close(), "close is idempotent")

	records, err := ReadJournal(path, 0)
	require.NoError(t, err)
	require.Len(t, records, 2)

	assert.Equal(t, JournalSchema, records[0].Schema)
	assert.Equal(t, "s-journal", records[0].SessionID)
	assert.Equal(t, uint64(1), records[0].Seq)
	assert.Equal(t, "run.started", records[0].Type)
	assert.Equal(t, when, records[0].Time)
	assert.JSONEq(t, `{"a":1}`, string(records[0].Payload))

	// A record without an explicit time gets one, and the sequence stays dense.
	assert.Equal(t, uint64(2), records[1].Seq)
	assert.False(t, records[1].Time.IsZero())

	// The file is private.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestJournalReopenContinuesTheSequence(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	first, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)
	require.NoError(t, first.Append(JournalRecord{Type: "a"}))
	require.NoError(t, first.Close())

	second, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), second.Sequence())
	require.NoError(t, second.Append(JournalRecord{Type: "b"}))
	require.NoError(t, second.Close())

	records, err := ReadJournal(path, 0)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, []uint64{1, 2}, []uint64{records[0].Seq, records[1].Seq})
}

func TestJournalRejectsAnotherSessionsRecord(t *testing.T) {
	t.Parallel()

	journal, err := OpenJournal(journalPath(t), "s-journal")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })

	err = journal.Append(JournalRecord{SessionID: "s-other", Type: "a"})
	require.ErrorIs(t, err, ErrJournalCorrupt)

	// The rejected record consumed no sequence.
	assert.Equal(t, uint64(0), journal.Sequence())
}

func TestJournalToleratesATornTail(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	journal, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)
	require.NoError(t, journal.Append(JournalRecord{Type: "committed"}))
	require.NoError(t, journal.Close())

	// Simulate an append interrupted mid-line: no terminating newline, invalid JSON.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test-owned path
	require.NoError(t, err)
	_, err = file.WriteString(`{"schema":"` + JournalSchema + `","seq":2,"ty`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	records, err := ReadJournal(path, 0)
	require.NoError(t, err, "a torn tail is an unfinished append, not corruption")
	require.Len(t, records, 1)
	assert.Equal(t, "committed", records[0].Type)

	// Reopening repairs the file: the torn bytes are dropped so the next append
	// cannot produce a corrupt interior line.
	resumed, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), resumed.Sequence())
	require.NoError(t, resumed.Append(JournalRecord{Type: "after repair"}))
	require.NoError(t, resumed.Close())

	repaired, err := ReadJournal(path, 0)
	require.NoError(t, err)
	require.Len(t, repaired, 2)
	assert.Equal(t, []string{"committed", "after repair"}, []string{repaired[0].Type, repaired[1].Type})
	assert.Equal(t, []uint64{1, 2}, []uint64{repaired[0].Seq, repaired[1].Seq})

	// The torn bytes are gone from the file itself: exactly two lines, each
	// terminated, so nothing of the partial append survives.
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), "\n"))
	assert.Equal(t, byte('\n'), raw[len(raw)-1], "the journal ends on a complete record")
}

func TestJournalRejectsCorruption(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"interior":   "{\"schema\":\"" + JournalSchema + "\",\"seq\":1,\"session_id\":\"s-1\",\"type\":\"a\"}\nnot json\n",
		"schema":     "{\"schema\":\"other/v9\",\"seq\":1,\"session_id\":\"s-1\",\"type\":\"a\"}\n",
		"empty line": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := journalPath(t)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

			records, err := ReadJournal(path, 0)
			if name == "empty line" {
				require.NoError(t, err, "a blank line carries no record")
				assert.Empty(t, records)

				return
			}

			require.ErrorIs(t, err, ErrJournalCorrupt)
		})
	}
}

func TestJournalReadHonoursLimitAndMissingFile(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	journal, err := OpenJournal(path, "s-journal")
	require.NoError(t, err)

	for _, kind := range []string{"a", "b", "c"} {
		require.NoError(t, journal.Append(JournalRecord{Type: kind}))
	}

	require.NoError(t, journal.Close())

	records, err := ReadJournal(path, 2)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "a", records[0].Type)

	missing, err := ReadJournal(filepath.Join(t.TempDir(), "absent.jsonl"), 0)
	require.NoError(t, err)
	assert.Empty(t, missing)
}
