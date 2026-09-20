package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entryGoldenJSON pins the on-disk envelope of one message entry: pipling
// stores these bytes in database rows, and existing JSONL sessions carry
// them, so changing the envelope is a migration rather than a test update.
const entryGoldenJSON = `{"kind":"message","id":"e-golden","parent_id":"root","time":"2026-09-20T10:30:00Z","message":{"role":"user","parts":[{"type":"text","text":"hello"}]}}`

// entryFixtures returns one entry per kind, so the codec is exercised over
// every payload field instead of the message case alone.
func entryFixtures(at time.Time) []struct {
	name  string
	entry harness.Entry
} {
	return []struct {
		name  string
		entry harness.Entry
	}{
		{
			name: "message",
			entry: harness.Entry{
				Kind: harness.KindMessage, ID: "e-message", ParentID: "root", Time: at,
				Message: ai.UserText("hello"),
				Usage:   &ai.Usage{InputTokens: 3, OutputTokens: 4, ReasoningTokens: 1},
			},
		},
		{
			name: "model change",
			entry: harness.Entry{
				Kind: harness.KindModelChange, ID: "e-model", Time: at,
				Provider: ai.Provider("openai"), ModelID: "gpt-x",
			},
		},
		{
			name: "compaction",
			entry: harness.Entry{
				Kind: harness.KindCompaction, ID: "e-compact", Time: at,
				Summary: "condensed history", FirstKeptID: "e-message", TokensBefore: 128,
			},
		},
		{
			name: "branch summary",
			entry: harness.Entry{
				Kind: harness.KindBranchSummary, ID: "e-branch", Time: at,
				Summary: "abandoned branch", FromID: "e-message",
			},
		},
		{
			name: "custom",
			entry: harness.Entry{
				Kind: harness.KindCustom, ID: "e-custom", Time: at,
				Custom: "pipling.tool_call", Data: ai.JSON(`{"name":"read_file"}`),
			},
		},
		{
			name: "label",
			entry: harness.Entry{
				Kind: harness.KindLabel, ID: "e-label", Time: at,
				TargetID: "e-message", Label: "checkpoint",
			},
		},
		{
			name: "name",
			entry: harness.Entry{
				Kind: harness.KindName, ID: "e-name", Time: at, Name: "session name",
			},
		},
		{
			name: "leaf",
			entry: harness.Entry{
				Kind: harness.KindLeaf, ID: "e-leaf", Time: at, LeafID: "e-message",
			},
		},
	}
}

func TestMarshalEntryRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 20, 10, 30, 0, 0, time.UTC)

	for _, tc := range entryFixtures(at) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := harness.MarshalEntry(tc.entry)
			require.NoError(t, err)

			got, err := harness.UnmarshalEntry(data)
			require.NoError(t, err)
			assert.Equal(t, tc.entry, got)
		})
	}
}

func TestMarshalEntryEnvelopeIsStable(t *testing.T) {
	t.Parallel()

	entry := harness.Entry{
		Kind: harness.KindMessage, ID: "e-golden", ParentID: "root",
		Time:    time.Date(2026, 9, 20, 10, 30, 0, 0, time.UTC),
		Message: ai.UserText("hello"),
	}

	data, err := harness.MarshalEntry(entry)
	require.NoError(t, err)
	// Byte-exact on purpose: JSONEq would ignore key order and spacing, and the
	// exact bytes are what a stored row carries.
	assert.Equal(t, entryGoldenJSON, string(data)) //nolint:testifylint // byte-exact pin, not a JSON-equality check
}

func TestMarshalEntryMatchesJSONLStoreBytes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := harness.CreateJSONL(path, "session-1", nil)
	require.NoError(t, err)

	entry := harness.Entry{
		Kind: harness.KindMessage, ID: "e-jsonl",
		Time:    time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC),
		Message: ai.UserText("from the store"),
	}
	require.NoError(t, store.Append(entry))
	require.NoError(t, store.Close())

	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from t.TempDir(), not from user input
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	require.Len(t, lines, 2, "a session file holds a header line followed by one line per entry")

	data, err := harness.MarshalEntry(entry)
	require.NoError(t, err)
	assert.Equal(t, lines[1], string(data), "the exported codec and the JSONL store must agree byte for byte")
}

func TestUnmarshalEntryRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	data, err := harness.MarshalEntry(harness.Entry{Kind: harness.KindName, ID: "e1", Name: "n"})
	require.NoError(t, err)

	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "unknown field",
			input: strings.Replace(string(data), `"kind"`, `"unknown_field":"x","kind"`, 1),
		},
		{name: "trailing data", input: string(data) + `{"kind":"name"}`},
		{name: "invalid json", input: `{"kind":`},
		{name: "empty input", input: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := harness.UnmarshalEntry([]byte(tc.input))
			assert.Error(t, err)
		})
	}
}
