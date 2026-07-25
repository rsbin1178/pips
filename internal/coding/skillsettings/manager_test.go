package skillsettings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerSaveLoadAndClear(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, true, nil)
	ref := Ref{Source: "user:agents/review/SKILL.md", Name: "review"}

	require.NoError(t, fixture.manager.Save(t.Context(), Empty().WithDisabled(ref, true)))
	loaded, err := fixture.manager.Load(t.Context())
	require.NoError(t, err)
	assert.True(t, loaded.IsDisabled(ref))

	filePath := filepath.Join(fixture.root, filepath.FromSlash(paths.ProjectSkillsFile()))
	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, fixture.manager.Save(t.Context(), loaded.WithDisabled(ref, false)))
	loaded, err = fixture.manager.Load(t.Context())
	require.NoError(t, err)
	assert.False(t, loaded.IsDisabled(ref))
}

func TestManagerUntrustedDoesNotInspectOrWriteProject(t *testing.T) {
	t.Parallel()

	git := &fakeProjectGit{trackedErr: errors.New("must not inspect")}
	fixture := newManagerFixture(t, false, git)

	loaded, err := fixture.manager.Load(t.Context())
	require.NoError(t, err)
	assert.False(t, loaded.IsDisabled(Ref{}))
	assert.Zero(t, git.trackedCalls)
	require.ErrorIs(t, fixture.manager.Save(t.Context(), Empty()), ErrUntrusted)
	assert.Zero(t, git.trackedCalls)
}

func TestManagerRejectsTrackedOrUnsafeFile(t *testing.T) {
	t.Parallel()

	t.Run("tracked", func(t *testing.T) {
		t.Parallel()

		fixture := newManagerFixture(t, true, &fakeProjectGit{tracked: true, repository: true})
		_, err := fixture.manager.Load(t.Context())
		require.ErrorIs(t, err, ErrUnsafeFile)
	})

	t.Run("broad mode", func(t *testing.T) {
		t.Parallel()

		fixture := newManagerFixture(t, true, nil)
		writeSettingsFixture(t, fixture.root, 0o644, "schema = \""+Schema+"\"\n")
		_, err := fixture.manager.Load(t.Context())
		require.ErrorIs(t, err, ErrUnsafeFile)
	})

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()

		fixture := newManagerFixture(t, true, nil)
		projectDir := filepath.Join(fixture.root, filepath.FromSlash(paths.ProjectRoot()))
		require.NoError(t, os.MkdirAll(projectDir, 0o700))
		target := filepath.Join(t.TempDir(), "skills.toml")
		require.NoError(t, os.WriteFile(target, []byte("schema = \""+Schema+"\"\n"), 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(projectDir, "skills.toml")))

		_, err := fixture.manager.Load(t.Context())
		require.ErrorIs(t, err, ErrUnsafeFile)
	})
}

func TestManagerStrictDecodeAndLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		error   error
	}{
		{name: "unknown field", content: "schema = \"" + Schema + "\"\nunknown = true\n", error: ErrInvalid},
		{name: "unsupported schema", content: "schema = \"old\"\n", error: ErrInvalid},
		{name: "duplicate", content: "schema = \"" + Schema + "\"\n" +
			"[[disabled]]\nsource = \"user:agents/review/SKILL.md\"\nname = \"review\"\n" +
			"[[disabled]]\nsource = \"user:agents/review/SKILL.md\"\nname = \"review\"\n", error: ErrDuplicate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newManagerFixture(t, true, nil)
			writeSettingsFixture(t, fixture.root, 0o600, test.content)
			_, err := fixture.manager.Load(t.Context())
			require.ErrorIs(t, err, test.error)
		})
	}
}

func TestManagerRejectsOversizedFileAndPrewriteGitFailure(t *testing.T) {
	t.Parallel()

	t.Run("oversized file", func(t *testing.T) {
		t.Parallel()

		fixture := newManagerFixtureWithLimits(t, true, nil, Limits{
			MaxFileBytes: 32, MaxDisabled: 8,
		})
		writeSettingsFixture(
			t,
			fixture.root,
			0o600,
			"schema = \""+Schema+"\"\n# bounded fixture content\n",
		)
		_, err := fixture.manager.Load(t.Context())
		require.ErrorIs(t, err, ErrLimitExceeded)
	})

	t.Run("exclude failure writes nothing", func(t *testing.T) {
		t.Parallel()

		git := &fakeProjectGit{
			repository: true, excludeErr: errors.New("exclude unavailable"),
		}
		fixture := newManagerFixture(t, true, git)
		err := fixture.manager.Save(t.Context(), Empty().WithDisabled(Ref{
			Source: "user:agents/review/SKILL.md", Name: "review",
		}, true))
		require.ErrorContains(t, err, "exclude unavailable")

		_, statErr := os.Stat(filepath.Join(
			fixture.root,
			filepath.FromSlash(paths.ProjectSkillsFile()),
		))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})
}

func TestManagerRegistersGitLocalExclude(t *testing.T) {
	t.Parallel()

	git := &fakeProjectGit{repository: true}
	fixture := newManagerFixture(t, true, git)
	require.NoError(t, fixture.manager.Save(t.Context(), Empty()))
	assert.Equal(t, []string{paths.ProjectSkillsFile()}, git.excluded)
	assert.GreaterOrEqual(t, git.trackedCalls, 2)
}

type managerFixture struct {
	root    string
	manager *Manager
}

func newManagerFixture(t *testing.T, trusted bool, git ProjectGit) managerFixture {
	t.Helper()

	return newManagerFixtureWithLimits(t, trusted, git, DefaultLimits())
}

func newManagerFixtureWithLimits(
	t *testing.T,
	trusted bool,
	git ProjectGit,
	limits Limits,
) managerFixture {
	t.Helper()

	root := t.TempDir()
	opened, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	manager, err := New(Options{
		Tree: tree, Git: git, Trusted: trusted, Limits: limits,
	})
	require.NoError(t, err)

	return managerFixture{root: root, manager: manager}
}

func writeSettingsFixture(t *testing.T, root string, mode os.FileMode, content string) {
	t.Helper()

	filePath := filepath.Join(root, filepath.FromSlash(paths.ProjectSkillsFile()))
	require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o700))
	require.NoError(t, os.WriteFile(filePath, []byte(content), mode))
}

type fakeProjectGit struct {
	tracked      bool
	repository   bool
	trackedErr   error
	excludeErr   error
	trackedCalls int
	excluded     []string
}

func (f *fakeProjectGit) Tracked(context.Context, string) (bool, bool, error) {
	f.trackedCalls++

	return f.tracked, f.repository, f.trackedErr
}

func (f *fakeProjectGit) ExcludeLocal(_ context.Context, filePath string) error {
	f.excluded = append(f.excluded, filePath)

	return f.excludeErr
}
