package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectorAttributesOnlyPostCaptureChanges(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.write("delete.txt", "delete me\n")
	fixture.write("old-name.txt", "rename me\n")
	fixture.write(".gitignore", "*.ignored\n")
	fixture.commitAll("initial")

	fixture.write("tracked.txt", "user dirty\n")
	fixture.write("existing-untracked.txt", "existing\n")
	indexBefore := fixture.indexDigest()

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	assert.Equal(t, indexBefore, fixture.indexDigest())
	assert.NotContains(t, string(snapshot.Payload()), fixture.root)
	assert.NotContains(t, string(snapshot.Payload()), fixture.inspector.tempRoot)

	fixture.write("tracked.txt", "agent changed\n")
	fixture.write("existing-untracked.txt", "existing changed\n")
	fixture.write("new-untracked.txt", "new file\n")
	fixture.write("new.ignored", "ignored\n")
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "delete.txt")))
	require.NoError(t, os.Rename(
		filepath.Join(fixture.root, "old-name.txt"),
		filepath.Join(fixture.root, "renamed.txt"),
	))

	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, indexBefore, fixture.indexDigest())
	assert.Equal(t, []changes.Entry{
		{Path: "delete.txt", Kind: changes.KindDeleted},
		{Path: "existing-untracked.txt", Kind: changes.KindModified},
		{Path: "new-untracked.txt", Kind: changes.KindUntracked},
		{Path: "renamed.txt", PreviousPath: "old-name.txt", Kind: changes.KindRenamed},
		{Path: "tracked.txt", Kind: changes.KindModified},
	}, report.Entries())
	assert.False(t, report.Truncated())
	assert.Contains(t, report.Diff(), "tracked.txt")
	assert.Contains(t, report.Diff(), "-user dirty")
	assert.Contains(t, report.Diff(), "+agent changed")
	assert.NotContains(t, report.Diff(), fixture.root)
	assert.NotContains(t, report.Diff(), fixture.inspector.tempRoot)
	assert.NotContains(t, report.Diff(), "new.ignored")
	assert.Empty(t, fixture.baselineDirectories())

	_, err = fixture.inspector.Changes(t.Context(), snapshot)
	require.ErrorIs(t, err, ErrSnapshotExpired)
}

func TestInspectorReportsNoChangeForExistingDirtyWorkspace(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "committed\n")
	fixture.commitAll("initial")
	fixture.write("tracked.txt", "already dirty\n")
	fixture.write("untracked.txt", "already here\n")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Empty(t, report.Entries())
	assert.Empty(t, report.Diff())
}

func TestInspectorTracksSymlinkTargetWithoutFollowingOutside(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	require.NoError(t, os.Symlink("first-target", filepath.Join(fixture.root, "link")))
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "link")))
	require.NoError(t, os.Symlink("second-target", filepath.Join(fixture.root, "link")))

	report, err := fixture.inspector.Changes(t.Context(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, []changes.Entry{{Path: "link", Kind: changes.KindModified}}, report.Entries())
	assert.Contains(t, report.Diff(), "first-target")
	assert.Contains(t, report.Diff(), "second-target")
}

func TestInspectorCloseExpiresAndCleansSnapshots(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "content\n")
	fixture.commitAll("initial")

	snapshot, err := fixture.inspector.Capture(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, fixture.baselineDirectories())
	require.NoError(t, fixture.inspector.Close())
	assert.Empty(t, fixture.baselineDirectories())

	_, err = fixture.inspector.Changes(t.Context(), snapshot)
	require.ErrorIs(t, err, ErrClosed)
	_, err = fixture.inspector.Capture(t.Context())
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, fixture.inspector.Close())
}

func TestInspectorFailsClosedOutsideRepositoryAndOnLimits(t *testing.T) {
	t.Parallel()

	t.Run("not repository", func(t *testing.T) {
		t.Parallel()

		fixture := newGitFixtureWithoutRepository(t, DefaultLimits())
		_, err := fixture.inspector.Capture(t.Context())
		require.ErrorIs(t, err, ErrNotRepository)
	})

	t.Run("hash limit", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.HashBytes = 4
		fixture := newGitFixtureWithoutRepository(t, limits)
		fixture.git("init", "--quiet")
		fixture.write("large.txt", "more than four bytes")
		fixture.commitAll("initial")

		_, err := fixture.inspector.Capture(t.Context())
		require.ErrorIs(t, err, ErrLimit)
		assert.Empty(t, fixture.baselineDirectories())
	})

	t.Run("copy limit", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.CopyBytes = 4
		limits.DiffFileBytes = 4
		fixture := newGitFixtureWithoutRepository(t, limits)
		fixture.git("init", "--quiet")
		fixture.write("first.txt", "1234")
		fixture.write("second.txt", "5678")
		fixture.commitAll("initial")

		_, err := fixture.inspector.Capture(t.Context())
		require.ErrorIs(t, err, ErrLimit)
		assert.Empty(t, fixture.baselineDirectories())
	})

	t.Run("inspection timeout", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.InspectTime = time.Nanosecond
		fixture := newGitFixtureWithoutRepository(t, limits)
		fixture.git("init", "--quiet")

		_, err := fixture.inspector.Capture(t.Context())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestInspectorHonorsCanceledContextAndRejectsForeignSnapshot(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("tracked.txt", "content\n")
	fixture.commitAll("initial")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fixture.inspector.Capture(ctx)
	require.ErrorIs(t, err, context.Canceled)

	foreign, err := changes.NewSnapshot("foreign/v1", []byte("payload"))
	require.NoError(t, err)
	_, err = fixture.inspector.Changes(t.Context(), foreign)
	require.ErrorIs(t, err, ErrSnapshotExpired)
}

type gitFixture struct {
	t         *testing.T
	root      string
	workspace workspace.Workspace
	tree      *workspace.Tree
	policy    execution.Policy
	executor  *execution.Executor
	gitPath   string
	tempRoot  string
	inspector *Inspector
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()

	fixture := newGitFixtureWithoutRepository(t, DefaultLimits())
	fixture.git("init", "--quiet")

	return fixture
}

func newGitFixtureWithoutRepository(t *testing.T, limits Limits) *gitFixture {
	t.Helper()

	return newGitFixtureAtRoot(t, t.TempDir(), limits)
}

func newGitFixtureAtRoot(t *testing.T, root string, limits Limits) *gitFixture {
	t.Helper()

	ws, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:       config.SandboxFullAccess,
		Approval:      config.ApprovalNever,
		SandboxSource: config.Source{Kind: config.SourceUserFile},
	})
	require.NoError(t, err)

	executionTemp := privateTempDir(t)
	executor, err := execution.NewExecutor(ws, execution.ExecutorConfig{
		TempRoot:    executionTemp,
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	gitPath := systemGitPath(t)
	inspectorTemp := privateTempDir(t)
	inspector, err := New(ws, tree, policy, executor, Config{
		GitPath:  gitPath,
		TempRoot: inspectorTemp,
		Limits:   limits,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspector.Close()) })

	return &gitFixture{
		t:         t,
		root:      root,
		workspace: ws,
		tree:      tree,
		policy:    policy,
		executor:  executor,
		gitPath:   gitPath,
		tempRoot:  inspectorTemp,
		inspector: inspector,
	}
}

func (f *gitFixture) write(name, content string) {
	f.t.Helper()

	path := filepath.Join(f.root, filepath.FromSlash(name))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o600))
}

func (f *gitFixture) commitAll(message string) {
	f.t.Helper()

	f.git("add", "--all", "--", ".")
	f.git(
		"-c", "user.name=Pips Test",
		"-c", "user.email=pips@example.invalid",
		"commit", "--quiet", "-m", message,
	)
}

func (f *gitFixture) git(arguments ...string) {
	f.t.Helper()

	operation, err := execution.NewOperation(f.t.Context(), f.workspace, execution.OperationSpec{
		Kind:       execution.KindGit,
		Tool:       "git_test_setup",
		Executable: f.gitPath,
		Args:       slices.Clone(arguments),
		CWD:        ".",
		Timeout:    30 * time.Second,
		Output: execution.OutputLimits{
			CaptureBytes: 1 << 20,
			MaxBytes:     2 << 20,
			ChunkBytes:   4096,
			QueueDepth:   4,
		},
		Workspace: execution.WorkspaceWrite,
		Network:   execution.NetworkNone,
	})
	require.NoError(f.t, err)

	decision := f.policy.Evaluate(operation)
	authorization, ok := decision.Authorization()
	require.True(f.t, ok)

	result, err := f.executor.Execute(f.t.Context(), operation, authorization, nil)
	require.NoError(f.t, err)
	require.Equal(f.t, execution.StatusExited, result.Status)
	require.Zero(f.t, result.ExitCode, string(streamContent(result.Stderr)))
}

func (f *gitFixture) indexDigest() [sha256.Size]byte {
	f.t.Helper()

	content, err := os.ReadFile(filepath.Join(f.root, ".git", "index"))
	require.NoError(f.t, err)

	return sha256.Sum256(content)
}

func (f *gitFixture) baselineDirectories() []string {
	f.t.Helper()

	paths, err := filepath.Glob(filepath.Join(f.tempRoot, baselinePrefix+"*"))
	require.NoError(f.t, err)

	return paths
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	//nolint:gosec // The execution and Inspector contracts require owner traversal.
	require.NoError(t, os.Chmod(root, 0o700))

	return root
}

func systemGitPath(t *testing.T) string {
	t.Helper()

	for _, candidate := range []string{"/usr/bin/git", "/bin/git"} {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}

	t.Skip("system Git is unavailable at a fixed supported path")

	return ""
}

func TestParsePathListRejectsMalformedAndDuplicatePaths(t *testing.T) {
	t.Parallel()

	tests := [][]byte{
		[]byte("unterminated"),
		[]byte("../outside\x00"),
		[]byte("same\x00same\x00"),
		[]byte("bad\\path\x00"),
	}

	for _, input := range tests {
		_, err := parsePathList(input, pathTracked, 10)
		require.Error(t, err)
	}
}

func TestSnapshotTokenValidation(t *testing.T) {
	t.Parallel()

	token, err := newToken()
	require.NoError(t, err)
	assert.True(t, validToken(token))
	assert.False(t, validToken(""))
	assert.False(t, validToken(token[:len(token)-1]))
	assert.False(t, validToken("Z"+token[1:]))
}

func TestInspectorConstructorRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	_, err = New(ws, tree, execution.Policy{}, nil, Config{})
	require.Error(t, err)

	limits := DefaultLimits()
	limits.Files = 0
	_, err = New(ws, tree, execution.Policy{}, &execution.Executor{}, Config{
		GitPath:  systemGitPath(t),
		TempRoot: privateTempDir(t),
		Limits:   limits,
	})
	require.Error(t, err)

	_, err = New(ws, tree, execution.Policy{}, &execution.Executor{}, Config{
		GitPath:  "git",
		TempRoot: privateTempDir(t),
		Limits:   DefaultLimits(),
	})
	require.Error(t, err)

	publicTemp := t.TempDir()
	//nolint:gosec // Deliberately make the fixture unsafe for validation.
	require.NoError(t, os.Chmod(publicTemp, 0o755))
	_, err = New(ws, tree, execution.Policy{}, &execution.Executor{}, Config{
		GitPath:  systemGitPath(t),
		TempRoot: publicTemp,
		Limits:   DefaultLimits(),
	})
	require.Error(t, err)
}

func TestInspectorErrorsAreClassifiable(t *testing.T) {
	t.Parallel()

	for _, target := range []error{ErrNotRepository, ErrSnapshotExpired, ErrLimit, ErrGit, ErrClosed} {
		wrapped := errors.Join(target, assert.AnError)
		require.ErrorIs(t, wrapped, target)
	}
}

func FuzzParsePathList(f *testing.F) {
	f.Add([]byte("file.txt\x00"))
	f.Add([]byte("../outside\x00"))
	f.Add([]byte{})

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parsePathList(data, pathTracked, 100)
	})
}
