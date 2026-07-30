package attachment

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		kind Kind
		err  error
	}{
		{name: "text", path: "internal/coding/main.go", kind: KindText},
		{name: "unicode", path: "文档/说明.txt", kind: KindText},
		{name: "image", path: "assets/SCREEN.PNG", kind: KindImage},
		{name: "kind changed", path: "assets/SCREEN.PNG", kind: KindText, err: workspace.ErrChanged},
		{name: "normalizes", path: "docs/./guide.md", kind: KindText},
		{name: "parent traversal", path: "../secret", err: workspace.ErrOutsideRoot},
		{name: "absolute", path: "/etc/passwd", err: workspace.ErrOutsideRoot},
		{name: "backslash", path: `dir\file`, err: workspace.ErrInvalidPath},
		{name: "control", path: "dir/line\nbreak", err: workspace.ErrInvalidPath},
		{name: "git metadata", path: ".git/config", err: ErrDenied},
		{name: "nested environment", path: "config/.env.production", err: ErrDenied},
		{name: "netrc", path: ".netrc", err: ErrDenied},
		{name: "npm credentials", path: "project/.npmrc", err: ErrDenied},
		{name: "python credentials", path: ".pypirc", err: ErrDenied},
		{name: "ssh key", path: "fixtures/id_ed25519", err: ErrDenied},
		{name: "pem", path: "certs/client.pem", err: ErrDenied},
		{name: "key extension", path: "certs/client.KEY", err: ErrDenied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeReference(Reference{Path: test.path, Kind: test.kind})
			if test.err != nil {
				require.ErrorIs(t, err, test.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.kind, got.Kind)
			assert.Equal(t, filepath.ToSlash(filepath.Clean(test.path)), got.Path)
		})
	}
}

func TestDiscoverFiltersAndSortsWorkspaceFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "z.txt", "z")
	writeFixture(t, root, "src/main.go", "package main")
	writeFixture(t, root, "test/main.go", "package test")
	writeFixture(t, root, "src/main.png", "not decoded yet")
	writeFixture(t, root, "src/重复.go", "package repeated")
	writeFixture(t, root, ".git/config", "private")
	writeFixture(t, root, ".env.local", "private")
	writeFixture(t, root, "keys/id_rsa", "private")
	require.NoError(t, os.Symlink("z.txt", filepath.Join(root, "linked.txt")))
	require.NoError(t, os.Symlink("src", filepath.Join(root, "linked-directory")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o700))

	tree := openAttachmentTree(t, root)
	snapshot, err := Discover(t.Context(), tree)
	require.NoError(t, err)

	assert.False(t, snapshot.Truncated)
	assert.Equal(t, []Summary{
		{Path: "src/main.go", Kind: KindText, Size: 12},
		{Path: "src/main.png", Kind: KindImage, Size: 15},
		{Path: "src/重复.go", Kind: KindText, Size: 16},
		{Path: "test/main.go", Kind: KindText, Size: 12},
		{Path: "z.txt", Kind: KindText, Size: 1},
	}, snapshot.Files)

	cloned := snapshot.Clone()
	cloned.Files[0].Path = "changed"
	assert.Equal(t, "src/main.go", snapshot.Files[0].Path)
}

func TestDiscoverHonorsEntryAndResultLimits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		writeFixture(t, root, name, name)
	}

	tree := openAttachmentTree(t, root)

	byEntries, err := discover(t.Context(), tree, discoveryLimits{entries: 2, results: 10})
	require.NoError(t, err)
	assert.True(t, byEntries.Truncated)
	assert.Len(t, byEntries.Files, 2)

	byResults, err := discover(t.Context(), tree, discoveryLimits{entries: 10, results: 2})
	require.NoError(t, err)
	assert.True(t, byResults.Truncated)
	assert.Equal(t, []string{"a.txt", "b.txt"}, []string{
		byResults.Files[0].Path,
		byResults.Files[1].Path,
	})
}

func TestDiscoverHandlesDeepTreesAndCancellation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	path := root
	for index := range 32 {
		path = filepath.Join(path, string(rune('a'+index%26)))
		require.NoError(t, os.Mkdir(path, 0o700))
	}

	writeFixture(t, path, "leaf.txt", "leaf")
	tree := openAttachmentTree(t, root)

	snapshot, err := Discover(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, snapshot.Files, 1)
	assert.True(t, strings.HasSuffix(snapshot.Files[0].Path, "/leaf.txt"))

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = Discover(canceled, tree)
	require.ErrorIs(t, err, context.Canceled)
}

func TestResolveTextUsesStableOpenedHandle(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "notes.txt", "exact\r\ncontent  ")
	tree := openAttachmentTree(t, root)

	resolved, err := ResolveText(t.Context(), tree, Reference{Path: "notes.txt", Kind: KindText})
	require.NoError(t, err)
	assert.Equal(t, "exact\r\ncontent  ", resolved.Content)
	assert.Equal(t, "\n\n[Workspace file: notes.txt]\nexact\r\ncontent  ", resolved.PromptText())
}

func TestResolveTextAcceptsExactMaximum(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := strings.Repeat("x", MaxTextBytes)
	writeFixture(t, root, "maximum.txt", content)
	tree := openAttachmentTree(t, root)

	resolved, err := ResolveText(t.Context(), tree, Reference{Path: "maximum.txt"})
	require.NoError(t, err)
	assert.Equal(t, content, resolved.Content)
}

func TestResolveTextRejectsInvalidContentAndUnsafePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBytesFixture(t, root, "invalid.txt", []byte{0xff})
	writeFixture(t, root, "nul.txt", "before\x00after")
	writeFixture(t, root, "large.txt", strings.Repeat("x", MaxTextBytes+1))
	writeFixture(t, root, "real/file.txt", "inside")
	require.NoError(t, os.Symlink("real", filepath.Join(root, "linked")))
	tree := openAttachmentTree(t, root)

	tests := []struct {
		name string
		path string
		err  error
	}{
		{name: "invalid utf8", path: "invalid.txt", err: ErrBinaryText},
		{name: "nul", path: "nul.txt", err: ErrBinaryText},
		{name: "size", path: "large.txt", err: ErrLimit},
		{name: "symlink parent", path: "linked/file.txt", err: workspace.ErrSymlink},
		{name: "image deferred", path: "image.png", err: ErrImagePending},
		{name: "denied", path: ".env", err: ErrDenied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ResolveText(t.Context(), tree, Reference{Path: test.path})
			require.ErrorIs(t, err, test.err)
		})
	}

	_, err := ResolveText(t.Context(), tree, Reference{Path: "nul.txt"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "before")
}

func TestResolveTextRejectsReplacementAndDeletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{
			name: "replacement",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Rename(
					filepath.Join(root, "file.txt"),
					filepath.Join(root, "old.txt"),
				))
				writeFixture(t, root, "file.txt", "replacement")
			},
		},
		{
			name: "deletion",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(root, "file.txt")))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeFixture(t, root, "file.txt", "before")
			tree := openAttachmentTree(t, root)

			_, err := resolveText(
				t.Context(),
				tree,
				Reference{Path: "file.txt"},
				func() { test.mutate(t, root) },
			)
			require.ErrorIs(t, err, workspace.ErrChanged)
		})
	}
}

func TestResolveTextHonorsCancellation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "file.txt", "content")
	tree := openAttachmentTree(t, root)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ResolveText(canceled, tree, Reference{Path: "file.txt"})
	require.ErrorIs(t, err, context.Canceled)
}

func FuzzNormalizeReference(f *testing.F) {
	for _, seed := range []string{"file.go", "文档/说明.md", "../outside", ".git/config", "a\\b"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		reference, err := NormalizeReference(Reference{Path: value})
		if err != nil {
			return
		}

		assert.True(t, fs.ValidPath(reference.Path))
		assert.True(t, utf8.ValidString(reference.Path))
		assert.LessOrEqual(t, len(reference.Path), MaxPathBytes)
		assert.False(t, deniedPath(reference.Path))

		for _, character := range reference.Path {
			assert.False(t, unicode.IsControl(character))
		}
	})
}

func openAttachmentTree(t *testing.T, root string) *workspace.Tree {
	t.Helper()

	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return tree
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	writeBytesFixture(t, root, name, []byte(content))
}

func writeBytesFixture(t *testing.T, root, name string, content []byte) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
	require.NoError(t, os.WriteFile(full, content, 0o600))
}
