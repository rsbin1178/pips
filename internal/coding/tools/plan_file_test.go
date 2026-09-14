package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

func planCatalog(t *testing.T, root string, store *planmode.Store, admitted bool) []agent.Tool {
	t.Helper()

	opened, err := workspace.Open(root)
	require.NoError(t, err)

	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	catalogValue, err := tools.NewCatalog(tree, tools.DefaultLimits(), tools.WithPlanFile(tools.PlanFile{
		Path:     store.Path,
		Admitted: func() bool { return admitted },
		Read: func(ctx context.Context) (string, error) {
			document, err := store.Read(ctx)
			if err != nil {
				return "", err
			}

			return document.Content, nil
		},
		Write: func(ctx context.Context, content string) error {
			_, err := store.Replace(ctx, content)

			return err
		},
	}))
	require.NoError(t, err)

	snapshot, err := catalogValue.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskWrite))
	require.NoError(t, err)

	return snapshot
}

func planTool(t *testing.T, root string, store *planmode.Store, admitted bool, name string) agent.Tool {
	t.Helper()

	for _, tool := range planCatalog(t, root, store, admitted) {
		if tool.Decl().Name == name {
			return tool
		}
	}

	require.FailNow(t, "plan tool must be registered", name)

	return nil
}

func planPatcher(t *testing.T, root string, store *planmode.Store, admitted bool) agent.Tool {
	t.Helper()

	return planTool(t, root, store, admitted, tools.ApplyPatchName)
}

func planReader(t *testing.T, root string, store *planmode.Store, admitted bool) agent.Tool {
	t.Helper()

	return planTool(t, root, store, admitted, "read")
}

func callRead(t *testing.T, tool agent.Tool, path string) (string, error) {
	t.Helper()

	args, err := json.Marshal(map[string]string{"path": path})
	require.NoError(t, err)

	parts, execErr := tool.Exec(t.Context(), agent.ToolCall{
		Name: "read", Args: ai.JSON(args),
	})
	if execErr != nil {
		return "", execErr
	}

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			return text.Text, nil
		}
	}

	return "", nil
}

func callPatch(t *testing.T, tool agent.Tool, document string) (string, error) {
	t.Helper()

	args, err := json.Marshal(map[string]string{"patch": document})
	require.NoError(t, err)

	parts, execErr := tool.Exec(t.Context(), agent.ToolCall{
		Name: tools.ApplyPatchName, Args: ai.JSON(args),
	})
	if execErr != nil {
		return "", execErr
	}

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			return text.Text, nil
		}
	}

	return "", nil
}

func TestApplyPatchWritesOnlyTheAdmittedPlanFile(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan", planmode.DefaultLimits())
	require.NoError(t, err)

	tool := planPatcher(t, t.TempDir(), store, true)

	create := "*** Begin Patch\n*** Add File: " + store.Path() + "\n+# Plan\n+Do it.\n*** End Patch"
	result, err := callPatch(t, tool, create)
	require.NoError(t, err)
	assert.Contains(t, result, "A "+store.Path())

	document, err := store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "# Plan\nDo it.\n", document.Content)

	update := "*** Begin Patch\n*** Update File: " + store.Path() + "\n@@\n # Plan\n-Do it.\n+Ship it.\n*** End Patch"
	_, err = callPatch(t, tool, update)
	require.NoError(t, err)

	document, err = store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "# Plan\nShip it.\n", document.Content)
}

func TestApplyPatchRejectsPlanTargetOutsidePlanMode(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-off", planmode.DefaultLimits())
	require.NoError(t, err)

	tool := planPatcher(t, t.TempDir(), store, false)

	patch := "*** Begin Patch\n*** Add File: " + store.Path() + "\n+escape\n*** End Patch"
	_, err = callPatch(t, tool, patch)
	require.Error(t, err)
}

func TestApplyPatchRejectsMixedPlanAndWorkspaceTargets(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-mixed", planmode.DefaultLimits())
	require.NoError(t, err)

	tool := planPatcher(t, t.TempDir(), store, true)

	patch := "*** Begin Patch\n*** Add File: " + store.Path() + "\n+plan\n*** Add File: note.md\n+note\n*** End Patch"
	_, err = callPatch(t, tool, patch)
	require.Error(t, err)
}

func TestApplyPatchReplacesTheSeededPlanFile(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-seeded", planmode.DefaultLimits())
	require.NoError(t, err)

	created, err := store.Seed(t.Context())
	require.NoError(t, err)
	require.True(t, created)

	tool := planPatcher(t, t.TempDir(), store, true)

	patch := "*** Begin Patch\n*** Add File: " + store.Path() + "\n+# Plan\n+Ship it.\n*** End Patch"
	result, err := callPatch(t, tool, patch)
	require.NoError(t, err)
	assert.Contains(t, result, "M "+store.Path())

	document, err := store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "# Plan\nShip it.\n", document.Content)
}

func TestReadToolReadsTheAdmittedPlanFile(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-read", planmode.DefaultLimits())
	require.NoError(t, err)

	_, err = store.Replace(t.Context(), "# Plan\n\n- Step one\n- Step two\n")
	require.NoError(t, err)

	reader := planReader(t, t.TempDir(), store, true)

	result, err := callRead(t, reader, store.Path())
	require.NoError(t, err)
	assert.Contains(t, result, "1: # Plan")
	assert.Contains(t, result, "3: - Step one")
	assert.Contains(t, result, "4: - Step two")

	_, err = callRead(t, reader, store.Path()+"-other")
	require.Error(t, err)
}

func TestReadToolRejectsThePlanFileOutsidePlanMode(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-read-off", planmode.DefaultLimits())
	require.NoError(t, err)

	_, err = store.Replace(t.Context(), "# Plan\n")
	require.NoError(t, err)

	reader := planReader(t, t.TempDir(), store, false)

	_, err = callRead(t, reader, store.Path())
	require.Error(t, err)
}

func TestReadToolStillReadsWorkspaceFiles(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-plan-workspace", planmode.DefaultLimits())
	require.NoError(t, err)

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "note.md"), []byte("workspace note\n"), 0o600))

	reader := planReader(t, root, store, true)

	result, err := callRead(t, reader, "note.md")
	require.NoError(t, err)
	assert.Contains(t, result, "1: workspace note")
}
