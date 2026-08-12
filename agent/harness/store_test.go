//nolint:wsl_v5 // Store lifecycle fixtures keep actions and assertions adjacent.
package harness_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSession populates a memory-backed session via fn and returns it.
func buildSession(t *testing.T) *harness.Session {
	t.Helper()

	sess, err := harness.NewSession(harness.NewMemoryStore("test"))
	require.NoError(t, err)

	return sess
}

func appendText(t *testing.T, sess *harness.Session, messageType ai.Message, text string, usage *ai.Usage) string {
	t.Helper()

	var message ai.Message
	switch messageType.(type) {
	case ai.UserMessage:
		message = ai.UserText(text)
	case ai.AssistantMessage:
		message = ai.AssistantText(text)
	default:
		t.Fatalf("unsupported message type %T", messageType)
	}

	id, err := sess.AppendMessage(message, usage)
	require.NoError(t, err)

	return id
}

func firstTextPart(t *testing.T, message ai.Message) ai.TextPart {
	t.Helper()

	parts, err := ai.MessageParts(message)
	require.NoError(t, err)
	require.NotEmpty(t, parts)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text
}

func requestSystemText(t *testing.T, request ai.Request) string {
	t.Helper()

	system, _, err := request.Messages.SplitSystem()
	require.NoError(t, err)

	return ai.JoinSystemText(system)
}

func TestJSONLRoundTripAllKinds(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/s.jsonl"

	store, err := harness.CreateJSONL(path, "sess-1", map[string]string{"app": "test"})
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)

	u := ai.Usage{InputTokens: 10, OutputTokens: 5}
	first := appendText(t, sess, ai.UserMessage{}, "hi", nil)
	appendText(t, sess, ai.AssistantMessage{}, "hello", &u)

	_, err = sess.AppendModelChange(ai.ProviderOpenAI, "gpt-4o")
	require.NoError(t, err)
	_, err = sess.AppendCompaction("summary", first, 1234)
	require.NoError(t, err)
	_, err = sess.AppendCustom("checkpoint", ai.JSON(`{"n":1}`))
	require.NoError(t, err)
	require.NoError(t, sess.SetLabel(first, "start"))
	require.NoError(t, sess.SetName("my session"))
	require.NoError(t, sess.MoveTo(first, "took another path"))
	require.NoError(t, store.Close())

	// Reopen and verify everything survived.
	reopened, err := harness.OpenJSONL(path)
	require.NoError(t, err)

	defer reopened.Close() //nolint:errcheck // test cleanup

	restored, err := harness.NewSession(reopened)
	require.NoError(t, err)

	assert.Equal(t, "sess-1", restored.Metadata().ID)
	assert.Equal(t, "test", restored.Metadata().Extra["app"])
	assert.Equal(t, sess.Entries(), restored.Entries())
	assert.Equal(t, sess.LeafID(), restored.LeafID())
	assert.Equal(t, "my session", restored.Name())
	assert.Equal(t, map[string]string{first: "start"}, restored.Labels())

	entries := restored.Entries()
	require.NotEmpty(t, entries)
	assert.Equal(t, harness.KindBranchSummary, entries[len(entries)-1].Kind)
	assert.Equal(t, ai.Usage{InputTokens: 10, OutputTokens: 5}, *entries[1].Usage)
}

func TestJSONLPendingRoundTrip(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/pending.jsonl"
	store, err := harness.CreateJSONL(path, "pending", nil)
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)
	_, err = sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "c1", Name: "read_file", Args: ai.JSON(`{"path":"main.go"}`)},
		ai.ToolCallPart{ID: "c2", Name: "search_text", Args: ai.JSON(`{"query":"main"}`)},
	), nil)
	require.NoError(t, err)
	_, err = sess.AppendMessage(ai.ToolResultText("c1", "read_file", "ok"), nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	reopened, err := harness.OpenJSONL(path)
	require.NoError(t, err)

	defer reopened.Close() //nolint:errcheck // test cleanup

	restored, err := harness.NewSession(reopened)
	require.NoError(t, err)
	pending, err := restored.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "c2", pending[0].ID)
	assert.JSONEq(t, `{"query":"main"}`, string(pending[0].Args))
}

func TestOpenJSONLRejectsForeignFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name, content string
	}{
		{"empty", ""},
		{"not json", "hello\n"},
		{"wrong type", `{"type":"other","version":1,"id":"x"}` + "\n"},
		{"wrong version", `{"type":"harness_session","version":99,"id":"x"}` + "\n"},
		{"unknown header field", `{"type":"harness_session","version":1,"id":"x","created_at":"2026-01-01T00:00:00Z","future":true}` + "\n"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := fmt.Sprintf("%s/f%d.jsonl", dir, i)
			require.NoError(t, writeFile(path, tt.content))

			_, err := harness.OpenJSONL(path)
			require.Error(t, err)
		})
	}
}

func TestJSONLReadersRejectSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	store, err := harness.CreateJSONL(target, "target", nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(target, link))

	_, err = harness.OpenJSONL(link)
	require.Error(t, err)
	_, err = harness.ReadJSONLMetadata(link)
	require.Error(t, err)
	_, err = harness.ReadJSONLPrefix(link, harness.JSONLPrefixLimits{})
	require.Error(t, err)

	metas, err := (harness.Repo{Dir: dir}).List()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "target", metas[0].ID)
}

func TestJSONLAppendRejectsReplacedPathWithoutModifyingEitherFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	store, err := harness.CreateJSONL(path, "session", nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	opened, err := harness.OpenJSONL(path)
	require.NoError(t, err)
	defer opened.Close() //nolint:errcheck // test cleanup

	originalPath := filepath.Join(dir, "original.jsonl")
	require.NoError(t, os.Rename(path, originalPath))
	replacement := []byte("replacement sentinel\n")
	require.NoError(t, os.WriteFile(path, replacement, 0o600))
	original := mustReadFile(t, originalPath)

	sess, err := harness.NewSession(opened)
	require.NoError(t, err)
	_, err = sess.AppendMessage(ai.UserText("must not persist"), nil)
	require.Error(t, err)
	assert.Equal(t, original, mustReadFile(t, originalPath))
	assert.Equal(t, replacement, mustReadFile(t, path))
}

func TestRepoListReadsOnlyHeader(t *testing.T) {
	t.Parallel()

	repo := harness.Repo{Dir: t.TempDir()}
	store, err := repo.Create("header-only", map[string]string{"workspace": "w"})
	require.NoError(t, err)
	path := store.Metadata().Path
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(path, append(mustReadFile(t, path), []byte("not-json\n")...), 0o600))

	metas, err := repo.List()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "header-only", metas[0].ID)
	assert.Equal(t, "w", metas[0].Extra["workspace"])

	_, err = repo.Open("header-only")
	require.Error(t, err, "full open still validates every entry")
}

func TestReadJSONLPrefixIsValidatedBoundedAndDefensive(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/prefix.jsonl"
	store, err := harness.CreateJSONL(path, "prefix", map[string]string{"workspace": "w"})
	require.NoError(t, err)
	sess, err := harness.NewSession(store)
	require.NoError(t, err)
	appendText(t, sess, ai.UserMessage{}, "first", nil)
	appendText(t, sess, ai.AssistantMessage{}, "second", nil)
	require.NoError(t, sess.SetName("bounded"))
	require.NoError(t, store.Close())

	full, err := harness.ReadJSONLPrefix(path, harness.JSONLPrefixLimits{})
	require.NoError(t, err)
	assert.Equal(t, "prefix", full.Metadata.ID)
	assert.Equal(t, "w", full.Metadata.Extra["workspace"])
	assert.False(t, full.Truncated)
	require.Len(t, full.Entries, 3)

	bounded, err := harness.ReadJSONLPrefix(path, harness.JSONLPrefixLimits{MaxEntries: 1})
	require.NoError(t, err)
	assert.True(t, bounded.Truncated)
	require.Len(t, bounded.Entries, 1)
	bounded.Metadata.Extra["workspace"] = "changed"
	message, ok := bounded.Entries[0].Message.(ai.UserMessage)
	require.True(t, ok)
	message.Parts[0] = ai.Text("changed")

	again, err := harness.ReadJSONLPrefix(path, harness.JSONLPrefixLimits{MaxEntries: 1})
	require.NoError(t, err)
	assert.Equal(t, "w", again.Metadata.Extra["workspace"])
	assert.Equal(t, ai.Text("first"), firstTextPart(t, again.Entries[0].Message))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is created inside the test's private temp directory
	require.NoError(t, err)

	return data
}

func TestRepoLifecycle(t *testing.T) {
	t.Parallel()

	repo := harness.Repo{Dir: t.TempDir() + "/sessions"}

	metas, err := repo.List()
	require.NoError(t, err)
	assert.Empty(t, metas)

	store, err := repo.Create("alpha", nil)
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)
	appendText(t, sess, ai.UserMessage{}, "hi", nil)
	require.NoError(t, store.Close())

	metas, err = repo.List()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "alpha", metas[0].ID)

	opened, err := repo.Open("alpha")
	require.NoError(t, err)
	require.NoError(t, opened.Close())

	require.NoError(t, repo.Delete("alpha"))

	_, err = repo.Open("alpha")
	require.Error(t, err)
}

func TestRepoFork(t *testing.T) {
	t.Parallel()

	repo := harness.Repo{Dir: t.TempDir()}

	store, err := repo.Create("src", nil)
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)

	first := appendText(t, sess, ai.UserMessage{}, "one", nil)
	appendText(t, sess, ai.AssistantMessage{}, "two", nil)
	require.NoError(t, store.Close())

	// Fork at the first entry: only that path is copied.
	forked, err := repo.Fork("src", first, "fork")
	require.NoError(t, err)

	defer forked.Close() //nolint:errcheck // test cleanup

	fsess, err := harness.NewSession(forked)
	require.NoError(t, err)

	entries := fsess.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, first, fsess.LeafID())

	cctx, err := fsess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 1)
}

func TestRepoForkNormalizesReferencesOutsideSelectedBranch(t *testing.T) {
	t.Parallel()

	repo := harness.Repo{Dir: t.TempDir()}
	store, err := repo.Create("src", nil)
	require.NoError(t, err)
	sess, err := harness.NewSession(store)
	require.NoError(t, err)
	root := appendText(t, sess, ai.UserMessage{}, "root", nil)
	abandoned := appendText(t, sess, ai.AssistantMessage{}, "old branch", nil)
	require.NoError(t, sess.MoveTo(root, "old branch summary"))
	require.NoError(t, sess.SetLabel(abandoned, "outside selected path"))
	current := appendText(t, sess, ai.AssistantMessage{}, "current branch", nil)
	before := sess.Entries()

	forked, err := repo.ForkSession(sess, current, "fork", nil)
	require.NoError(t, err)
	defer forked.Close() //nolint:errcheck // test cleanup
	assert.Equal(t, before, sess.Entries())

	forkedSession, err := harness.NewSession(forked)
	require.NoError(t, err)
	assert.Equal(t, current, forkedSession.LeafID())
	assert.Empty(t, forkedSession.Labels())
	entries := forkedSession.Entries()
	require.Len(t, entries, 3)
	assert.Equal(t, harness.KindBranchSummary, entries[1].Kind)
	assert.Equal(t, "root", entries[1].FromID)
	assert.Equal(t, entries[1].ID, entries[2].ParentID)

	contextValue, err := forkedSession.Context()
	require.NoError(t, err)
	require.Len(t, contextValue.Messages, 3)
	assert.Equal(t, ai.AssistantText("current branch"), contextValue.Messages[2])
}
