package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twoFilePatch = `*** Begin Patch
*** Update File: a.txt
@@
-old-a
+new-a
*** Update File: b.txt
@@
-old-b
+new-b
*** End Patch
`

const nestedAddPatch = `*** Begin Patch
*** Add File: created/deep/a.txt
+nested
*** Add File: z.txt
+last
*** End Patch
`

func TestPatchCommitFailureRollsBackAllTargets(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	writePatchFile(t, root, "a.txt", "old-a\n")
	writePatchFile(t, root, "b.txt", "old-b\n")

	service.patchFault = func(phase, operation, name string) error {
		if phase == "commit" && operation == "rename" && name == "b.txt" {
			return errors.New("injected commit failure")
		}

		return nil
	}

	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: twoFilePatch})
	require.Error(t, err)
	assert.Equal(t, "old-a\n", string(readPatchFile(t, root, "a.txt")))
	assert.Equal(t, "old-b\n", string(readPatchFile(t, root, "b.txt")))
	assert.Empty(t, patchTemporaryFiles(t, root))

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "io_error", header.Code)
}

func TestPatchDetectsChangeAfterValidation(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	writePatchFile(t, root, "a.txt", "old-a\n")

	changed := false
	service.patchFault = func(phase, operation, _ string) error {
		if !changed && phase == "prepare" && operation == "stage" {
			changed = true
			return os.WriteFile(filepath.Join(root, "a.txt"), []byte("user-change\n"), 0o600)
		}

		return nil
	}

	patch := strings.Replace(twoFilePatch, "*** Update File: b.txt\n@@\n-old-b\n+new-b\n", "", 1)
	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: patch})
	require.Error(t, err)
	assert.Equal(t, "user-change\n", string(readPatchFile(t, root, "a.txt")))
	assert.Empty(t, patchTemporaryFiles(t, root))

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "conflict", header.Code)
}

func TestPatchRollbackFailureRetainsRecoveryMaterial(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	writePatchFile(t, root, "a.txt", "old-a\n")
	writePatchFile(t, root, "b.txt", "old-b\n")

	service.patchFault = func(phase, operation, name string) error {
		switch {
		case phase == "commit" && operation == "rename" && name == "b.txt":
			return errors.New("injected commit failure")
		case phase == "rollback" && operation == "rename" && name == "a.txt":
			return errors.New("injected rollback failure")
		default:
			return nil
		}
	}

	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: twoFilePatch})
	require.Error(t, err)

	var recovery *RecoveryError
	require.ErrorAs(t, err, &recovery)
	assert.False(t, recovery.Applied)
	assert.Contains(t, recovery.RecoveryPaths, "a.txt")
	require.Len(t, patchTemporaryFiles(t, root), 1)
	assert.Equal(t, "new-a\n", string(readPatchFile(t, root, "a.txt")))
	assert.Equal(t, "old-b\n", string(readPatchFile(t, root, "b.txt")))

	backup := ""

	for _, name := range recovery.RecoveryPaths {
		if strings.HasPrefix(name, ".pips-backup-") {
			backup = name
			break
		}
	}

	require.NotEmpty(t, backup)
	assert.Equal(t, "old-a\n", string(readPatchFile(t, root, backup)))

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "recovery_required", header.Code)
}

func TestPatchCommitFailureRemovesCreatedParents(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	service.patchFault = func(phase, operation, name string) error {
		if phase == "commit" && operation == "link" && name == "z.txt" {
			return errors.New("injected commit failure")
		}

		return nil
	}

	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: nestedAddPatch})
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(root, "created"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(root, "z.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPatchRollbackRefusesReplacedCreatedDirectory(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	replaced := false
	service.patchFault = func(phase, operation, name string) error {
		switch {
		case phase == "commit" && operation == "link" && name == "z.txt":
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

	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: nestedAddPatch})
	require.Error(t, err)
	var recovery *RecoveryError
	require.ErrorAs(t, err, &recovery)
	assert.Contains(t, recovery.RecoveryPaths, "created/deep")
	assert.Contains(t, recovery.RecoveryPaths, "created")
	assert.Equal(t,
		"replacement",
		string(readPatchFile(t, root, filepath.Join("created", "deep", "marker"))),
	)
}

func TestPatchRejectsAncestorAndDescendantTargets(t *testing.T) {
	t.Parallel()

	service, root := newPatchService(t)
	patch := `*** Begin Patch
*** Add File: created
+parent
*** Add File: created/child.txt
+child
*** End Patch
`

	_, err := service.applyPatch(t.Context(), applyPatchArgs{Patch: patch})
	require.ErrorContains(t, err, "conflicts with descendant")
	_, statErr := os.Lstat(filepath.Join(root, "created"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func newPatchService(t *testing.T) (*service, string) {
	t.Helper()

	root := t.TempDir()
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return &service{tree: tree, limits: DefaultLimits()}, root
}

func writePatchFile(t *testing.T, root, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
}

func readPatchFile(t *testing.T, root, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // Tests use a fixed temporary root.
	require.NoError(t, err)

	return data
}

func patchTemporaryFiles(t *testing.T, root string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, ".pips-*"))
	require.NoError(t, err)

	for index := range matches {
		matches[index] = filepath.Base(matches[index])
	}

	return matches
}
