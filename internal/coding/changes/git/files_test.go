package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectorListWorkspaceFilesRespectsRepositoryIgnores(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	fixture.write(".gitignore", ".npm-cache/\nnode_modules/\n.env*\n!.env.example\n")
	fixture.write(".env.example", "API_KEY=\n")
	fixture.write("nested/.gitignore", "*.tmp\n!important.tmp\n")
	fixture.write("src/main.ts", "export {}\n")
	fixture.commitAll("initial")

	fixture.write("notes.txt", "untracked\n")
	fixture.write("nested/ignored.tmp", "ignored\n")
	fixture.write("nested/important.tmp", "included\n")
	fixture.write(".env.local", "API_KEY=private\n")
	fixture.write(".npm-cache/_cacache/CACHEDIR.TAG", "cache\n")
	fixture.write("node_modules/pkg/index.js", "dependency\n")

	paths, repository, err := fixture.inspector.ListWorkspaceFiles(t.Context())
	require.NoError(t, err)
	assert.True(t, repository)
	assert.Equal(t, []string{
		".env.example",
		".gitignore",
		"nested/.gitignore",
		"nested/important.tmp",
		"notes.txt",
		"src/main.ts",
	}, paths)
}

func TestInspectorListWorkspaceFilesOutsideRepositoryAndAfterClose(t *testing.T) {
	t.Parallel()

	fixture := newGitFixtureWithoutRepository(t, DefaultLimits())
	paths, repository, err := fixture.inspector.ListWorkspaceFiles(t.Context())
	require.NoError(t, err)
	assert.False(t, repository)
	assert.Empty(t, paths)

	require.NoError(t, fixture.inspector.Close())
	_, _, err = fixture.inspector.ListWorkspaceFiles(t.Context())
	require.ErrorIs(t, err, ErrClosed)
}

func TestInspectorListWorkspaceFilesHonorsCancellation(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := fixture.inspector.ListWorkspaceFiles(canceled)
	require.ErrorIs(t, err, context.Canceled)
}
