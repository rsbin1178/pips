package workspace_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		allowRoot bool
		want      string
		wantError error
	}{
		{name: "file", path: "internal/coding/file.go", want: "internal/coding/file.go"},
		{name: "clean separators", path: "internal//coding/./file.go", want: "internal/coding/file.go"},
		{name: "root allowed", path: ".", allowRoot: true, want: "."},
		{name: "root rejected", path: ".", wantError: workspace.ErrInvalidPath},
		{name: "empty", wantError: workspace.ErrInvalidPath},
		{name: "absolute", path: "/etc/passwd", wantError: workspace.ErrOutsideRoot},
		{name: "windows volume", path: "C:/Windows/system.ini", wantError: workspace.ErrOutsideRoot},
		{name: "parent", path: "../outside", wantError: workspace.ErrOutsideRoot},
		{name: "embedded parent", path: "safe/../outside", wantError: workspace.ErrOutsideRoot},
		{name: "backslash", path: `safe\file`, wantError: workspace.ErrInvalidPath},
		{name: "nul", path: "safe\x00file", wantError: workspace.ErrInvalidPath},
		{name: "unicode", path: "源码/文件.go", want: "源码/文件.go"},
		{name: "long segment", path: strings.Repeat("a", 1024), want: strings.Repeat("a", 1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := workspace.NormalizePath(tt.path, tt.allowRoot)
			if tt.wantError != nil {
				require.ErrorIs(t, err, tt.wantError)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTreeReadsInternalSymlinkAndRejectsExternalEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "target.txt"), []byte("inside"), 0o600))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "internal-link")))
	require.NoError(t, os.Symlink("missing.txt", filepath.Join(root, "dangling-link")))

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "external-link")))

	tree := openTree(t, root)

	file, err := tree.Open("internal-link")
	require.NoError(t, err)
	data, err := fs.ReadFile(tree.FileSystem(), "internal-link")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	assert.Equal(t, "inside", string(data))

	_, err = tree.Open("external-link")
	require.Error(t, err)
	_, err = tree.Open("dangling-link")
	require.Error(t, err)
	_, _, err = tree.InspectMutationPath("dangling-link")
	require.ErrorIs(t, err, workspace.ErrSymlink)
	assert.Equal(t, "outside", string(mustReadFile(t, outside)))
}

func TestTreeInspectMutationPathRejectsSymlinksAndUnsupportedTypes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "real"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real", "file.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("real", filepath.Join(root, "linked")))

	tree := openTree(t, root)

	path, info, err := tree.InspectMutationPath("real/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "real/file.txt", path)
	assert.True(t, info.Mode().IsRegular())

	path, info, err = tree.InspectMutationPath("real/new.txt")
	require.NoError(t, err)
	assert.Equal(t, "real/new.txt", path)
	assert.Nil(t, info)

	_, _, err = tree.InspectMutationPath("linked/file.txt")
	require.ErrorIs(t, err, workspace.ErrSymlink)

	_, _, err = tree.InspectMutationPath("real")
	require.ErrorIs(t, err, workspace.ErrUnsupportedType)

	path, info, err = tree.InspectRegularPath("real/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "real/file.txt", path)
	assert.True(t, info.Mode().IsRegular())

	_, _, err = tree.InspectRegularPath("real/new.txt")
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, _, err = tree.InspectRegularPath("linked/file.txt")
	require.ErrorIs(t, err, workspace.ErrSymlink)
}

func TestTreeInspectAddPathReportsMissingParentsAndRejectsUnsafeComponents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "existing"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("existing", filepath.Join(root, "linked")))
	tree := openTree(t, root)

	name, info, missing, err := tree.InspectAddPath("existing/new/deep/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "existing/new/deep/file.txt", name)
	assert.Nil(t, info)
	assert.Equal(t, []string{"existing/new", "existing/new/deep"}, missing)

	_, _, _, err = tree.InspectAddPath("linked/file.txt")
	require.ErrorIs(t, err, workspace.ErrSymlink)

	_, _, _, err = tree.InspectAddPath("file/child.txt")
	require.ErrorIs(t, err, workspace.ErrUnsupportedType)
}

func TestTreeMutationUsesStableDirectoryAndExpiresHandles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("before"), 0o600))
	tree := openTree(t, root)

	var captured *workspace.MutationDir

	err := tree.Mutate(t.Context(), func(mutation *workspace.Mutation) error {
		dir, err := mutation.OpenDir(".")
		if err != nil {
			return err
		}

		captured = dir

		stage, err := dir.OpenFile(".stage", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}

		if _, err := stage.WriteString("after"); err != nil {
			_ = stage.Close()
			return err
		}

		if err := stage.Close(); err != nil {
			return err
		}

		if err := dir.Rename("file.txt", ".backup"); err != nil {
			return err
		}

		if err := dir.Rename(".stage", "file.txt"); err != nil {
			return err
		}

		if err := dir.Remove(".backup"); err != nil {
			return err
		}

		return dir.Sync()
	})
	require.NoError(t, err)
	assert.Equal(t, "after", string(mustReadFile(t, filepath.Join(root, "file.txt"))))

	_, err = captured.Lstat("file.txt")
	require.ErrorIs(t, err, workspace.ErrClosed)
	require.NoError(t, captured.Close())
}

func TestTreeDetectsReplacementAndClose(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o700))

	opened, err := workspace.Open(root)
	require.NoError(t, err)
	require.NoError(t, os.Rename(root, root+"-old"))
	require.NoError(t, os.Mkdir(root, 0o700))

	_, err = workspace.OpenTree(opened)
	require.ErrorIs(t, err, workspace.ErrChanged)

	tree := openTree(t, root)
	require.NoError(t, tree.Close())
	require.NoError(t, tree.Close())

	_, err = tree.Open(".")
	require.ErrorIs(t, err, workspace.ErrClosed)
	assert.ErrorIs(t, tree.Mutate(t.Context(), func(*workspace.Mutation) error { return nil }), workspace.ErrClosed)
}

func TestMutationDirMkdirCreatesDirectChild(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tree := openTree(t, root)

	err := tree.Mutate(t.Context(), func(mutation *workspace.Mutation) error {
		directory, openErr := mutation.OpenDir(".")
		if openErr != nil {
			return openErr
		}
		defer func() { _ = directory.Close() }()

		return directory.Mkdir(".pips", 0o700)
	})
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(root, ".pips"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, fs.FileMode(0o700), info.Mode().Perm())
}

func TestMutationDirMkdirRejectsInvalidOrExistingChild(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "existing"), 0o700))
	tree := openTree(t, root)

	err := tree.Mutate(t.Context(), func(mutation *workspace.Mutation) error {
		directory, openErr := mutation.OpenDir(".")
		if openErr != nil {
			return openErr
		}
		defer func() { _ = directory.Close() }()

		require.Error(t, directory.Mkdir("../outside", 0o700))
		require.Error(t, directory.Mkdir("existing", 0o700))

		return nil
	})
	require.NoError(t, err)
}

func TestTreeMutationHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	tree := openTree(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := tree.Mutate(ctx, func(*workspace.Mutation) error {
		return errors.New("must not run")
	})
	require.ErrorIs(t, err, context.Canceled)
}

func FuzzNormalizePath(f *testing.F) {
	for _, seed := range []string{"file.go", ".", "../outside", "/absolute", "a/b/c", "a\\b"} {
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, value string, allowRoot bool) {
		got, err := workspace.NormalizePath(value, allowRoot)
		if err != nil {
			return
		}

		assert.True(t, fs.ValidPath(got))
		assert.False(t, filepath.IsAbs(got))
		assert.NotContains(t, got, "\\")

		for segment := range strings.SplitSeq(got, "/") {
			assert.NotEqual(t, "..", segment)
		}
	})
}

func openTree(t *testing.T, root string) *workspace.Tree {
	t.Helper()

	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return tree
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // Tests read fixed temporary paths.
	require.NoError(t, err)

	return data
}
