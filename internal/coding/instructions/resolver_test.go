package instructions_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/instructions"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveOrdersRootToScopeAndPreservesSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeInstruction(t, root, "AGENTS.md", "root rules\n")
	writeInstruction(t, root, "internal/AGENTS.md", "internal rules")
	writeInstruction(t, root, "internal/coding/AGENTS.md", "coding rules\n")

	resolver := newResolver(t, root, instructions.DefaultLimits())
	set, err := resolver.Resolve(t.Context(), "internal/coding")
	require.NoError(t, err)

	sources := set.Sources()
	require.Len(t, sources, 3)
	assert.Equal(t, []string{"AGENTS.md", "internal/AGENTS.md", "internal/coding/AGENTS.md"}, []string{
		sources[0].Path, sources[1].Path, sources[2].Path,
	})
	assert.Equal(t, []string{".", "internal", "internal/coding"}, []string{
		sources[0].Scope, sources[1].Scope, sources[2].Scope,
	})
	assert.Equal(t, len("root rules\ninternal rulescoding rules\n"), set.Bytes())

	prompt := set.SystemPrompt()
	rootIndex := assertIndex(t, prompt, `source="AGENTS.md"`)
	internalIndex := assertIndex(t, prompt, `source="internal/AGENTS.md"`)
	codingIndex := assertIndex(t, prompt, `source="internal/coding/AGENTS.md"`)
	assert.Less(t, rootIndex, internalIndex)
	assert.Less(t, internalIndex, codingIndex)
	assert.Contains(t, prompt, "the later source takes precedence")

	sources[0].Content = "changed"
	assert.Equal(t, "root rules\n", set.Sources()[0].Content)
}

func TestResolveSupportsRootAndMissingFiles(t *testing.T) {
	t.Parallel()

	resolver := newResolver(t, t.TempDir(), instructions.DefaultLimits())
	set, err := resolver.Resolve(t.Context(), ".")
	require.NoError(t, err)
	assert.Empty(t, set.Sources())
	assert.Empty(t, set.SystemPrompt())
	assert.Zero(t, set.Bytes())
}

func TestResolveAllowsInternalSymlinkButRejectsExternalEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeInstruction(t, root, "shared.md", "shared rules")
	require.NoError(t, os.Symlink("shared.md", filepath.Join(root, "AGENTS.md")))

	resolver := newResolver(t, root, instructions.DefaultLimits())
	set, err := resolver.Resolve(t.Context(), ".")
	require.NoError(t, err)
	require.Len(t, set.Sources(), 1)
	assert.Equal(t, "shared rules", set.Sources()[0].Content)

	require.NoError(t, os.Remove(filepath.Join(root, "AGENTS.md")))
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "AGENTS.md")))

	_, err = resolver.Resolve(t.Context(), ".")
	require.ErrorIs(t, err, instructions.ErrFile)
}

func TestResolveRejectsInvalidScopeBinaryAndLimits(t *testing.T) {
	t.Parallel()

	t.Run("scope is file", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeInstruction(t, root, "file", "content")
		resolver := newResolver(t, root, instructions.DefaultLimits())
		_, err := resolver.Resolve(t.Context(), "file")
		require.ErrorIs(t, err, instructions.ErrInvalid)
	})

	t.Run("binary", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeInstruction(t, root, "AGENTS.md", "text\x00binary")
		resolver := newResolver(t, root, instructions.DefaultLimits())
		_, err := resolver.Resolve(t.Context(), ".")
		require.ErrorIs(t, err, instructions.ErrBinary)
	})

	t.Run("file limit", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeInstruction(t, root, "AGENTS.md", "12345")
		resolver := newResolver(t, root, instructions.Limits{FileBytes: 4, TotalBytes: 8})
		_, err := resolver.Resolve(t.Context(), ".")
		require.ErrorIs(t, err, instructions.ErrTooLarge)
	})

	t.Run("total limit", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeInstruction(t, root, "AGENTS.md", "1234")
		writeInstruction(t, root, "nested/AGENTS.md", "5678")
		resolver := newResolver(t, root, instructions.Limits{FileBytes: 4, TotalBytes: 7})
		_, err := resolver.Resolve(t.Context(), "nested")
		require.ErrorIs(t, err, instructions.ErrTooLarge)
	})
}

func TestResolveHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	resolver := newResolver(t, t.TempDir(), instructions.DefaultLimits())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := resolver.Resolve(ctx, ".")
	require.ErrorIs(t, err, context.Canceled)
}

func TestNewRejectsInvalidDependenciesAndLimits(t *testing.T) {
	t.Parallel()

	_, err := instructions.New(nil, instructions.DefaultLimits())
	require.ErrorIs(t, err, instructions.ErrInvalid)

	tree := openInstructionTree(t, t.TempDir())
	_, err = instructions.New(tree, instructions.Limits{})
	require.ErrorIs(t, err, instructions.ErrInvalid)

	_, err = instructions.New(tree, instructions.Limits{FileBytes: 2, TotalBytes: 1})
	require.ErrorIs(t, err, instructions.ErrInvalid)
}

func newResolver(t *testing.T, root string, limits instructions.Limits) *instructions.Resolver {
	t.Helper()

	resolver, err := instructions.New(openInstructionTree(t, root), limits)
	require.NoError(t, err)

	return resolver
}

func openInstructionTree(t *testing.T, root string) *workspace.Tree {
	t.Helper()

	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return tree
}

func writeInstruction(t *testing.T, root, relative, content string) {
	t.Helper()

	name := filepath.Join(root, filepath.FromSlash(relative))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
}

func assertIndex(t *testing.T, value, substring string) int {
	t.Helper()

	index := strings.Index(value, substring)

	require.NotEqual(t, -1, index)

	return index
}
