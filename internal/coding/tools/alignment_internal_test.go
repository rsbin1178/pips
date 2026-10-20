//nolint:wsl_v5 // Fault injection stays beside the rollback assertion.
package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditAndWriteRejectInvalidUTF8(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)

	_, err := service.writeFile(t.Context(), writeArgs{
		FilePath: "bad.txt",
		Content:  string([]byte{0xff, 'x'}),
	})
	require.Error(t, err)
	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "binary_file", header.Code)
	_, statErr := os.Lstat(filepath.Join(root, "bad.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "binary.txt"), []byte{'a', 0, 'b'}, 0o600,
	))
	_, err = service.edit(t.Context(), editArgs{
		FilePath: "binary.txt", OldString: "a", NewString: "b",
	})
	require.Error(t, err)
	header, _, parseErr = ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "binary_file", header.Code)
}

func TestEditAndWritePlanGate(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	service.plan = &PlanFile{
		Path:     func() string { return "/session/plan.md" },
		Admitted: func() bool { return true },
		Read:     func(context.Context) (string, error) { return "", nil },
		Write:    func(context.Context, string) error { return nil },
	}

	_, err := service.writeFile(t.Context(), writeArgs{FilePath: "a.txt", Content: "x"})
	require.Error(t, err)
	header, body, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
	assert.Contains(t, body, "file edits are not allowed in plan mode")
	_, statErr := os.Lstat(filepath.Join(root, "a.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("old\n"), 0o600))
	_, err = service.edit(t.Context(), editArgs{
		FilePath: "a.txt", OldString: "old", NewString: "new",
	})
	require.Error(t, err)
	header, body, parseErr = ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "invalid_argument", header.Code)
	assert.Contains(t, body, "file edits are not allowed in plan mode")
	assert.Equal(t, "old\n", string(readMutationTestFile(t, root, "a.txt")))
}

func TestWriteTransactionRollsBack(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	service.mutationFault = func(phase, operation, _ string) error {
		if phase == "commit" && operation == "link" {
			return errors.New("injected commit failure")
		}

		return nil
	}

	_, err := service.writeFile(t.Context(), writeArgs{
		FilePath: "nested/new.txt", Content: "new\n",
	})
	require.ErrorContains(t, err, "injected commit failure")
	_, statErr := os.Lstat(filepath.Join(root, "nested/new.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Lstat(filepath.Join(root, "nested"))
	require.True(t, os.IsNotExist(statErr), "transaction-created parent should be removed")
	assert.Empty(t, mutationTemporaryFiles(t, root))

	service, root = newMutationService(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("old\n"), 0o600))
	service.mutationFault = func(phase, operation, _ string) error {
		if phase == "commit" && operation == "rename" {
			return errors.New("injected rename failure")
		}

		return nil
	}

	_, err = service.writeFile(t.Context(), writeArgs{
		FilePath: "existing.txt", Content: "new\n",
	})
	require.ErrorContains(t, err, "injected rename failure")
	assert.Equal(t, "old\n", string(readMutationTestFile(t, root, "existing.txt")))
	assert.Empty(t, mutationTemporaryFiles(t, root))
}

func TestEditTransactionRollsBack(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("one two\n"), 0o600))
	service.mutationFault = func(phase, operation, _ string) error {
		if phase == "commit" && operation == "rename" {
			return errors.New("injected rename failure")
		}

		return nil
	}

	_, err := service.edit(t.Context(), editArgs{
		FilePath: "a.txt", OldString: "two", NewString: "three",
	})
	require.ErrorContains(t, err, "injected rename failure")
	assert.Equal(t, "one two\n", string(readMutationTestFile(t, root, "a.txt")))
	assert.Empty(t, mutationTemporaryFiles(t, root))
}

func TestEditDetectsChangeAfterValidation(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("old-a\n"), 0o600))

	changed := false
	service.mutationFault = func(phase, operation, _ string) error {
		if !changed && phase == "prepare" && operation == "stage" {
			changed = true
			return os.WriteFile(filepath.Join(root, "a.txt"), []byte("user-change\n"), 0o600)
		}

		return nil
	}

	_, err := service.edit(t.Context(), editArgs{
		FilePath: "a.txt", OldString: "old-a", NewString: "new-a",
	})
	require.Error(t, err)
	assert.Equal(t, "user-change\n", string(readMutationTestFile(t, root, "a.txt")))
	assert.Empty(t, mutationTemporaryFiles(t, root))

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "conflict", header.Code)
}

func TestCommitPatchRollsBackAllTargets(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("old-a\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o600))

	changes := []*plannedChange{
		plannedUpdate(t, root, "a.txt", "new-a\n"),
		plannedUpdate(t, root, "b.txt", "new-b\n"),
	}

	service.mutationFault = func(phase, operation, name string) error {
		if phase == "commit" && operation == "rename" && name == "b.txt" {
			return errors.New("injected commit failure")
		}

		return nil
	}

	err := service.commitPatch(t.Context(), changes)
	require.Error(t, err)
	assert.Equal(t, "old-a\n", string(readMutationTestFile(t, root, "a.txt")))
	assert.Equal(t, "old-b\n", string(readMutationTestFile(t, root, "b.txt")))
	assert.Empty(t, mutationTemporaryFiles(t, root))

	header, _, parseErr := ParseResult(failure(EditName, err).Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "io_error", header.Code)
}

func TestCommitPatchRollbackFailureRetainsRecoveryMaterial(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("old-a\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o600))

	changes := []*plannedChange{
		plannedUpdate(t, root, "a.txt", "new-a\n"),
		plannedUpdate(t, root, "b.txt", "new-b\n"),
	}

	service.mutationFault = func(phase, operation, name string) error {
		switch {
		case phase == "commit" && operation == "rename" && name == "b.txt":
			return errors.New("injected commit failure")
		case phase == "rollback" && operation == "rename" && name == "a.txt":
			return errors.New("injected rollback failure")
		default:
			return nil
		}
	}

	err := service.commitPatch(t.Context(), changes)
	require.Error(t, err)

	var recovery *RecoveryError
	require.ErrorAs(t, err, &recovery)
	assert.False(t, recovery.Applied)
	assert.Contains(t, recovery.RecoveryPaths, "a.txt")
	require.Len(t, mutationTemporaryFiles(t, root), 1)
	assert.Equal(t, "new-a\n", string(readMutationTestFile(t, root, "a.txt")))
	assert.Equal(t, "old-b\n", string(readMutationTestFile(t, root, "b.txt")))
}

func TestWriteCommitFailureRemovesCreatedParents(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	service.mutationFault = func(phase, operation, name string) error {
		if phase == "commit" && operation == "link" && name == "created/deep/a.txt" {
			return errors.New("injected commit failure")
		}

		return nil
	}

	_, err := service.writeFile(t.Context(), writeArgs{
		FilePath: "created/deep/a.txt", Content: "nested\n",
	})
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(root, "created"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(root, "created/deep/a.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCommitPatchRollbackRefusesReplacedCreatedDirectory(t *testing.T) {
	t.Parallel()

	service, root := newMutationService(t)
	change, err := service.planWriteChange(t.Context(), "created/deep/a.txt", []byte("nested\n"))
	require.NoError(t, err)

	replaced := false
	service.mutationFault = func(phase, operation, name string) error {
		switch {
		case phase == "commit" && operation == "link" && name == "created/deep/a.txt":
			return errors.New("injected commit failure")
		case !replaced && phase == "rollback_cleanup" && operation == "rmdir" && name == "created/deep":
			replaced = true
			original := filepath.Join(root, "created", "deep")
			held := filepath.Join(root, "held-deep")
			if err := os.Rename(original, held); err != nil {
				return err
			}
			if err := os.Mkdir(original, 0o700); err != nil {
				return err
			}

			return os.WriteFile(filepath.Join(original, "marker"), []byte("replacement"), 0o600)
		default:
			return nil
		}
	}

	err = service.commitPatch(t.Context(), []*plannedChange{change})
	require.Error(t, err)
	var recovery *RecoveryError
	require.ErrorAs(t, err, &recovery)
	assert.Contains(t, recovery.RecoveryPaths, "created/deep")
	assert.Contains(t, recovery.RecoveryPaths, "created")
	assert.Equal(t,
		"replacement",
		string(readMutationTestFile(t, root, filepath.Join("created", "deep", "marker"))),
	)
}

func plannedUpdate(t *testing.T, root, name, after string) *plannedChange {
	t.Helper()

	info, err := os.Stat(filepath.Join(root, name))
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // Fixed temporary path.
	require.NoError(t, err)

	return &plannedChange{
		kind:     changeUpdate,
		path:     name,
		parent:   pathDir(name),
		base:     pathBase(name),
		before:   before,
		after:    []byte(after),
		expected: info,
		mode:     info.Mode().Perm(),
		hash:     sha256.Sum256(before),
	}
}

func newMutationService(t *testing.T) (*service, string) {
	t.Helper()

	root := t.TempDir()
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return &service{tree: tree, limits: DefaultLimits()}, root
}

func readMutationTestFile(t *testing.T, root, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // Tests use a fixed temporary root.
	require.NoError(t, err)

	return data
}

func mutationTemporaryFiles(t *testing.T, root string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, ".pips-*"))
	require.NoError(t, err)

	for index := range matches {
		matches[index] = filepath.Base(matches[index])
	}

	return matches
}
