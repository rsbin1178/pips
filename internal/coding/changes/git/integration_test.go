package git

import (
	"os"
	"testing"

	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitIntegration(t *testing.T) {
	if os.Getenv("PIPS_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set PIPS_SANDBOX_INTEGRATION=1 to exercise the native sandbox")
	}

	t.Parallel()

	setup := newGitFixture(t)
	setup.write("file.txt", "before\n")
	setup.commitAll("initial")
	indexBefore := setup.indexDigest()

	ws, err := workspace.Open(setup.root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	})
	require.NoError(t, err)
	executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
		TempRoot:    privateTempDir(t),
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	inspector, err := New(ws, tree, policy, executor, Config{
		GitPath:  setup.gitPath,
		TempRoot: privateTempDir(t),
		Limits:   DefaultLimits(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspector.Close()) })

	snapshot, err := inspector.Capture(t.Context())
	require.NoError(t, err)
	setup.write("file.txt", "after\n")

	report, err := inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "file.txt", Kind: changes.KindModified}}, report.Entries())
	assert.Contains(t, report.Diff(), "-before")
	assert.Contains(t, report.Diff(), "+after")
	assert.Equal(t, indexBefore, setup.indexDigest())
}
