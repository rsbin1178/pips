//go:build darwin || linux

package gitcontrol_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamWorktreeCreateCaptureAndCleanupPreservesParent(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	gitPath, err = filepath.EvalSymlinks(gitPath)
	require.NoError(t, err)

	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	product := filepath.Join(root, "product")
	worktrees := filepath.Join(root, "worktrees")
	leases := filepath.Join(product, "teams", "leases")

	require.NoError(t, os.Mkdir(repository, 0o700))
	require.NoError(t, os.Mkdir(product, 0o700))
	runGit(t, gitPath, repository, "init", "-q", "--initial-branch=main")
	runGit(t, gitPath, repository, "config", "user.name", "Pips Test")
	runGit(t, gitPath, repository, "config", "user.email", "pips@example.invalid")

	writeFile(t, repository, "normal.txt", []byte("base\n"), 0o644)
	writeFile(t, repository, "executable.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	writeFile(t, repository, "binary.bin", []byte{0, 1, 2, 3}, 0o644)
	writeFile(t, repository, "remove.txt", []byte("remove me\n"), 0o644)
	writeFile(t, repository, ".gitignore", []byte("ignored.txt\n"), 0o644)
	writeFile(t, repository, ".gitattributes", []byte("*.flt filter=evil\n"), 0o644)
	writeFile(t, repository, "filtered.flt", []byte("raw pointer\n"), 0o644)
	require.NoError(t, os.Symlink("normal.txt", filepath.Join(repository, "link")))
	runGit(t, gitPath, repository, "add", "--all")
	runGit(t, gitPath, repository, "commit", "-q", "-m", "base")
	baseOID := string(bytes.TrimSpace(runGit(t, gitPath, repository, "rev-parse", "HEAD")))

	sentinel := filepath.Join(root, "hook-ran")
	hook := filepath.Join(repository, ".git", "hooks", "post-checkout")
	require.NoError(t, os.WriteFile( //nolint:gosec // Executable fixture proves checkout hooks are never invoked.
		hook, fmt.Appendf(nil, "#!/bin/sh\nprintf invoked > %q\n", sentinel), 0o755,
	))
	parentBefore := snapshotParent(t, gitPath, repository)

	manager, err := teamworktree.New(teamworktree.Options{
		GitPath: gitPath, ProductRoot: product, WorktreesRoot: worktrees,
		LeasesRoot: leases, Limits: teamworktree.DefaultLimits(),
	})
	require.NoError(t, err)
	lease, err := manager.Acquire(t.Context(), team.ID("team-integration"), 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	first, err := manager.Create(t.Context(), lease, teamworktree.CreateRequest{
		Owner: teamworktree.Owner{
			TeamID: "team-integration", MemberID: "member-a",
			AttemptID: "attempt-a", LeaseGeneration: 1,
		},
		Workspace: repository, BaseOID: baseOID,
	})
	require.NoError(t, err)
	second, err := manager.Create(t.Context(), lease, teamworktree.CreateRequest{
		Owner: teamworktree.Owner{
			TeamID: "team-integration", MemberID: "member-b",
			AttemptID: "attempt-b", LeaseGeneration: 1,
		},
		Workspace: repository, BaseOID: baseOID,
	})
	require.NoError(t, err)
	assertParentEqual(t, parentBefore, snapshotParent(t, gitPath, repository))

	_, err = os.Stat(sentinel)
	require.ErrorIs(t, err, fs.ErrNotExist)

	writeFile(t, first.Directory.Path, "normal.txt", []byte("changed\n"), 0o755)
	writeFile(t, first.Directory.Path, "binary.bin", []byte{0, 4, 255, 7}, 0o644)
	require.NoError(t, os.Remove(filepath.Join(first.Directory.Path, "remove.txt")))
	writeFile(t, first.Directory.Path, "new.txt", []byte("new\n"), 0o644)
	writeFile(t, first.Directory.Path, "ignored.txt", []byte("ignored\n"), 0o644)
	require.NoError(t, os.Remove(filepath.Join(first.Directory.Path, "link")))
	require.NoError(t, os.Symlink("new.txt", filepath.Join(first.Directory.Path, "link")))

	captured, err := manager.Capture(t.Context(), lease, first, teamworktree.CaptureRequest{
		Message: "integration capture", Time: time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, baseOID, string(bytes.TrimSpace(runGit(
		t, gitPath, repository, "rev-parse", captured.CommitOID+"^",
	))))
	assert.Equal(t, "changed\n", string(runGit(
		t, gitPath, repository, "show", captured.CommitOID+":normal.txt",
	)))
	assert.Equal(t, []byte{0, 4, 255, 7}, runGit(
		t, gitPath, repository, "show", captured.CommitOID+":binary.bin",
	))
	assert.Equal(t, "new.txt", string(runGit(
		t, gitPath, repository, "show", captured.CommitOID+":link",
	)))
	assert.Contains(t, string(runGit(
		t, gitPath, repository, "ls-tree", captured.CommitOID, "normal.txt",
	)), "100755 blob")
	assertGitObjectMissing(t, gitPath, repository, captured.CommitOID+":remove.txt")
	assertGitObjectMissing(t, gitPath, repository, captured.CommitOID+":ignored.txt")
	assert.Equal(t, "new\n", string(runGit(
		t, gitPath, repository, "show", captured.CommitOID+":new.txt",
	)))

	_, err = os.Stat(sentinel)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assertParentEqual(t, parentBefore, snapshotParent(t, gitPath, repository))

	unchangedCleanup, err := manager.Cleanup(t.Context(), lease, second)
	require.NoError(t, err)
	assert.True(t, unchangedCleanup.WorktreeRemoved)
	assert.True(t, unchangedCleanup.BranchDeleted)
	capturedCleanup, err := manager.Cleanup(t.Context(), lease, captured.Resource)
	require.NoError(t, err)
	assert.True(t, capturedCleanup.WorktreeRemoved)
	assert.True(t, capturedCleanup.BranchDeleted)
	assert.True(t, capturedCleanup.ResultRetained)
	assert.Equal(t, "changed\n", string(runGit(
		t, gitPath, repository, "show", captured.Resource.ResultRef+":normal.txt",
	)))
	assertParentEqual(t, parentBefore, snapshotParent(t, gitPath, repository))
}

type parentState struct {
	head, branch string
	status       []byte
	index        [sha256.Size]byte
}

func snapshotParent(t *testing.T, gitPath, repository string) parentState {
	t.Helper()

	index, err := os.ReadFile(filepath.Join(repository, ".git", "index")) //nolint:gosec // Test reads an exact private fixture index.
	require.NoError(t, err)

	return parentState{
		head:   string(bytes.TrimSpace(runGit(t, gitPath, repository, "rev-parse", "HEAD"))),
		branch: string(bytes.TrimSpace(runGit(t, gitPath, repository, "symbolic-ref", "HEAD"))),
		status: runGit(t, gitPath, repository, "status", "--porcelain=v1", "-z"),
		index:  sha256.Sum256(index),
	}
}

func assertParentEqual(t *testing.T, expected, actual parentState) {
	t.Helper()
	assert.Equal(t, expected, actual)
}

func writeFile(t *testing.T, root, name string, content []byte, mode fs.FileMode) {
	t.Helper()

	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, content, mode))
	require.NoError(t, os.Chmod(path, mode))
}

func runGit(t *testing.T, gitPath, directory string, arguments ...string) []byte {
	t.Helper()

	command := exec.CommandContext( //nolint:gosec // Test helper receives only fixed fixture commands.
		t.Context(), gitPath, append([]string{"-C", directory}, arguments...)...,
	)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat",
	)

	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %v: %v: %s", arguments, err, exit.Stderr)
		}

		require.NoError(t, err)
	}

	return output
}

func assertGitObjectMissing(t *testing.T, gitPath, directory, object string) {
	t.Helper()

	command := exec.CommandContext( //nolint:gosec // Test validates a fixed fixture object expression.
		t.Context(), gitPath, "-C", directory, "cat-file", "-e", object,
	)

	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
	)
	err := command.Run()

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
}
