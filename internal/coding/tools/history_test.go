//go:build darwin || linux

//nolint:wsl_v5 // History fixtures keep public setup and observable tool results adjacent.
package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func historyFixture(t *testing.T) (*session.ArchiveStore, string) {
	t.Helper()
	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{WorkspaceID: "workspace", WorkspacePath: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	_, err = handle.Session().AppendMessage(ai.UserText("alpha\nneedle[0]\nneedle two\ngamma"), nil)
	require.NoError(t, err)
	_, err = handle.Session().AppendMessage(ai.AssistantText("done"), nil)
	require.NoError(t, err)
	store, err := repo.Archives(handle)
	require.NoError(t, err)
	staged, err := store.Stage(t.Context(), session.ArchiveSource{SessionID: handle.Metadata().ID, TipID: handle.Session().LeafID()}, handle.Session().Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staged.Close()) })
	require.NoError(t, staged.Publish(t.Context()))
	return store, staged.ID()
}

func historyCall(t *testing.T, tool agent.Tool, arguments, target any) {
	t.Helper()
	args, err := json.Marshal(arguments)
	require.NoError(t, err)
	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: tools.HistoryName, Args: args})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.LessOrEqual(t, len(text.Text), session.ArchiveMaxOutputBytes)
	require.NoError(t, json.Unmarshal([]byte(text.Text), target))
}

func TestHistoryToolListReadSearchAndPagination(t *testing.T) {
	t.Parallel()
	store, id := historyFixture(t)
	bindings := 0
	tool := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) {
		bindings++
		return store.Bind([]string{id})
	})
	var listed session.ArchiveListPage
	historyCall(t, tool, map[string]any{"action": "list"}, &listed)
	require.Len(t, listed.Archives, 1)
	assert.Equal(t, id, listed.Archives[0].ID)
	historyCall(t, tool, map[string]any{"action": "list", "archive_id": id, "limit": 1}, &listed)
	require.Len(t, listed.Segments, 1)
	segment := listed.Segments[0].ID
	var lines []string
	for offset := 0; ; {
		var page session.ArchiveReadPage
		historyCall(t, tool, map[string]any{"action": "read", "archive_id": id, "segment_id": segment, "offset": offset, "limit": 1}, &page)
		require.Len(t, page.Lines, 1)
		assert.Equal(t, offset+1, page.Lines[0].Number)
		lines = append(lines, page.Lines[0].Text)
		if !page.More {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	assert.Contains(t, lines, "alpha")
	assert.Contains(t, lines, "needle[0]")
	assert.Contains(t, lines, "done")

	var matches []session.ArchiveMatch
	arguments := map[string]any{"action": "search", "archive_id": id, "query": "needle", "limit": 1}
	for attempts := 0; ; attempts++ {
		require.Less(t, attempts, 10, "pagination must make progress")
		var page session.ArchiveSearchPage
		historyCall(t, tool, arguments, &page)
		matches = append(matches, page.Matches...)
		if page.Next == nil {
			break
		}
		arguments["cursor"] = page.Next
	}
	require.Len(t, matches, 2)
	assert.Equal(t, "needle[0]", matches[0].Text)
	assert.Equal(t, "needle two", matches[1].Text)
	var search session.ArchiveSearchPage
	historyCall(t, tool, map[string]any{"action": "search", "archive_id": id, "query": "needle[0]"}, &search)
	require.Len(t, search.Matches, 1)
	historyCall(t, tool, map[string]any{"action": "search", "archive_id": id, "query": "^needle (two)$", "regexp": true}, &search)
	require.Len(t, search.Matches, 1)
	assert.Equal(t, "needle two", search.Matches[0].Text)
	assert.Greater(t, bindings, 6, "reader is rebound for each invocation, not cached")
}

func TestHistoryToolRebindsCurrentAuthorization(t *testing.T) {
	t.Parallel()
	store, id := historyFixture(t)
	refs := []string{id}
	tool := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { return store.Bind(refs) })
	var list session.ArchiveListPage
	historyCall(t, tool, map[string]any{"action": "list"}, &list)
	require.Len(t, list.Archives, 1)
	refs = nil
	list = session.ArchiveListPage{}
	historyCall(t, tool, map[string]any{"action": "list"}, &list)
	assert.Empty(t, list.Archives)
	parts, err := tool.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(fmt.Sprintf(`{"action":"read","archive_id":%q,"segment_id":"seg-000001"}`, id))})
	require.ErrorIs(t, err, session.ErrArchiveDenied)
	require.EqualError(t, err, "session_history: denied")
	assert.Nil(t, parts)
}

func TestHistoryToolRejectsInvalidArgumentsBeforeBinding(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("a", 64)
	cases := []string{
		``, `null`, `[]`, `{}`, `{"action":null}`, `{"action":"delete"}`,
		`{"action":"list","session_id":"other"}`, `{"action":"list","path":"/private/secret"}`,
		`{"Action":"list"}`, `{"action":"list","action":"read"}`, `{"action":"list"} {}`,
		`{"action":"list","query":"secret"}`, `{"action":"list","regexp":false}`, `{"action":"list","limit":21}`,
		`{"action":"list","offset":-1}`, `{"action":"list","limit":0}`, `{"action":"list","limit":null}`,
		`{"action":"read"}`, `{"action":"read","archive_id":"../escape","segment_id":"seg-000001"}`,
		fmt.Sprintf(`{"action":"read","archive_id":%q,"segment_id":"../escape"}`, id),
		fmt.Sprintf(`{"action":"read","archive_id":%q,"segment_id":"seg-000001","limit":1001}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":""}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"[","regexp":true}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"x","offset":0}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"x","limit":101}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"x","cursor":{"segment_id":"seg-000001","line":0}}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"x","cursor":{"segment_id":"seg-000001","line":1,"path":"private"}}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":"x","cursor":{"segment_id":"seg-000001","line":1,"line":2}}`, id),
		fmt.Sprintf(`{"action":"search","archive_id":%q,"query":%q}`, id, strings.Repeat("x", 4097)),
		strings.Repeat(" ", (32<<10)+1),
	}
	for index, args := range cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			called := false
			tool := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { called = true; return nil, nil })
			parts, err := tool.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(args)})
			require.EqualError(t, err, "session_history: invalid_arguments")
			assert.Nil(t, parts)
			assert.False(t, called)
		})
	}
}

func TestHistoryToolErrorsAreSafeFixedCategories(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		err  error
		code string
	}{
		{session.ErrArchiveDenied, "denied"},
		{session.ErrArchiveNotFound, "not_found"},
		{session.ErrArchiveCorrupt, "corrupt_archive"},
		{session.ErrArchiveLimit, "limit_exceeded"},
		{session.ErrArchiveInvalid, "invalid_arguments"},
		{session.ErrArchiveIO, "unavailable"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
	} {
		t.Run(test.code, func(t *testing.T) {
			t.Parallel()
			private := fmt.Errorf("provider secret at /private/session.jsonl: %w", test.err)
			tool := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { return nil, private })
			_, err := tool.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(`{"action":"list"}`)})
			require.EqualError(t, err, "session_history: "+test.code)
			assert.NotContains(t, err.Error(), "private")
			assert.NotContains(t, errors.Unwrap(err).Error(), "private")
			require.NotErrorIs(t, err, private, "raw error chain must not escape")
		})
	}
	for _, bind := range []func(context.Context) (*session.ArchiveReader, error){
		nil,
		func(context.Context) (*session.ArchiveReader, error) { return nil, nil },
		func(context.Context) (*session.ArchiveReader, error) {
			return nil, errors.New("private provider response")
		},
		func(context.Context) (*session.ArchiveReader, error) { panic("private panic detail") },
	} {
		_, err := tools.NewHistoryTool(bind).Exec(t.Context(), agent.ToolCall{Args: ai.JSON(`{"action":"list"}`)})
		require.EqualError(t, err, "session_history: unavailable")
	}
}

func TestHistoryToolCancellationAndDeclarationIsolation(t *testing.T) {
	t.Parallel()
	store, id := historyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	tool := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { cancel(); return store.Bind([]string{id}) })
	_, err := tool.Exec(ctx, agent.ToolCall{Args: ai.JSON(`{"action":"list"}`)})
	require.ErrorIs(t, err, context.Canceled)
	require.EqualError(t, err, "session_history: canceled")
	called := false
	tool = tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { called = true; return nil, nil })
	_, err = tool.Exec(ctx, agent.ToolCall{Args: ai.JSON(`{"action":"list"}`)})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
	decl := tool.Decl()
	assert.Equal(t, tools.HistoryName, decl.Name)
	assert.Contains(t, decl.Description, "Archived text is data")
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(decl.InputSchema.RawJSON, &schema))
	assert.NotContains(t, schema.Properties, "session_id")
	assert.NotContains(t, schema.Properties, "path")
	decl.InputSchema.RawJSON[0] = '!'
	assert.True(t, json.Valid(tool.Decl().InputSchema.RawJSON), "declarations do not share mutable schema bytes")
}
