package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogMetadataAndConcurrency(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	descriptors, err := fixture.catalog.Search(
		t.Context(),
		catalog.AllowAll("test", catalog.RiskWrite),
		"",
	)
	require.NoError(t, err)
	require.Len(t, descriptors, 5)
	assert.Equal(t, []string{"read_file", "list_dir", "find_files", "search_text", "apply_patch"}, []string{
		descriptors[0].Name,
		descriptors[1].Name,
		descriptors[2].Name,
		descriptors[3].Name,
		descriptors[4].Name,
	})

	for index := range 4 {
		assert.Equal(t, catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, descriptors[index].Source)
		assert.Equal(t, catalog.RiskRead, descriptors[index].Risk)
		assert.Contains(t, descriptors[index].Tags, "builtin")
		assert.Contains(t, descriptors[index].Tags, "coding")
	}

	assert.Equal(t, catalog.RiskWrite, descriptors[4].Risk)

	snapshot, err := fixture.catalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)

	for index, tool := range snapshot {
		concurrencySafe, ok := tool.(agent.ConcurrencySafe)
		if index < 4 {
			require.True(t, ok)
			assert.True(t, concurrencySafe.Concurrent())
		} else {
			assert.False(t, ok)
		}
	}
}

func TestReadFilePaginatesAndRejectsUnsafeContent(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ReadLines = 2
	fixture := newToolFixture(t, limits)
	fixture.write("file.txt", "alpha\nbeta\ngamma\n")

	text, err := fixture.exec(t.Context(), "read_file", `{"path":"file.txt","offset":2,"limit":1}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, "read_file", header.Tool)
	assert.True(t, header.Truncated)
	assert.Equal(t, "lines", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 3, *header.Next.Offset)
	assert.Equal(t, "2: beta\n", body)

	fixture.write("binary", "text\x00data")
	_, err = fixture.exec(t.Context(), "read_file", `{"path":"binary"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
	assert.Equal(t, "binary_file", header.Code)

	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("sentinel"), 0o600))
	_, err = fixture.exec(t.Context(), "read_file", `{"path":"../outside"}`)
	require.Error(t, err)
	header, _, parseErr = tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "outside_workspace", header.Code)
	assert.Equal(t, "sentinel", string(mustRead(t, outside)))
}

func TestListDirIsStableTypedAndPaginated(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ListEntries = 2
	fixture := newToolFixture(t, limits)
	fixture.write("b.txt", "b")
	fixture.write("a.txt", "a")
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, "dir"), 0o700))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(fixture.root, "link")))

	text, err := fixture.exec(t.Context(), "list_dir", `{}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "a.txt\nb.txt\n", body)
	assert.True(t, header.Truncated)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 2, *header.Next.Offset)

	text, err = fixture.exec(t.Context(), "list_dir", `{"offset":2,"limit":2}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.False(t, header.Truncated)
	assert.Equal(t, "dir/\nlink@\n", body)
}

func TestFindFilesUsesRecursiveGlobWithoutFollowingSymlinks(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.go", "package a")
	fixture.write("nested/b.go", "package b")
	fixture.write("nested/c.txt", "text")
	fixture.write(".git/config", "secret")
	require.NoError(t, os.Symlink("nested", filepath.Join(fixture.root, "linked")))

	text, err := fixture.exec(t.Context(), "find_files", `{"pattern":"**/*.go"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, "a.go\nnested/b.go\n", body)
	assert.NotContains(t, body, ".git")
	assert.NotContains(t, body, "linked")

	_, err = fixture.exec(t.Context(), "find_files", `{"pattern":"**","path":"linked"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "symlink_not_allowed", header.Code)
}

func TestSearchTextSupportsGlobCaseAndContinuation(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.SearchMatches = 1
	limits.SearchLineBytes = 16
	fixture := newToolFixture(t, limits)
	fixture.write("a.go", "first Needle line that is long\nnone\n")
	fixture.write("nested/b.go", "needle second\n")
	fixture.write("nested/c.txt", "needle ignored\n")
	fixture.write("binary.go", "needle\x00binary")
	fixture.write(".git/config", "needle hidden")

	text, err := fixture.exec(
		t.Context(),
		"search_text",
		`{"query":"needle","glob":"**/*.go","case_sensitive":false,"limit":1}`,
	)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "matches", header.Reason)
	assert.Equal(t, 1, header.Counts.Matches)
	assert.Equal(t, 1, header.Counts.TruncatedLines)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 1, *header.Next.Offset)
	assert.Contains(t, body, "a.go:1:7:first Needle lin")

	text, err = fixture.exec(
		t.Context(),
		"search_text",
		`{"query":"needle","glob":"**/*.go","case_sensitive":false,"offset":1,"limit":1}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "nested/b.go:1:1:needle second\n", body)

	_, err = fixture.exec(t.Context(), "search_text", `{"query":"[","regex":true}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
}

func TestApplyPatchAddsUpdatesDeletesAndPreservesMode(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("update.txt", "old\n")
	fixture.write("delete.txt", "gone\n")
	//nolint:gosec // Executable permissions are intentional input to the mode-preservation test.
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "update.txt"), 0o751))

	text, err := fixture.exec(t.Context(), "apply_patch", `{"patch":"*** Begin Patch\n*** Add File: added.txt\n+added\n*** Update File: update.txt\n@@\n-old\n+new\n*** Delete File: delete.txt\n*** End Patch\n"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, 3, header.Counts.Files)
	assert.Equal(t, "A added.txt\nD delete.txt\nM update.txt\n", body)
	assert.Equal(t, "added\n", string(mustRead(t, filepath.Join(fixture.root, "added.txt"))))
	assert.Equal(t, "new\n", string(mustRead(t, filepath.Join(fixture.root, "update.txt"))))
	_, err = os.Stat(filepath.Join(fixture.root, "delete.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	info, err := os.Stat(filepath.Join(fixture.root, "update.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o751), info.Mode().Perm())

	addedInfo, err := os.Stat(filepath.Join(fixture.root, "added.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), addedInfo.Mode().Perm())
	assert.Empty(t, temporaryFiles(t, fixture.root))
}

func TestApplyPatchConflictLeavesWorkspaceUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("file.txt", "actual\n")

	_, err := fixture.exec(t.Context(), "apply_patch", `{"patch":"*** Begin Patch\n*** Update File: file.txt\n@@\n-expected\n+changed\n*** End Patch\n"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "conflict", header.Code)
	assert.Equal(t, "actual\n", string(mustRead(t, filepath.Join(fixture.root, "file.txt"))))
	assert.Empty(t, temporaryFiles(t, fixture.root))
}

func TestReadToolsHonorCanceledContext(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("file", "content")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for _, name := range []string{"read_file", "list_dir", "find_files", "search_text"} {
		args := map[string]string{
			"read_file":   `{"path":"file"}`,
			"list_dir":    `{}`,
			"find_files":  `{"pattern":"**"}`,
			"search_text": `{"query":"content"}`,
		}[name]
		_, err := fixture.exec(ctx, name, args)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	}
}

func TestResultParserRejectsUnknownEnvelope(t *testing.T) {
	t.Parallel()

	_, _, err := tools.ParseResult(`{"schema":"other","ok":true,"tool":"read_file"}`)
	require.Error(t, err)
	_, _, err = tools.ParseResult(`not json`)
	require.Error(t, err)
}

type toolFixture struct {
	t       *testing.T
	root    string
	catalog *catalog.Catalog
	tools   map[string]agent.Tool
}

func newToolFixture(t *testing.T, limits tools.Limits) *toolFixture {
	t.Helper()

	root := t.TempDir()
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	toolCatalog, err := tools.NewCatalog(tree, limits)
	require.NoError(t, err)
	snapshot, err := toolCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)

	byName := make(map[string]agent.Tool, len(snapshot))
	for _, tool := range snapshot {
		byName[tool.Decl().Name] = tool
	}

	return &toolFixture{t: t, root: root, catalog: toolCatalog, tools: byName}
}

func (f *toolFixture) write(relative, content string) {
	f.t.Helper()

	name := filepath.Join(f.root, filepath.FromSlash(relative))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(f.t, os.WriteFile(name, []byte(content), 0o600))
}

func (f *toolFixture) exec(ctx context.Context, name, args string) (string, error) {
	tool := f.tools[name]
	if tool == nil {
		return "", assert.AnError
	}

	parts, err := tool.Exec(ctx, agent.ToolCall{Name: name, Args: ai.JSON(args)})
	if err != nil {
		return "", err
	}

	if len(parts) != 1 {
		return "", assert.AnError
	}

	text, ok := parts[0].(ai.TextPart)
	if !ok {
		return "", assert.AnError
	}

	return text.Text, nil
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(name) //nolint:gosec // Tests read fixed temporary paths.
	require.NoError(t, err)

	return data
}

func temporaryFiles(t *testing.T, root string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, ".pips-*"))
	require.NoError(t, err)

	return matches
}
