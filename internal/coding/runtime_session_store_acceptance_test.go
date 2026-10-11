package coding

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionStoreLayersExistAfterARealTurn is the end-to-end acceptance test
// for the layered session store. One prompt through a real Runtime must leave
// every documented layer inside a single session directory, and the durable
// event journal must follow the documented delta policy.
func TestSessionStoreLayersExistAfterARealTurn(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("stored answer")))
	sessionDir := runtime.handle.Dir()
	require.NotEmpty(t, sessionDir)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("remember this request")))
	require.NoError(t, runtime.Close(t.Context()))

	// L1: the index record is the store's listing surface.
	record, err := session.ReadMetadataRecord(filepath.Join(sessionDir, session.MetadataFileName))
	require.NoError(t, err)
	assert.Equal(t, session.StoreSchema, record.Schema)
	assert.Equal(t, session.StoreVersion, record.Version)
	assert.Equal(t, session.KindConversation, record.Kind)
	assert.NotEmpty(t, record.Workspace.ID)
	assert.NotEmpty(t, record.Workspace.Path)
	assert.Positive(t, record.Counts.Entries, "the writer keeps the entry count current")

	// L2: transcript and event journal are both authoritative and both present.
	assert.FileExists(t, filepath.Join(sessionDir, session.TranscriptFileName))
	journalPath := filepath.Join(sessionDir, session.JournalFileName)
	require.FileExists(t, journalPath)

	records, err := session.ReadJournal(journalPath, 0)
	require.NoError(t, err)
	require.NotEmpty(t, records)

	for _, entry := range records {
		assert.NotEqual(t, string(EventMessageDelta), entry.Type,
			"streaming deltas stay ephemeral and never enter the journal")
		assert.NotEqual(t, string(EventModelRetry), entry.Type,
			"retry notices stay ephemeral and never enter the journal")
	}

	assert.Equal(t, uint64(len(records)), records[len(records)-1].Seq, "the journal sequence is dense")

	// L3: the model-input snapshots record what the model was sent.
	modelDir := runtime.handle.ModelDir()
	assert.FileExists(t, filepath.Join(modelDir, session.SystemPromptFileName))
	assert.FileExists(t, filepath.Join(modelDir, session.PromptContextFileName))
	assert.FileExists(t, filepath.Join(modelDir, session.ToolDefinitionsFileName))

	// L4: the derived rewind points mark the user prompt.
	assert.FileExists(t, filepath.Join(runtime.handle.StateDir(), "rewind_points.jsonl"))

	// L8: the derived search document carries the prompt for keyword search.
	doc, err := session.ReadSearchDoc(filepath.Join(sessionDir, session.SearchFileName))
	require.NoError(t, err)
	assert.Equal(t, record.ID, doc.ID)
	assert.Contains(t, doc.Content(), "remember this request")

	// The writer lock is released by Close, and the session resumes: the
	// repository locates the directory across groups and reopens it.
	repository, err := session.NewRepository(runtime.paths.SessionsDir())
	require.NoError(t, err)

	reopened, err := repository.Open(t.Context(), session.OpenOptions{
		ID:          record.ID,
		WorkspaceID: record.Workspace.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, sessionDir, reopened.Dir())
	require.NoError(t, reopened.Close())
}
