package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
	assert.Equal(t, []string{"read", "ls", "glob", "grep", "apply_patch"}, []string{
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

	grepDeclaration := fixture.tools["grep"].Decl()
	require.NotNil(t, grepDeclaration.InputSchema)
	assert.Equal(
		t,
		[]string{"pattern", "fixed_strings", "path", "glob", "case_sensitive", "offset", "limit"},
		grepDeclaration.InputSchema.Required,
	)
	assert.Contains(t, grepDeclaration.InputSchema.Properties, "fixed_strings")
	assert.True(t, grepDeclaration.InputSchema.Properties["fixed_strings"].Nullable)
	assert.NotContains(t, grepDeclaration.InputSchema.Properties, "query")
	assert.NotContains(t, grepDeclaration.InputSchema.Properties, "regex")
}

func TestTaskCatalogIsBoundedApplicationState(t *testing.T) {
	t.Parallel()

	value, err := tools.NewTaskCatalog()
	require.NoError(t, err)
	descriptors, err := value.Search(t.Context(), catalog.AllowAll("test", catalog.RiskRead), "")
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	assert.Equal(t, tasklist.ToolName, descriptors[0].Name)
	assert.Equal(t, catalog.RiskRead, descriptors[0].Risk)

	snapshot, err := value.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskRead))
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	parts, err := snapshot[0].Exec(t.Context(), agent.ToolCall{
		ID: "task-1", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[{"step":"Test","status":"completed"}]}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.Contains(t, parts[0].(ai.TextPart).Text, `"completed":1`)

	_, err = snapshot[0].Exec(t.Context(), agent.ToolCall{
		ID: "task-2", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[{"step":"Test","status":"completed"}],"unknown":true}`),
	})
	require.Error(t, err)
}

func TestReadPaginatesAndRejectsUnsafeContent(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ReadLines = 2
	fixture := newToolFixture(t, limits)
	fixture.write("file.txt", "alpha\nbeta\ngamma\n")

	text, err := fixture.exec(t.Context(), "read", `{"path":"file.txt","offset":2,"limit":1}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, "read", header.Tool)
	assert.True(t, header.Truncated)
	assert.Equal(t, "lines", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 3, *header.Next.Offset)
	assert.Equal(t, "2: beta\n", body)

	fixture.write("binary", "text\x00data")
	_, err = fixture.exec(t.Context(), "read", `{"path":"binary"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
	assert.Equal(t, "binary_file", header.Code)

	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("sentinel"), 0o600))
	_, err = fixture.exec(t.Context(), "read", `{"path":"../outside"}`)
	require.Error(t, err)
	header, _, parseErr = tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "outside_workspace", header.Code)
	assert.Equal(t, "sentinel", string(mustRead(t, outside)))
}

func TestLSIsStableTypedAndPaginated(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ListEntries = 2
	fixture := newToolFixture(t, limits)
	fixture.write("b.txt", "b")
	fixture.write("a.txt", "a")
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, "dir"), 0o700))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(fixture.root, "link")))

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "a.txt\nb.txt\n", body)
	assert.True(t, header.Truncated)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 2, *header.Next.Offset)

	text, err = fixture.exec(t.Context(), "ls", `{"offset":2,"limit":2}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.False(t, header.Truncated)
	assert.Equal(t, "dir/\nlink@\n", body)
}

func TestGlobUsesRecursivePatternWithoutFollowingSymlinks(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.go", "package a")
	fixture.write("nested/b.go", "package b")
	fixture.write("nested/c.txt", "text")
	fixture.write(".git/config", "secret")
	require.NoError(t, os.Symlink("nested", filepath.Join(fixture.root, "linked")))

	text, err := fixture.exec(t.Context(), "glob", `{"pattern":"**/*.go"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, "a.go\nnested/b.go\n", body)
	assert.NotContains(t, body, ".git")
	assert.NotContains(t, body, "linked")

	_, err = fixture.exec(t.Context(), "glob", `{"pattern":"**","path":"linked"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "symlink_not_allowed", header.Code)
}

func TestGrepSupportsGlobCaseAndContinuation(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.GrepMatches = 1
	limits.GrepLineBytes = 16
	fixture := newToolFixture(t, limits)
	fixture.write("a.go", "first Needle line that is long\nnone\n")
	fixture.write("nested/b.go", "needle second\n")
	fixture.write("nested/c.txt", "needle ignored\n")
	fixture.write("binary.go", "needle\x00binary")
	fixture.write(".git/config", "needle hidden")

	text, err := fixture.exec(
		t.Context(),
		"grep",
		`{"pattern":"needle","glob":"**/*.go","case_sensitive":false,"limit":1}`,
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
		"grep",
		`{"pattern":"needle","glob":"**/*.go","case_sensitive":false,"offset":1,"limit":1}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "nested/b.go:1:1:needle second\n", body)

	_, err = fixture.exec(t.Context(), "grep", `{"pattern":"["}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
}

func TestGrepUsesRegexByDefaultAndSupportsFixedStrings(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("values.txt", "alpha[beta\nalphaXbeta\n")

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"alpha.beta"}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "values.txt:1:1:alpha[beta\nvalues.txt:2:1:alphaXbeta\n", body)

	text, err = fixture.exec(
		t.Context(),
		"grep",
		`{"pattern":"alpha[beta","fixed_strings":true}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "values.txt:1:1:alpha[beta\n", body)
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

func TestApplyPatchAddCreatesNestedParents(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	text, err := fixture.exec(t.Context(), "apply_patch", `{"patch":"*** Begin Patch\n*** Add File: prisma/schema/schema.prisma\n+datasource db {}\n*** End Patch\n"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Equal(t, "A prisma/schema/schema.prisma\n", body)
	assert.Equal(t,
		"datasource db {}\n",
		string(mustRead(t, filepath.Join(fixture.root, "prisma", "schema", "schema.prisma"))),
	)
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

	for _, name := range []string{"read", "ls", "glob", "grep"} {
		args := map[string]string{
			"read": `{"path":"file"}`,
			"ls":   `{}`,
			"glob": `{"pattern":"**"}`,
			"grep": `{"pattern":"content"}`,
		}[name]
		_, err := fixture.exec(ctx, name, args)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	}
}

func TestResultParserRejectsUnknownEnvelope(t *testing.T) {
	t.Parallel()

	_, _, err := tools.ParseResult(`{"schema":"other","ok":true,"tool":"read"}`)
	require.Error(t, err)
	_, _, err = tools.ParseResult(`not json`)
	require.Error(t, err)
}

func TestResultParserKeepsProblemMetadataBackwardCompatible(t *testing.T) {
	t.Parallel()

	legacy, _, err := tools.ParseResult(
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"operation_invalid"}`,
	)
	require.NoError(t, err)
	assert.Nil(t, legacy.Problem)

	current, body, err := tools.ParseResult(
		`{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"cwd_not_found","problem":{"field":"cwd","retryable":true,"hint":"choose an existing Workspace directory"}}` +
			"\n\nchoose an existing Workspace directory",
	)
	require.NoError(t, err)
	require.NotNil(t, current.Problem)
	assert.Equal(t, "cwd", current.Problem.Field)
	assert.True(t, current.Problem.Retryable)
	assert.Equal(t, "choose an existing Workspace directory", current.Problem.Hint)
	assert.Equal(t, "choose an existing Workspace directory", body)
}

func TestToolPathAndOffsetRobustness(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("file.txt", "line1\nline2\n")

	// Read with offset 0 should start from line 1
	text, err := fixture.exec(t.Context(), "read", `{"path":"file.txt","offset":0}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Contains(t, body, "1: line1\n2: line2\n")

	// ls with path "" should list workspace root
	text, err = fixture.exec(t.Context(), "ls", `{"path":""}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Contains(t, body, "file.txt\n")

	// glob with path "" should glob from workspace root
	text, err = fixture.exec(t.Context(), "glob", `{"pattern":"*.txt","path":""}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Contains(t, body, "file.txt\n")

	// grep with path "" should grep from workspace root
	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"line1","path":""}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Contains(t, body, "file.txt:1:1:line1\n")
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
