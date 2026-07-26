package changes_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInspector struct {
	snapshot changes.Snapshot
	report   changes.Report
}

func (f *fakeInspector) Capture(ctx context.Context) (changes.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return changes.Snapshot{}, err
	}

	return f.snapshot, nil
}

func (f *fakeInspector) Changes(ctx context.Context, snapshot changes.Snapshot) (changes.Report, error) {
	if err := ctx.Err(); err != nil {
		return changes.Report{}, err
	}

	if snapshot.Format() != f.snapshot.Format() {
		return changes.Report{}, changes.ErrInvalid
	}

	return f.report, nil
}

func TestInspectorBoundaryAndDefensiveValues(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"dirty":["existing.txt"]}`)
	snapshot, err := changes.NewSnapshot("fake/v1", payload)
	require.NoError(t, err)

	payload[0] = 'x'

	assert.JSONEq(t, `{"dirty":["existing.txt"]}`, string(snapshot.Payload()))

	entries := []changes.Entry{{Path: "agent.txt", Kind: changes.KindModified}}
	report, err := changes.NewReport(entries, "diff", false)
	require.NoError(t, err)

	entries[0].Path = "changed"
	assert.Equal(t, "agent.txt", report.Entries()[0].Path)

	var inspector changes.Inspector = &fakeInspector{snapshot: snapshot, report: report}

	baseline, err := inspector.Capture(t.Context())
	require.NoError(t, err)
	got, err := inspector.Changes(t.Context(), baseline)
	require.NoError(t, err)
	assert.Equal(t, report.Entries(), got.Entries())
	assert.Equal(t, "diff", got.Diff())
	assert.False(t, got.Truncated())
}

func TestChangeValuesRejectInvalidInput(t *testing.T) {
	t.Parallel()

	_, err := changes.NewSnapshot("", nil)
	require.ErrorIs(t, err, changes.ErrInvalid)

	tests := []changes.Entry{
		{Path: "../outside", Kind: changes.KindModified},
		{Path: "file", Kind: "unknown"},
	}
	for _, entry := range tests {
		_, err := changes.NewReport([]changes.Entry{entry}, "", false)
		require.ErrorIs(t, err, changes.ErrInvalid)
	}

	_, err = changes.NewReport([]changes.Entry{
		{Path: "file", Kind: changes.KindAdded},
		{Path: "file", Kind: changes.KindModified},
	}, "", false)
	require.ErrorIs(t, err, changes.ErrInvalid)

	_, err = changes.NewReport([]changes.Entry{{
		Path: "new.go", Kind: changes.KindRenamed,
	}}, "", false)
	require.ErrorIs(t, err, changes.ErrInvalid)

	_, err = changes.NewReport([]changes.Entry{{
		Path: "new.go", PreviousPath: "old.go", Kind: changes.KindModified,
	}}, "", false)
	require.ErrorIs(t, err, changes.ErrInvalid)
}

func TestWorktreeStatusRejectsUnsafeMetadata(t *testing.T) {
	t.Parallel()

	validEntry := changes.StatusEntry{
		Path: "main.go", Index: changes.PathModified, Submodule: "N...",
	}
	_, err := changes.NewWorktreeStatus(
		true,
		changes.Branch{Head: "main\x1b[2J"},
		[]changes.StatusEntry{validEntry},
		changes.DiffSection{},
		changes.DiffSection{},
		changes.DiffSection{},
		0,
	)
	require.ErrorIs(t, err, changes.ErrInvalid)

	invalidSubmodule := validEntry
	invalidSubmodule.Submodule = "Sbad"
	_, err = changes.NewWorktreeStatus(
		true,
		changes.Branch{Head: "main"},
		[]changes.StatusEntry{invalidSubmodule},
		changes.DiffSection{},
		changes.DiffSection{},
		changes.DiffSection{},
		0,
	)
	require.ErrorIs(t, err, changes.ErrInvalid)
}

func TestCodingToolsCloseChangeInspectionBoundary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "code.go"), []byte("package code\n// needle old\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("user dirty\n"), 0o600))

	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	toolCatalog, err := tools.NewCatalog(tree, tools.DefaultLimits())
	require.NoError(t, err)
	toolSet, err := toolCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)

	byName := make(map[string]agent.Tool, len(toolSet))
	for _, tool := range toolSet {
		byName[tool.Decl().Name] = tool
	}

	snapshot, err := changes.NewSnapshot("fake/v1", []byte(`{"dirty":["existing.txt"]}`))
	require.NoError(t, err)
	report, err := changes.NewReport(
		[]changes.Entry{{Path: "code.go", Kind: changes.KindModified}},
		"diff -- code.go",
		false,
	)
	require.NoError(t, err)

	inspector := &fakeInspector{snapshot: snapshot, report: report}

	baseline, err := inspector.Capture(t.Context())
	require.NoError(t, err)

	read := executeTool(t, byName, "read", `{"path":"code.go"}`)
	assert.Contains(t, read, "needle old")
	found := executeTool(t, byName, "glob", `{"pattern":"**/*.go"}`)
	assert.Contains(t, found, "code.go")
	searched := executeTool(t, byName, "grep", `{"pattern":"needle"}`)
	assert.Contains(t, searched, "code.go:2")
	executeTool(t, byName, "apply_patch", `{"patch":"*** Begin Patch\n*** Update File: code.go\n@@\n-// needle old\n+// needle new\n*** End Patch\n"}`)

	observed, err := inspector.Changes(t.Context(), baseline)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "code.go", Kind: changes.KindModified}}, observed.Entries())
	assert.JSONEq(t, `{"dirty":["existing.txt"]}`, string(baseline.Payload()))
	assert.Equal(t, "user dirty\n", string(mustRead(t, filepath.Join(root, "existing.txt"))))
	assert.Contains(t, string(mustRead(t, filepath.Join(root, "code.go"))), "needle new")
}

func executeTool(t *testing.T, toolSet map[string]agent.Tool, name, args string) string {
	t.Helper()

	tool := toolSet[name]
	require.NotNil(t, tool)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{Name: name, Args: ai.JSON(args)})
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(name) //nolint:gosec // Tests read fixed temporary paths.
	require.NoError(t, err)

	return data
}
