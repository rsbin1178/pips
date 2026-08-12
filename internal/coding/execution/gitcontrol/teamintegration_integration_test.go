//nolint:gosec,paralleltest,wsl_v5 // Isolated Git fixtures require real modes, subprocesses, and ordered repository mutations.
package gitcontrol_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/teamintegration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerPrepareAndApplyLeavesParentHeadAndIndexUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native symlink transaction is supported on macOS and Linux")
	}
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	gitPath, err = filepath.Abs(gitPath)
	require.NoError(t, err)

	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	require.NoError(t, os.Mkdir(repository, 0o700))
	runIntegrationGit(t, gitPath, repository, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "message.txt"), []byte("base\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "deleted.txt"), []byte("delete\n"), 0o644))
	require.NoError(t, os.Symlink("message.txt", filepath.Join(repository, "link")))
	runIntegrationGit(t, gitPath, repository, "add", "--", "message.txt", "deleted.txt", "link")
	runIntegrationGit(t, gitPath, repository, "-c", "user.name=Pips Test", "-c", "user.email=pips@example.test",
		"commit", "-m", "base")
	baseOID := strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD"))

	require.NoError(t, os.WriteFile(filepath.Join(repository, "message.txt"), []byte("result\n"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(repository, "message.txt"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "added.bin"), []byte{'a', 0, 'b'}, 0o644))
	require.NoError(t, os.Remove(filepath.Join(repository, "deleted.txt")))
	require.NoError(t, os.Remove(filepath.Join(repository, "link")))
	require.NoError(t, os.Symlink("added.bin", filepath.Join(repository, "link")))
	runIntegrationGit(t, gitPath, repository, "add", "--all", "--", "message.txt", "added.bin", "deleted.txt", "link")
	runIntegrationGit(t, gitPath, repository, "-c", "user.name=Pips Test", "-c", "user.email=pips@example.test",
		"commit", "-m", "result")
	resultOID := strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD"))
	resultRef := "refs/pips/team/test/results/attempt-1"
	runIntegrationGit(t, gitPath, repository, "update-ref", resultRef, resultOID)
	runIntegrationGit(t, gitPath, repository, "reset", "--hard", baseOID)

	gitDirectory := strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "--absolute-git-dir"))
	indexBefore, err := os.ReadFile(filepath.Join(gitDirectory, "index"))
	require.NoError(t, err)

	productRoot := filepath.Join(root, "product")
	worktreesRoot := filepath.Join(root, "worktrees")
	integrationsRoot := filepath.Join(productRoot, "teams", "integrations")
	manager, err := teamintegration.New(teamintegration.Options{
		GitPath: gitPath, ProductRoot: productRoot, WorktreesRoot: worktreesRoot,
		IntegrationsRoot: integrationsRoot, Limits: teamintegration.DefaultLimits(),
	})
	require.NoError(t, err)

	preview, err := manager.Prepare(t.Context(), teamintegration.PrepareRequest{
		Workspace: repository,
		Selection: teamintegration.Selection{
			TeamID: "team-1", ResourceRevision: 1, BaseOID: baseOID,
			Artifacts: []teamintegration.Artifact{{
				TaskID: "task-1", AttemptID: "attempt-1", BaseOID: baseOID,
				ResultOID: resultOID, ResultRef: resultRef,
			}},
		},
	})
	require.NoError(t, err)
	resource, err := manager.Resource(preview.ID)
	require.NoError(t, err)
	require.NotEmpty(t, preview.ApprovalToken)
	assert.Equal(t, teamintegration.VerificationNotRun, preview.Verification.Status)
	assert.Len(t, preview.Manifest.Entries, 4)
	assert.Equal(t, baseOID, strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD")))
	assert.Equal(t, "base\n", string(mustReadFile(t, filepath.Join(repository, "message.txt"))))
	_, err = os.Lstat(filepath.Join(repository, "added.bin"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, "delete\n", string(mustReadFile(t, filepath.Join(repository, "deleted.txt"))))
	linkTarget, err := os.Readlink(filepath.Join(repository, "link"))
	require.NoError(t, err)
	assert.Equal(t, "message.txt", linkTarget)
	assert.Equal(t, indexBefore, mustReadFile(t, filepath.Join(gitDirectory, "index")))

	result, err := manager.Apply(t.Context(), preview.ID, preview.ApprovalToken)
	require.NoError(t, err)
	assert.Equal(t, "applied", result.State)
	assert.Equal(t, "result\n", string(mustReadFile(t, filepath.Join(repository, "message.txt"))))
	assert.Equal(t, []byte{'a', 0, 'b'}, mustReadFile(t, filepath.Join(repository, "added.bin")))
	_, err = os.Lstat(filepath.Join(repository, "deleted.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
	linkTarget, err = os.Readlink(filepath.Join(repository, "link"))
	require.NoError(t, err)
	assert.Equal(t, "added.bin", linkTarget)
	messageInfo, err := os.Stat(filepath.Join(repository, "message.txt"))
	require.NoError(t, err)
	assert.NotZero(t, messageInfo.Mode().Perm()&0o111)
	assert.Equal(t, baseOID, strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD")))
	assert.Equal(t, indexBefore, mustReadFile(t, filepath.Join(gitDirectory, "index")))

	_, err = manager.Apply(t.Context(), preview.ID, preview.ApprovalToken)
	require.ErrorIs(t, err, teamintegration.ErrConsumed)

	changed := resource
	changed.BranchRef += "-changed"
	_, err = manager.Cleanup(t.Context(), teamintegration.CleanupRequest{
		TeamID: "team-1", Resource: changed,
	})
	require.ErrorIs(t, err, teamintegration.ErrRetained)
	require.ErrorIs(t, err, teamintegration.ErrStale)
	_, err = os.Stat(resource.Directory.Path())
	require.NoError(t, err)

	dirtyPath := filepath.Join(resource.Directory.Path(), "dirty.tmp")
	require.NoError(t, os.WriteFile(dirtyPath, []byte("retain\n"), 0o600))
	_, err = manager.Cleanup(t.Context(), teamintegration.CleanupRequest{
		TeamID: "team-1", Resource: resource,
	})
	require.ErrorIs(t, err, teamintegration.ErrRetained)
	_, err = os.Stat(resource.Directory.Path())
	require.NoError(t, err)
	require.NoError(t, os.Remove(dirtyPath))

	cleanup, err := manager.Cleanup(t.Context(), teamintegration.CleanupRequest{
		TeamID: "team-1", Resource: resource,
	})
	require.NoError(t, err)
	assert.True(t, cleanup.WorktreeRemoved)
	assert.True(t, cleanup.BranchDeleted)
	assert.True(t, cleanup.IntegrationRefDeleted)
	assert.True(t, cleanup.JournalRemoved)
	_, err = os.Lstat(resource.Directory.Path())
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, resultOID, strings.TrimSpace(runIntegrationGit(
		t, gitPath, repository, "rev-parse", resultRef,
	)))
	assertIntegrationRefMissing(t, gitPath, repository, resource.BranchRef)
	assertIntegrationRefMissing(t, gitPath, repository, resource.IntegrationRef)
}

func TestManagerConsumesApprovalWhenParentIndexChanges(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	gitPath, err = filepath.Abs(gitPath)
	require.NoError(t, err)

	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	require.NoError(t, os.Mkdir(repository, 0o700))
	runIntegrationGit(t, gitPath, repository, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "file.txt"), []byte("base\n"), 0o644))
	runIntegrationGit(t, gitPath, repository, "add", "--", "file.txt")
	runIntegrationGit(t, gitPath, repository, "-c", "user.name=Pips Test", "-c", "user.email=pips@example.test",
		"commit", "-m", "base")
	baseOID := strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "file.txt"), []byte("result\n"), 0o644))
	runIntegrationGit(t, gitPath, repository, "add", "--", "file.txt")
	runIntegrationGit(t, gitPath, repository, "-c", "user.name=Pips Test", "-c", "user.email=pips@example.test",
		"commit", "-m", "result")
	resultOID := strings.TrimSpace(runIntegrationGit(t, gitPath, repository, "rev-parse", "HEAD"))
	resultRef := "refs/pips/team/test/results/attempt-2"
	runIntegrationGit(t, gitPath, repository, "update-ref", resultRef, resultOID)
	runIntegrationGit(t, gitPath, repository, "reset", "--hard", baseOID)

	manager, err := teamintegration.New(teamintegration.Options{
		GitPath: gitPath, ProductRoot: filepath.Join(root, "product"),
		WorktreesRoot:    filepath.Join(root, "worktrees"),
		IntegrationsRoot: filepath.Join(root, "product", "teams", "integrations"),
		Limits:           teamintegration.DefaultLimits(),
	})
	require.NoError(t, err)
	preview, err := manager.Prepare(t.Context(), teamintegration.PrepareRequest{
		Workspace: repository,
		Selection: teamintegration.Selection{
			TeamID: "team-2", ResourceRevision: 2, BaseOID: baseOID,
			Artifacts: []teamintegration.Artifact{{
				TaskID: "task-2", AttemptID: "attempt-2", BaseOID: baseOID,
				ResultOID: resultOID, ResultRef: resultRef,
			}},
		},
	})
	require.NoError(t, err)

	runIntegrationGit(t, gitPath, repository, "update-index", "--chmod=+x", "file.txt")
	_, err = manager.Apply(t.Context(), preview.ID, preview.ApprovalToken)
	require.ErrorIs(t, err, teamintegration.ErrStale)
	assert.Equal(t, "base\n", string(mustReadFile(t, filepath.Join(repository, "file.txt"))))
	_, err = manager.Apply(t.Context(), preview.ID, preview.ApprovalToken)
	assert.ErrorIs(t, err, teamintegration.ErrConsumed)
}

func runIntegrationGit(t *testing.T, gitPath, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), gitPath, append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)

	return string(output)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)

	return value
}

func assertIntegrationRefMissing(t *testing.T, gitPath, directory, ref string) {
	t.Helper()
	command := exec.CommandContext(
		t.Context(), gitPath, "-C", directory, "show-ref", "--verify", "--quiet", ref,
	)
	command.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	err := command.Run()
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
}
