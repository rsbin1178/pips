package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectorSupportsRepositorySubdirectoryWorkspace(t *testing.T) {
	t.Parallel()

	repository := newGitFixture(t)
	repository.write("sub/file.txt", "before\n")
	repository.commitAll("initial")

	subdirectory := newGitFixtureAtRoot(
		t,
		filepath.Join(repository.root, "sub"),
		DefaultLimits(),
	)
	snapshot, err := subdirectory.inspector.Capture(t.Context())
	require.NoError(t, err)
	subdirectory.write("file.txt", "after\n")

	report, err := subdirectory.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "file.txt", Kind: changes.KindModified}}, report.Entries())
	assert.Contains(t, report.Diff(), "file.txt")
	assert.NotContains(t, report.Diff(), "sub/file.txt")
}

func TestInspectorSupportsLinkedWorktreeGitFile(t *testing.T) {
	t.Parallel()

	repository := newGitFixture(t)
	repository.write("file.txt", "before\n")
	repository.commitAll("initial")

	worktree := filepath.Join(t.TempDir(), "linked-worktree")
	repository.git("worktree", "add", "--quiet", "--detach", worktree)

	linked := newGitFixtureAtRoot(t, worktree, DefaultLimits())
	gitInfo, err := os.Stat(filepath.Join(worktree, ".git"))
	require.NoError(t, err)
	assert.True(t, gitInfo.Mode().IsRegular())

	snapshot, err := linked.inspector.Capture(t.Context())
	require.NoError(t, err)
	linked.write("file.txt", "after\n")
	report, err := linked.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "file.txt", Kind: changes.KindModified}}, report.Entries())
}

func TestInspectorDisablesRepositoryControlledProcessHooks(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("file.txt", "before\n")
	fixture.write(".gitattributes", "*.txt diff=unsafe\n")
	fixture.commitAll("initial")

	marker := filepath.Join(t.TempDir(), "invoked")
	script := filepath.Join(t.TempDir(), "unsafe-diff")
	//nolint:gosec // Executable fixture detects forbidden repository-controlled process hooks.
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 1\n"), 0o700))
	fixture.git("config", "diff.external", script)
	fixture.git("config", "diff.unsafe.command", script)
	fixture.git("config", "core.fsmonitor", script)

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	fixture.write("file.txt", "after\n")
	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Contains(t, report.Diff(), "-before")
	assert.Contains(t, report.Diff(), "+after")

	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestInspectorTruncatesDiffWithoutDroppingEntries(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.DiffBytes = 1024
	fixture := newGitFixtureWithoutRepository(t, limits)
	fixture.git("init", "--quiet")
	fixture.write("large.txt", strings.Repeat("before line\n", 1000))
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	fixture.write("large.txt", strings.Repeat("after line\n", 1000))
	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "large.txt", Kind: changes.KindModified}}, report.Entries())
	assert.True(t, report.Truncated())
	assert.Contains(t, report.Diff(), "[diff truncated]")
}

func TestInspectorClassifiesBinaryLargeModeAndRestoredFiles(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.DiffFileBytes = 16
	fixture := newGitFixtureWithoutRepository(t, limits)
	fixture.git("init", "--quiet")
	fixture.write("binary", "old\x00binary")
	fixture.write("large.txt", strings.Repeat("x", 32))
	fixture.write("mode.sh", "echo ok\n")
	fixture.write("restored.txt", "tracked\n")
	fixture.commitAll("initial")
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "restored.txt")))

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	fixture.write("binary", "new\x00binary")
	fixture.write("large.txt", strings.Repeat("y", 32))
	//nolint:gosec // Executable permission is the intentional mode-only change.
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "mode.sh"), 0o700))
	fixture.write("restored.txt", "tracked\n")

	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{
		{Path: "binary", Kind: changes.KindModified},
		{Path: "large.txt", Kind: changes.KindModified},
		{Path: "mode.sh", Kind: changes.KindModified},
		{Path: "restored.txt", Kind: changes.KindAdded},
	}, report.Entries())
	assert.NotContains(t, report.Diff(), "binary")
	assert.NotContains(t, report.Diff(), "large.txt")
	assert.Contains(t, report.Diff(), "mode.sh")
}

func TestInspectorKeepsAmbiguousRenamesAsAddsAndDeletes(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("old-a", "same\n")
	fixture.write("old-b", "same\n")
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "old-a")))
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "old-b")))
	fixture.write("new-a", "same\n")
	fixture.write("new-b", "same\n")

	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{
		{Path: "new-a", Kind: changes.KindUntracked},
		{Path: "new-b", Kind: changes.KindUntracked},
		{Path: "old-a", Kind: changes.KindDeleted},
		{Path: "old-b", Kind: changes.KindDeleted},
	}, report.Entries())
}

func TestSnapshotCannotCrossInspectorOwnership(t *testing.T) {
	t.Parallel()

	first := newGitFixture(t)
	first.write("file", "content")
	first.commitAll("initial")

	second := newGitFixture(t)
	second.write("file", "content")
	second.commitAll("initial")

	snapshot, err := first.inspector.Capture(t.Context())
	require.NoError(t, err)
	_, err = second.inspector.Changes(t.Context(), snapshot)
	require.ErrorIs(t, err, ErrSnapshotExpired)

	report, err := first.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Empty(t, report.Entries())
}

func TestDiffPathSanitizerDoesNotRewriteFileContent(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("file.txt", "a/old/keep b/new/keep "+fixture.root+"\n")
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	fixture.write("file.txt", "a/old/changed b/new/changed "+fixture.root+"\n")

	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Contains(t, report.Diff(), "-a/old/keep b/new/keep "+fixture.root)
	assert.Contains(t, report.Diff(), "+a/old/changed b/new/changed "+fixture.root)
	assert.Contains(t, report.Diff(), "diff --git a/file.txt b/file.txt")
}

func TestInspectorRefusesReplacedTempRootWithoutDeletingReplacement(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("file", "before")
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)

	moved := fixture.tempRoot + "-moved"

	t.Cleanup(func() { require.NoError(t, os.RemoveAll(moved)) })
	require.NoError(t, os.Rename(fixture.tempRoot, moved))
	require.NoError(t, os.Mkdir(fixture.tempRoot, 0o700))
	sentinel := filepath.Join(fixture.tempRoot, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))

	_, err = fixture.inspector.Changes(t.Context(), snapshot)
	require.Error(t, err)
	assert.Equal(t, "keep", string(mustReadGitTest(t, sentinel)))

	baselines, err := filepath.Glob(filepath.Join(moved, baselinePrefix+"*"))
	require.NoError(t, err)
	assert.Empty(t, baselines)
}

func mustReadGitTest(t *testing.T, path string) []byte {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // Test reads an exact private sentinel path.
	require.NoError(t, err)

	return content
}
