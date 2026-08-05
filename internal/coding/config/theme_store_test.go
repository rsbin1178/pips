//nolint:gosec,wsl_v5 // Persistence tests intentionally exercise unsafe modes and paths.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/statusline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveThemeReplacesOnlyTheScalarToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# keep this comment\nmode = \"agent\"\n\n[tui]\n  theme = \"default-dark\" # keep this comment\n\n[providers.openai.models.gpt]\n"
	writeFile(t, path, original)

	require.NoError(t, config.SaveThemeWithOptions(path, "dracula", config.ThemeSaveOptions{}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	want := strings.Replace(original, `"default-dark"`, `"dracula"`, 1)
	assert.Equal(t, want, string(data))

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "dracula", loaded.Config.TUI.Theme)
}

func TestSaveThemeInsertsIntoExistingTableAndAppendsMissingTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "existing table",
			content: "tool_search = true\n\n[tui]\n# local selection\n\n[providers.openai.models.gpt]\n",
			want:    "tool_search = true\n\n[tui]\n# local selection\n\ntheme = \"nord\"\n[providers.openai.models.gpt]\n",
		},
		{
			name:    "table at eof",
			content: "[tui]",
			want:    "[tui]\ntheme = \"nord\"\n",
		},
		{
			name:    "missing table",
			content: "tool_search = true\n",
			want:    "tool_search = true\n[tui]\ntheme = \"nord\"\n",
		},
		{
			name:    "missing table without final newline",
			content: "tool_search = true",
			want:    "tool_search = true\n[tui]\ntheme = \"nord\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.content)
			require.NoError(t, config.SaveThemeWithOptions(path, "nord", config.ThemeSaveOptions{}))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(data))
		})
	}
}

func TestSaveThemeCreatesOnlyWhenAllowed(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.ErrorIs(t, config.SaveThemeWithOptions(path, "auto", config.ThemeSaveOptions{}), config.ErrFile)
	require.NoError(t, config.SaveTheme(path, "auto"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "[tui]\ntheme = \"auto\"\n", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveThemeCreatesMissingDefaultParentPrivately(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "nested", "private")
	path := filepath.Join(parent, "config.toml")

	require.NoError(t, config.SaveThemeWithOptions(path, "nord", config.ThemeSaveOptions{AllowCreate: true}))

	for _, directory := range []string{filepath.Join(root, "nested"), parent} {
		info, err := os.Stat(directory)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "[tui]\ntheme = \"nord\"\n", string(data))
}

func TestSaveThemeRejectsMissingExplicitParent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "config.toml")
	err := config.SaveThemeWithOptions(path, "nord", config.ThemeSaveOptions{AllowCreate: false})
	require.ErrorIs(t, err, config.ErrFile)
	_, statErr := os.Stat(filepath.Dir(path))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSaveThemeRejectsMalformedUnknownAndAmbiguousSources(t *testing.T) {
	t.Parallel()

	tests := []string{
		"[tui]\ntheme = \"dracula\"\nunknown = true\n",
		"[tui]\ntheme = \"dracula\"\n[tui]\n",
		"tui = {theme = \"dracula\"}\n",
		"[[tui]]\ntheme = \"dracula\"\n",
		"[tui]\ntheme = \"dracula\"\n[tui.palette]\nvalue = true\n",
		"[tui]\ntheme = \"dracula\"\ntheme = \"nord\"\n",
		"[tui]\ntheme = \"\"\"dracula\"\"\"\n",
		"[tui]\ntheme = \"dracula\" trailing\n",
		"[providers.local]\nbase_url = \"\"\"\n[tui]\ntheme = \"fake\"\n\"\"\"\n",
		"[tui]\ntheme = \"dracula\n",
	}
	for _, content := range tests {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, content)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Error(t, config.SaveThemeWithOptions(path, "nord", config.ThemeSaveOptions{}))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSaveThemeRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"auto\"\n")
	require.NoError(t, os.Chmod(path, 0o664))
	require.ErrorIs(t, config.SaveThemeWithOptions(path, "nord", config.ThemeSaveOptions{}), config.ErrUnsafe)

	target := filepath.Join(directory, "target.toml")
	writeFile(t, target, "[tui]\ntheme = \"auto\"\n")
	link := filepath.Join(directory, "link.toml")
	require.NoError(t, os.Symlink(target, link))
	assert.ErrorIs(t, config.SaveThemeWithOptions(link, "nord", config.ThemeSaveOptions{}), config.ErrUnsafe)
}

func TestSaveThemeSerializesConcurrentSaves(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"auto\"\n")
	values := []string{"dracula", "nord", "one-dark", "solarized-light"}
	var (
		group    sync.WaitGroup
		errorsCh = make(chan error, len(values))
	)
	for _, value := range values {
		group.Go(func() {
			errorsCh <- config.SaveThemeWithOptions(path, value, config.ThemeSaveOptions{})
		})
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Contains(t, values, loaded.Config.TUI.Theme)
}

func TestSaveThemeSelectionValidationDoesNotTouchDisk(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.toml")
	err := config.SaveTheme(path, "not valid!")
	require.ErrorIs(t, err, config.ErrInvalid)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSaveThemeRejectsEditedConfigOverSizeLimit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	prefix := "[tui]\ntheme = \"a\"\n#"
	content := prefix + strings.Repeat("x", configFileSizeForTest()-len(prefix))
	writeFile(t, path, content)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	err = config.SaveThemeWithOptions(path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", config.ThemeSaveOptions{})
	require.ErrorIs(t, err, config.ErrFile)
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
}

func configFileSizeForTest() int { return 1 << 20 }

func TestSaveStatusLineReplacesOnlyTheArrayAndPreservesTOML(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# keep this comment\nmode = \"agent\"\n\n[tui]\n  theme = \"nord\" # keep theme\n  status_line = [\"workspace\", \"phase\"] # keep status\n\n[providers.openai.models.gpt]\n"
	writeFile(t, path, original)

	items := []statusline.Item{statusline.Model, statusline.ContextUsed}
	require.NoError(t, config.SaveStatusLineWithOptions(path, items, config.TUIConfigSaveOptions{}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	want := strings.Replace(original, `["workspace", "phase"]`, `["model", "context_used"]`, 1)
	assert.Equal(t, want, string(data))

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, items, loaded.Config.TUI.StatusLine)
	assert.Equal(t, "nord", loaded.Config.TUI.Theme)
}

func TestSaveStatusLineInsertsAndPreservesExplicitEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
		items   []statusline.Item
	}{
		{
			name:    "existing table",
			content: "tool_search = true\n\n[tui]\n# local preferences\n\ntheme = \"nord\"\n[providers.openai.models.gpt]\n",
			want:    "tool_search = true\n\n[tui]\n# local preferences\n\ntheme = \"nord\"\nstatus_line = [\"workspace\", \"session\"]\n[providers.openai.models.gpt]\n",
			items:   []statusline.Item{statusline.Workspace, statusline.Session},
		},
		{
			name:    "missing table",
			content: "tool_search = true\n",
			want:    "tool_search = true\n[tui]\nstatus_line = []\n",
			items:   []statusline.Item{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.content)
			require.NoError(t, config.SaveStatusLineWithOptions(path, tt.items, config.TUIConfigSaveOptions{}))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(data))
		})
	}
}

func TestSaveStatusLineRejectsUnsupportedOrInvalidSources(t *testing.T) {
	t.Parallel()

	tests := []string{
		"[tui]\nstatus_line = [\"workspace\",\n  \"phase\"]\n",
		"[tui]\nstatus_line = [\"workspace\"] trailing\n",
		"[tui]\nstatus_line = [\"workspace\", \"unknown\"]\n",
		"[tui]\nstatus_line = [\"workspace\"]\nstatus_line = []\n",
		"[tui]\nstatus_line = [\"workspace\"]\n[tui.palette]\nvalue = true\n",
		"tui.status_line = [\"workspace\"]\n",
	}
	for _, content := range tests {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, content)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			err = config.SaveStatusLineWithOptions(
				path,
				[]statusline.Item{statusline.Phase},
				config.TUIConfigSaveOptions{},
			)
			require.Error(t, err)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSaveThemeAndStatusLineSharePerPathLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"auto\"\nstatus_line = [\"workspace\"]\n")

	var group sync.WaitGroup
	errorsCh := make(chan error, 2)
	group.Go(func() {
		errorsCh <- config.SaveThemeWithOptions(path, "nord", config.TUIConfigSaveOptions{})
	})
	group.Go(func() {
		errorsCh <- config.SaveStatusLineWithOptions(
			path,
			[]statusline.Item{statusline.Model, statusline.Phase},
			config.TUIConfigSaveOptions{},
		)
	})
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "nord", loaded.Config.TUI.Theme)
	assert.Equal(t, []statusline.Item{statusline.Model, statusline.Phase}, loaded.Config.TUI.StatusLine)
}
