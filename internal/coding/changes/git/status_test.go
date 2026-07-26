package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePorcelainV2PreservesColumnsRenamesAndBranch(t *testing.T) {
	t.Parallel()

	content := []byte(
		"# branch.oid abcdef\x00" +
			"# branch.head main\x00" +
			"# branch.upstream origin/main\x00" +
			"# branch.ab +2 -1\x00" +
			"1 M. N... 100644 100644 100644 aaa bbb staged.go\x00" +
			"1 .M N... 100644 100644 100644 aaa bbb worktree.go\x00" +
			"2 R. N... 100644 100644 100644 aaa bbb R100 new name.go\x00old name.go\x00" +
			"? notes.txt\x00" +
			"? .pips/config.toml\x00",
	)

	parsed, err := parsePorcelainV2(content, 16)
	require.NoError(t, err)
	assert.Equal(t, changes.Branch{
		Head: "main", OID: "abcdef", Upstream: "origin/main", Ahead: 2, Behind: 1,
	}, parsed.branch)
	assert.Equal(t, 1, parsed.protectedOmitted)
	assert.Equal(t, []changes.StatusEntry{
		{Path: "staged.go", Index: changes.PathModified, Submodule: "N..."},
		{Path: "worktree.go", Worktree: changes.PathModified, Submodule: "N..."},
		{
			Path: "new name.go", PreviousPath: "old name.go",
			Index: changes.PathRenamed, Submodule: "N...",
		},
		{Path: "notes.txt", Worktree: changes.PathUntracked},
	}, parsed.entries)
}

func TestParsePorcelainV2FailsClosedOnUnknownRecordsAndLimits(t *testing.T) {
	t.Parallel()

	_, err := parsePorcelainV2([]byte("x unsupported\x00"), 8)
	require.ErrorIs(t, err, ErrGit)
	_, err = parsePorcelainV2([]byte("? one\x00? two\x00"), 1)
	require.ErrorIs(t, err, ErrLimit)
	_, err = parsePorcelainV2([]byte("? unterminated"), 8)
	require.ErrorIs(t, err, ErrGit)
}

func TestInspectorStatusReportsCurrentIndexWorktreeAndUntrackedState(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("both.txt", "base\n")
	fixture.write("rename-old.txt", "rename\n")
	fixture.commitAll("initial")

	fixture.write("both.txt", "staged\n")
	fixture.git("add", "--", "both.txt")
	fixture.write("both.txt", "worktree\n")
	fixture.git("mv", "--", "rename-old.txt", "rename-new.txt")
	fixture.write("notes.txt", "first\n\nthird\n")
	fixture.write("binary.dat", "binary\x00data")
	fixture.write(".pips/config.toml", "secret = true\n")
	indexBefore := fixture.indexDigest()

	status, err := fixture.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Repository())
	assert.Equal(t, indexBefore, fixture.indexDigest())
	assert.NotEmpty(t, status.Branch().Head)
	assert.Equal(t, 1, status.ProtectedOmitted())

	entries := status.Entries()
	assert.Contains(t, entries, changes.StatusEntry{
		Path: "both.txt", Index: changes.PathModified,
		Worktree: changes.PathModified, Submodule: "N...",
	})
	assert.Contains(t, entries, changes.StatusEntry{
		Path: "rename-new.txt", PreviousPath: "rename-old.txt",
		Index: changes.PathRenamed, Submodule: "N...",
	})
	assert.Contains(t, entries, changes.StatusEntry{
		Path: "notes.txt", Worktree: changes.PathUntracked,
	})
	assert.NotContains(t, status.Staged().Diff, ".pips")
	assert.NotContains(t, status.Unstaged().Diff, ".pips")
	assert.NotContains(t, status.Untracked().Diff, ".pips")
	assert.Contains(t, status.Staged().Diff, "+staged")
	assert.Contains(t, status.Unstaged().Diff, "+worktree")
	assert.Contains(t, status.Untracked().Diff, "+first\n+\n+third")
	assert.Equal(t, 1, status.Untracked().Summary.Binary)
	assert.GreaterOrEqual(t, status.Staged().Summary.Files, 2)
	assert.Equal(t, 1, status.Unstaged().Summary.Files)
}

func TestInspectorStatusHandlesNonRepositoryDetachedAndUnborn(t *testing.T) {
	t.Parallel()

	nonRepository := newGitFixtureWithoutRepository(t, DefaultLimits())
	status, err := nonRepository.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.False(t, status.Repository())

	unborn := newGitFixture(t)
	status, err = unborn.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Branch().Unborn)

	detached := newGitFixture(t)
	detached.write("file", "content")
	detached.commitAll("initial")
	detached.git("checkout", "--quiet", "--detach", "HEAD")
	status, err = detached.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Branch().Detached)
}

func TestInspectorStatusReportsDeletedAndConflictedPaths(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("conflict.txt", "base\n")
	fixture.write("deleted.txt", "delete me\n")
	fixture.commitAll("initial")

	fixture.git("checkout", "--quiet", "-b", "side")
	fixture.write("conflict.txt", "side\n")
	fixture.commitAll("side")
	fixture.git("checkout", "--quiet", "main")
	fixture.write("conflict.txt", "main\n")
	fixture.commitAll("main")
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "deleted.txt")))
	fixture.gitExpectExit(1, "merge", "--no-edit", "side")

	status, err := fixture.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.Contains(t, status.Entries(), changes.StatusEntry{
		Path: "deleted.txt", Worktree: changes.PathDeleted, Submodule: "N...",
	})
	assert.Contains(t, status.Entries(), changes.StatusEntry{
		Path: "conflict.txt", Index: changes.PathUnmerged,
		Worktree: changes.PathUnmerged, Conflict: true, Submodule: "N...",
	})
}

func TestInspectorStatusOmitsOversizeAndSymlinkUntrackedContent(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.DiffFileBytes = 4
	fixture := newGitFixtureWithoutRepository(t, limits)
	fixture.git("init", "--quiet")
	fixture.write("large.txt", "larger than four")
	require.NoError(t, os.Symlink("large.txt", filepath.Join(fixture.root, "link")))

	status, err := fixture.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, status.Untracked().Summary.Omitted)
	assert.Empty(t, status.Untracked().Diff)
}

func TestInspectorStatusNeverExposesTrackedProductMetadata(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write("visible.txt", "base\n")
	fixture.write(".pips/settings.toml", "token = \"initial-secret\"\n")
	fixture.git("add", "-f", "--", ".pips/settings.toml", "visible.txt")
	fixture.git("commit", "--quiet", "-m", "initial")
	fixture.write(".pips/settings.toml", "token = \"changed-secret\"\n")
	fixture.write("visible.txt", "changed\n")

	status, err := fixture.inspector.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.ProtectedOmitted())
	content := status.Staged().Diff + status.Unstaged().Diff + status.Untracked().Diff
	assert.NotContains(t, content, ".pips")
	assert.NotContains(t, content, "changed-secret")
	assert.Contains(t, content, "+changed")
}

func FuzzParsePorcelainV2(f *testing.F) {
	f.Add([]byte("? file.txt\x00"), 8)
	f.Add([]byte("# branch.head main\x00"), 8)
	f.Fuzz(func(_ *testing.T, content []byte, maximum int) {
		if maximum < 1 || maximum > 128 {
			maximum = 16
		}

		_, _ = parsePorcelainV2(content, maximum)
	})
}
