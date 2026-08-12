//go:build darwin || linux

package gitcontrol_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamWorktreeDirtyCleanupRetainsExactResource(t *testing.T) {
	t.Parallel()

	fixture := newSimpleFixture(t)
	resource := fixture.create(t)
	writeFile(t, resource.Directory.Path, "tracked.txt", []byte("changed\n"), 0o644)

	_, err := fixture.manager.Cleanup(t.Context(), fixture.lease, resource)
	require.ErrorIs(t, err, teamworktree.ErrRetained)
	require.ErrorIs(t, err, teamworktree.ErrDirty)

	info, statErr := os.Stat(resource.Directory.Path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
	assert.Equal(t, resource.BaseOID, strings.TrimSpace(string(runGit(
		t, fixture.gitPath, fixture.repository, "rev-parse", "--verify", resource.BranchRef,
	))))
}

func TestTeamWorktreeIdentityChangesFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("branch ref", func(t *testing.T) {
		t.Parallel()

		fixture := newSimpleFixture(t)
		resource := fixture.create(t)
		runGit(t, fixture.gitPath, fixture.repository, "commit", "-q", "--allow-empty", "-m", "other")
		otherOID := strings.TrimSpace(string(runGit(
			t, fixture.gitPath, fixture.repository, "rev-parse", "HEAD",
		)))
		runGit(t, fixture.gitPath, fixture.repository, "update-ref", resource.BranchRef, otherOID)

		_, err := fixture.manager.Inspect(t.Context(), fixture.lease, resource)
		require.ErrorIs(t, err, teamworktree.ErrIdentity)
	})

	t.Run("worktree lock", func(t *testing.T) {
		t.Parallel()

		fixture := newSimpleFixture(t)
		resource := fixture.create(t)
		runGit(
			t, fixture.gitPath, fixture.repository,
			"worktree", "unlock", "--", resource.Directory.Path,
		)

		_, err := fixture.manager.Inspect(t.Context(), fixture.lease, resource)
		require.ErrorIs(t, err, teamworktree.ErrIdentity)
	})

	t.Run("worktree path", func(t *testing.T) {
		t.Parallel()

		fixture := newSimpleFixture(t)
		resource := fixture.create(t)
		moved := filepath.Join(fixture.root, "moved-worktree")
		require.NoError(t, os.Rename(resource.Directory.Path, moved))
		require.NoError(t, os.Mkdir(resource.Directory.Path, 0o700))

		_, err := fixture.manager.Inspect(t.Context(), fixture.lease, resource)
		require.ErrorIs(t, err, teamworktree.ErrIdentity)
	})
}

func TestTeamWorktreeTakeoverRequiresHigherLeaseGeneration(t *testing.T) {
	t.Parallel()

	fixture := newSimpleFixture(t)
	resource := fixture.create(t)
	require.NoError(t, fixture.lease.Close())

	lease, err := fixture.manager.Acquire(t.Context(), resource.Owner.TeamID, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })
	updated, err := fixture.manager.Takeover(t.Context(), lease, resource)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), updated.Owner.LeaseGeneration)
	_, err = fixture.manager.Inspect(t.Context(), lease, updated)
	require.NoError(t, err)

	_, err = fixture.manager.Takeover(t.Context(), lease, updated)
	require.ErrorIs(t, err, teamworktree.ErrLeaseLost)
	_, err = fixture.manager.Inspect(t.Context(), lease, resource)
	require.ErrorIs(t, err, teamworktree.ErrLeaseLost)

	_, err = fixture.manager.Cleanup(t.Context(), lease, updated)
	require.NoError(t, err)
}

func TestTeamWorktreeRejectsUnmergedIndex(t *testing.T) {
	t.Parallel()

	fixture := newSimpleFixture(t)
	resource := fixture.create(t)
	blobOID := strings.TrimSpace(string(runGit(
		t, fixture.gitPath, fixture.repository, "rev-parse", resource.BaseOID+":tracked.txt",
	)))

	var entries strings.Builder
	for stage := 1; stage <= 3; stage++ {
		entries.WriteString("100644 ")
		entries.WriteString(blobOID)
		entries.WriteString(" ")
		entries.WriteString(string(rune('0' + stage)))
		entries.WriteString("\ttracked.txt\n")
	}

	runGitInput(
		t, fixture.gitPath, resource.Directory.Path, []byte(entries.String()),
		"update-index", "--index-info",
	)

	_, err := fixture.manager.Inspect(t.Context(), fixture.lease, resource)
	require.ErrorIs(t, err, teamworktree.ErrUnsafeRepository)
}

func TestTeamWorktreeRejectsGitlinkBaseAndRetainsEvidence(t *testing.T) {
	t.Parallel()

	fixture := newSimpleFixture(t)
	runGit(
		t, fixture.gitPath, fixture.repository,
		"update-index", "--add", "--cacheinfo", "160000", fixture.request.BaseOID, "vendor/module",
	)
	runGit(t, fixture.gitPath, fixture.repository, "commit", "-q", "-m", "gitlink")
	fixture.request.BaseOID = strings.TrimSpace(string(runGit(
		t, fixture.gitPath, fixture.repository, "rev-parse", "HEAD",
	)))

	resource, err := fixture.manager.Create(t.Context(), fixture.lease, fixture.request)
	require.ErrorIs(t, err, teamworktree.ErrRetained)
	require.ErrorIs(t, err, teamworktree.ErrUnsafeRepository)
	assert.NotEmpty(t, resource.ID)
	assert.NotZero(t, resource.Directory.Device)
	assert.NotZero(t, resource.GitDir.Inode)
}

func TestTeamWorktreeLimitFailureRetainsBoundIdentity(t *testing.T) {
	t.Parallel()

	limits := teamworktree.DefaultLimits()
	limits.FileBytes = 4
	fixture := newSimpleFixtureWithLimits(t, limits)

	resource, err := fixture.manager.Create(t.Context(), fixture.lease, fixture.request)
	require.ErrorIs(t, err, teamworktree.ErrRetained)
	require.ErrorIs(t, err, teamworktree.ErrLimit)
	assert.NotEmpty(t, resource.ID)
	assert.NotZero(t, resource.Directory.Device)
	assert.NotZero(t, resource.GitDir.Inode)
	assert.Equal(t, resource.BaseOID, strings.TrimSpace(string(runGit(
		t, fixture.gitPath, fixture.repository, "rev-parse", "--verify", resource.BranchRef,
	))))
}

func TestTeamWorktreeRejectsIncludedExecutableConfigWithoutRunningIt(t *testing.T) {
	t.Parallel()

	fixture := newSimpleFixture(t)
	sentinel := filepath.Join(fixture.root, "filter-ran")
	executable := filepath.Join(fixture.root, "evil-filter")
	require.NoError(t, os.WriteFile( //nolint:gosec // Executable fixture proves repository filters are never invoked.
		executable, []byte("#!/bin/sh\nprintf invoked > \"$1\"\ncat\n"), 0o700,
	))

	include := filepath.Join(fixture.repository, ".git", "unsafe.inc")
	require.NoError(t, os.WriteFile(include, []byte(
		"[filter \"evil\"]\n\tclean = "+executable+" "+sentinel+"\n",
	), 0o600))
	runGit(t, fixture.gitPath, fixture.repository, "config", "--local", "include.path", "unsafe.inc")

	resource, err := fixture.manager.Create(t.Context(), fixture.lease, fixture.request)
	require.ErrorIs(t, err, teamworktree.ErrUnsafeRepository)
	assert.Equal(t, teamworktree.Resource{}, resource)

	_, statErr := os.Stat(sentinel)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	refs := runGit(
		t, fixture.gitPath, fixture.repository,
		"for-each-ref", "--format=%(refname)", "refs/heads/pips/team",
	)
	assert.Empty(t, refs)
	worktreeList := runGit(t, fixture.gitPath, fixture.repository, "worktree", "list", "--porcelain")
	assert.Equal(t, 1, bytes.Count(worktreeList, []byte("worktree ")))
}

type simpleFixture struct {
	root       string
	gitPath    string
	repository string
	manager    *teamworktree.Manager
	lease      *teamworktree.Lease
	request    teamworktree.CreateRequest
}

func newSimpleFixture(t *testing.T) simpleFixture {
	t.Helper()

	return newSimpleFixtureWithLimits(t, teamworktree.DefaultLimits())
}

func newSimpleFixtureWithLimits(t *testing.T, limits teamworktree.Limits) simpleFixture {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	gitPath, err = filepath.EvalSymlinks(gitPath)
	require.NoError(t, err)

	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	product := filepath.Join(root, "product")

	require.NoError(t, os.Mkdir(repository, 0o700))
	require.NoError(t, os.Mkdir(product, 0o700))
	runGit(t, gitPath, repository, "init", "-q", "--initial-branch=main")
	runGit(t, gitPath, repository, "config", "user.name", "Pips Test")
	runGit(t, gitPath, repository, "config", "user.email", "pips@example.invalid")
	writeFile(t, repository, "tracked.txt", []byte("base\n"), 0o644)
	runGit(t, gitPath, repository, "add", "--all")
	runGit(t, gitPath, repository, "commit", "-q", "-m", "base")
	baseOID := strings.TrimSpace(string(runGit(t, gitPath, repository, "rev-parse", "HEAD")))

	manager, err := teamworktree.New(teamworktree.Options{
		GitPath: gitPath, ProductRoot: product,
		WorktreesRoot: filepath.Join(root, "worktrees"),
		LeasesRoot:    filepath.Join(product, "teams", "leases"),
		Limits:        limits,
	})
	require.NoError(t, err)
	lease, err := manager.Acquire(t.Context(), team.ID("team-failure"), 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	return simpleFixture{
		root: root, gitPath: gitPath, repository: repository, manager: manager, lease: lease,
		request: teamworktree.CreateRequest{
			Owner: teamworktree.Owner{
				TeamID: "team-failure", MemberID: "member-a",
				AttemptID: "attempt-a", LeaseGeneration: 1,
			},
			Workspace: repository, BaseOID: baseOID,
		},
	}
}

func runGitInput(
	t *testing.T,
	gitPath, directory string,
	input []byte,
	arguments ...string,
) []byte {
	t.Helper()

	command := exec.CommandContext( //nolint:gosec // Test helper receives only fixed fixture commands.
		t.Context(), gitPath, append([]string{"-C", directory}, arguments...)...,
	)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat",
	)
	command.Stdin = bytes.NewReader(input)

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

func (f simpleFixture) create(t *testing.T) teamworktree.Resource {
	t.Helper()
	resource, err := f.manager.Create(t.Context(), f.lease, f.request)
	require.NoError(t, err)

	return resource
}
