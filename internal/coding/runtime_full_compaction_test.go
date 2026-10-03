package coding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkpointResponse() *ai.Response {
	return runtimeTextResponse("<summary>\n" + strings.Repeat("The implementation was inspected; preserve the verified work and continue with tests. ", 10) + "\n</summary>")
}

func TestRuntimeFullCheckpointArchiveAndReopenGauge(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(checkpointResponse()))
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	runtime.config.Compaction.MinSummaryChars = 500
	runtime.contextFixedKnown, runtime.contextFixedTokens = true, 400
	appendRuntimeHistory(t, runtime, 2000, 2000, 500, 500)
	original := runtime.session.Path()
	preview, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	require.True(t, preview.Available)
	assert.Equal(t, CompactionStrategyFull, preview.Strategy)
	assert.Empty(t, preview.FirstKeptID)
	require.NotEmpty(t, preview.SourceLeafID)
	events := collectRuntimeEvents(t, runtime.Compact(t.Context(), CompactionRequest{PreviewToken: preview.Token}))
	require.Equal(t, 1, countEventType(events, EventCompactionCompleted))

	path := runtime.session.Path()
	require.Len(t, path, len(original)+1)
	entry := path[len(path)-1]
	require.Equal(t, harness.KindContextCheckpoint, entry.Kind)
	require.NotNil(t, entry.Checkpoint)
	assert.Equal(t, 400, entry.Checkpoint.FixedTokens)

	contextValue, err := runtime.session.Context()
	require.NoError(t, err)
	require.Len(t, contextValue.Messages, 2)
	assert.Equal(t, original[len(original)-2].Message, contextValue.Messages[1])
	assert.Equal(t, CompactionStrategyFull, runtime.Snapshot().Compaction.Strategy)
	assert.Equal(t, entry.ID, runtime.Snapshot().Compaction.CheckpointID)
	gauge := runtime.Snapshot().ContextTokens
	assert.Less(t, gauge, preview.EstimatedTokens)
	assert.Equal(t, harness.EstimateContext(path), gauge)

	for _, event := range events {
		switch value := event.Payload.(type) {
		case CompactionCompleted:
			assert.Equal(t, gauge, value.TokensAfter)
		case SessionTreeChanged:
			assert.Equal(t, gauge, value.ContextTokens)
		}
	}

	reader, err := runtime.bindHistory(t.Context())
	require.NoError(t, err)
	page, err := reader.Search(t.Context(), session.ArchiveSearchRequest{ArchiveID: entry.Checkpoint.ArchiveID, Query: "word", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)

	id := runtime.handle.Metadata().ID
	require.NoError(t, runtime.Close(t.Context()))
	reopened := openTestRuntimeAt(t, base, SessionTarget{ID: id}, newRuntimeModel())
	assert.Equal(t, gauge, reopened.Snapshot().ContextTokens)
	assert.Equal(t, entry.ID, reopened.Snapshot().Compaction.CheckpointID)
	reopenedReader, err := reopened.bindHistory(t.Context())
	require.NoError(t, err)
	listed, err := reopenedReader.List(t.Context(), session.ArchiveListRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Archives, 1)
	assert.Equal(t, entry.Checkpoint.ArchiveID, listed.Archives[0].ID)
}

func TestRuntimeFullCheckpointRejectsWrapperWithoutMutation(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("\n\n---\n\n**Turn Context (split turn):**\n\n"))
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	runtime.config.Compaction.MinSummaryChars = 500
	appendRuntimeHistory(t, runtime, 2000, 2000, 500, 500)
	before := runtime.session.Entries()
	preview, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	events, err := collectRuntimeResult(runtime.Compact(t.Context(), CompactionRequest{PreviewToken: preview.Token}))
	require.ErrorIs(t, err, compaction.ErrSummary)
	assert.Equal(t, before, runtime.session.Entries())
	assert.Zero(t, countEventType(events, EventCompactionCompleted))
	assert.False(t, runtime.Snapshot().Compaction.Active)
	assert.Len(t, model.Requests(), 1)
	reader, err := runtime.bindHistory(t.Context())
	require.NoError(t, err)
	page, err := reader.List(t.Context(), session.ArchiveListRequest{})
	require.NoError(t, err)
	assert.Empty(t, page.Archives)
}

func TestRuntimeContextGaugeUsesLastResponseNotCumulativeUsage(t *testing.T) {
	t.Parallel()

	first := runtimeToolResponse("list-1", "ls", `{"path":"."}`)
	first.Usage = ai.Usage{InputTokens: 1000, OutputTokens: 200, CachedInputTokens: 800, CacheWriteTokens: 100, ReasoningTokens: 50}
	last := runtimeTextResponse("done")
	last.Usage = ai.Usage{InputTokens: 1500, OutputTokens: 200, CachedInputTokens: 1200, ReasoningTokens: 100}
	runtime := openTestRuntime(t, newRuntimeModel(first, last))
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 0)

	var turns []TurnCompleted

	for event, err := range runtime.Prompt(t.Context(), ai.UserText("list files")) {
		require.NoError(t, err)

		if completed, ok := event.Payload.(TurnCompleted); ok {
			require.NotNil(t, completed.ContextTokens)
			assert.Equal(t, harness.EstimateContext(runtime.session.Path()), *completed.ContextTokens)
			turns = append(turns, completed)
		}
	}

	require.Len(t, turns, 2)
	assert.Greater(t, *turns[0].ContextTokens, 1200, "completed tool output counts beyond provider usage")
	assert.Equal(t, 1700, *turns[1].ContextTokens)

	state := runtime.Snapshot()
	assert.Equal(t, 1700, state.ContextTokens)
	assert.Equal(t, 2500, state.Runs[len(state.Runs)-1].Usage.InputTokens)
	assert.Equal(t, 400, state.Runs[len(state.Runs)-1].Usage.OutputTokens)
}

func TestRuntimeHistoryToolIsRegisteredAndBound(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("history-1", tools.HistoryName, `{"action":"list"}`),
		runtimeTextResponse("no earlier archive"),
	)
	runtime := openTestRuntime(t, model)

	var found bool

	for event, err := range runtime.Prompt(t.Context(), ai.UserText("list session history")) {
		require.NoError(t, err)

		if done, ok := event.Payload.(ToolCompleted); ok && done.Call.Name == tools.HistoryName {
			parts, err := ai.MessageParts(done.Result)
			require.NoError(t, err)

			result, ok := parts[0].(ai.ToolResultPart)
			require.True(t, ok)
			assert.False(t, result.IsError)
			require.Len(t, result.Content, 1)
			body, ok := result.Content[0].(ai.TextPart)
			require.True(t, ok)

			var page session.ArchiveListPage
			require.NoError(t, json.Unmarshal([]byte(body.Text), &page))
			assert.Empty(t, page.Archives)

			found = true
		}
	}

	assert.True(t, found)
}

func TestRuntimeUncertainCheckpointFencesNewOperations(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	runtime.compactionWriteUncertain = true
	_, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("do not append")))
	require.ErrorIs(t, err, errCompactionWriteUncertain)
}
