package harness_test

import (
	"fmt"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
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

func appendText(t *testing.T, sess *harness.Session, role ai.Role, text string, usage *ai.Usage) string {
	t.Helper()

	msg := ai.Message{Role: role, Parts: []ai.Part{ai.Text(text)}}

	id, err := sess.AppendMessage(msg, usage)
	require.NoError(t, err)

	return id
}

func TestJSONLRoundTripAllKinds(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/s.jsonl"

	store, err := harness.CreateJSONL(path, "sess-1", map[string]string{"app": "test"})
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)

	u := ai.Usage{InputTokens: 10, OutputTokens: 5}
	first := appendText(t, sess, ai.RoleUser, "hi", nil)
	appendText(t, sess, ai.RoleAssistant, "hello", &u)

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
	appendText(t, sess, ai.RoleUser, "hi", nil)
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

	first := appendText(t, sess, ai.RoleUser, "one", nil)
	appendText(t, sess, ai.RoleAssistant, "two", nil)
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
