package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func searchTestRecord() MetadataRecord {
	return MetadataRecord{
		ID:        "session-1",
		Workspace: WorkspaceRef{ID: "ws-1", Path: "/tmp/ws"},
		Title:     TitleRecord{Text: "Fix Flaky Test"},
		Summary:   "the CI suite retries",
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// TestSearchBuildSearchDocNormalizes pins the prompt normalization: trimming,
// dropping empties, deduplication, truncation on a UTF-8 boundary, the prompt
// count cap, and the combined content budget.
func TestSearchBuildSearchDocNormalizes(t *testing.T) {
	t.Parallel()

	record := searchTestRecord()
	doc := BuildSearchDoc(record, []string{"  hello  ", "", "   ", "hello", "world"})

	require.Equal(t, SearchSchema, doc.Schema)
	require.Equal(t, record.ID, doc.ID)
	require.Equal(t, record.Workspace.ID, doc.WorkspaceID)
	require.Equal(t, record.Title.Text, doc.Title)
	require.Equal(t, record.Summary, doc.Summary)
	require.Equal(t, record.UpdatedAt, doc.UpdatedAt)
	require.Equal(t, []string{"hello", "world"}, doc.Prompts)

	empty := BuildSearchDoc(record, nil)
	require.NotNil(t, empty.Prompts)
	require.Empty(t, empty.Prompts)
}

// TestSearchBuildSearchDocTruncatesPrompt checks that a long prompt is cut to
// MaxSearchPromptBytes without splitting a multi-byte rune.
func TestSearchBuildSearchDocTruncatesPrompt(t *testing.T) {
	t.Parallel()

	long := "x" + strings.Repeat("é", 300)
	doc := BuildSearchDoc(searchTestRecord(), []string{long})

	require.Len(t, doc.Prompts, 1)
	require.LessOrEqual(t, len(doc.Prompts[0]), MaxSearchPromptBytes)
	require.True(t, utf8.ValidString(doc.Prompts[0]))
	require.Len(t, doc.Prompts[0], 511)
}

// TestSearchBuildSearchDocCapsPromptCount checks the per-document prompt cap.
func TestSearchBuildSearchDocCapsPromptCount(t *testing.T) {
	t.Parallel()

	prompts := make([]string, 0, MaxSearchPrompts+8)
	for i := range MaxSearchPrompts + 8 {
		prompts = append(prompts, "prompt-"+strings.Repeat("a", i+1))
	}

	doc := BuildSearchDoc(searchTestRecord(), prompts)

	require.Len(t, doc.Prompts, MaxSearchPrompts)
}

// TestSearchBuildSearchDocRespectsContentBudget checks that prompt collection
// stops before the combined content budget is exceeded.
func TestSearchBuildSearchDocRespectsContentBudget(t *testing.T) {
	t.Parallel()

	prompts := make([]string, 0, MaxSearchPrompts)
	for i := range MaxSearchPrompts {
		prompts = append(prompts, string(rune('a'+i%26))+strings.Repeat("b", MaxSearchPromptBytes-1))
	}

	doc := BuildSearchDoc(searchTestRecord(), prompts)

	total := 0
	for _, prompt := range doc.Prompts {
		total += len(prompt)
	}

	require.LessOrEqual(t, total, MaxSearchContentBytes)
	require.Len(t, doc.Prompts, MaxSearchContentBytes/MaxSearchPromptBytes)
}

// TestSearchDocHash pins that the hash follows the content: stable for equal
// inputs and different when any part changes.
func TestSearchDocHash(t *testing.T) {
	t.Parallel()

	record := searchTestRecord()
	first := BuildSearchDoc(record, []string{"alpha"})
	second := BuildSearchDoc(record, []string{"alpha"})
	require.Equal(t, first.Hash, second.Hash)
	require.Len(t, first.Hash, 64)
	require.Equal(t, strings.ToLower(first.Hash), first.Hash)

	changed := record
	changed.Summary = "a different summary"
	require.NotEqual(t, first.Hash, BuildSearchDoc(changed, []string{"alpha"}).Hash)
	require.NotEqual(t, first.Hash, BuildSearchDoc(record, []string{"beta"}).Hash)

	require.Equal(t, "Fix Flaky Test\nthe CI suite retries\nalpha", first.Content())
}

// TestSearchDocRoundTrip checks that a written document reads back unchanged
// with private file permissions.
func TestSearchDocRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SearchFileName)
	doc := BuildSearchDoc(searchTestRecord(), []string{"alpha", "beta"})

	require.NoError(t, WriteSearchDoc(path, doc))

	got, err := ReadSearchDoc(path)
	require.NoError(t, err)
	require.Equal(t, doc.Schema, got.Schema)
	require.Equal(t, doc.ID, got.ID)
	require.Equal(t, doc.WorkspaceID, got.WorkspaceID)
	require.Equal(t, doc.Title, got.Title)
	require.Equal(t, doc.Summary, got.Summary)
	require.Equal(t, doc.Prompts, got.Prompts)
	require.Equal(t, doc.Hash, got.Hash)
	require.True(t, doc.UpdatedAt.Equal(got.UpdatedAt))

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

// TestSearchReadDocMissingFile checks that a missing document is reported as
// os.ErrNotExist rather than swallowed.
func TestSearchReadDocMissingFile(t *testing.T) {
	t.Parallel()

	_, err := ReadSearchDoc(filepath.Join(t.TempDir(), SearchFileName))
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestSearchReadDocRejectsInvalid pins strict decoding and validation of a
// stored document.
func TestSearchReadDocRejectsInvalid(t *testing.T) {
	t.Parallel()

	valid := `{"schema":"` + SearchSchema + `","id":"session-1","updated_at":"2026-01-02T03:04:05Z","hash":"abc"}`
	cases := map[string]struct {
		body     string
		wantSent bool
	}{
		"wrong schema": {body: `{"schema":"other","id":"session-1","updated_at":"2026-01-02T03:04:05Z","hash":"abc"}`, wantSent: true},
		"missing id":   {body: `{"schema":"` + SearchSchema + `","id":"","updated_at":"2026-01-02T03:04:05Z","hash":"abc"}`, wantSent: true},
		"unknown field": {body: `{"schema":"` + SearchSchema + `","id":"session-1","extra":1,` +
			`"updated_at":"2026-01-02T03:04:05Z","hash":"abc"}`},
		"oversized": {body: valid + strings.Repeat(" ", maxSearchDocBytes)},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), SearchFileName)
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))

			_, err := ReadSearchDoc(path)
			require.Error(t, err)

			if tc.wantSent {
				require.ErrorIs(t, err, ErrSearch)
			}
		})
	}
}

// TestSearchDocMatch pins the case-insensitive, all-tokens-present matching.
func TestSearchDocMatch(t *testing.T) {
	t.Parallel()

	doc := BuildSearchDoc(searchTestRecord(), []string{"add retry helper"})

	require.True(t, doc.Match("FLAKY"))
	require.True(t, doc.Match("flaky retry"))
	require.True(t, doc.Match("RETRY HELPER"))
	require.False(t, doc.Match("flaky missing"))
	require.False(t, doc.Match(""))
	require.False(t, doc.Match("   "))
	require.False(t, doc.Match("zzz"))
	require.False(t, SearchDoc{}.Match("flaky"))
}
