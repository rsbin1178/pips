//nolint:wsl_v5 // Parse cases and their assertions stay adjacent.
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseInteractiveScreenAndAltScreenPolicy(t *testing.T) {
	t.Parallel()

	for _, value := range []string{config.ScreenFullscreen, config.ScreenInline} {
		got, err := config.ParseInteractiveScreen(value)
		require.NoError(t, err)
		assert.Equal(t, value, got)
	}
	for _, value := range []string{"", "full", "FULLSCREEN", " native"} {
		_, err := config.ParseInteractiveScreen(value)
		require.ErrorIs(t, err, config.ErrInvalid, "%q", value)
	}

	for _, value := range []string{config.AltScreenAuto, config.AltScreenAlways, config.AltScreenNever} {
		got, err := config.ParseAltScreenPolicy(value)
		require.NoError(t, err)
		assert.Equal(t, value, got)
	}
	for _, value := range []string{"", "yes", "TRUE", "inline"} {
		_, err := config.ParseAltScreenPolicy(value)
		require.ErrorIs(t, err, config.ErrInvalid, "%q", value)
	}
}

func TestDefaultsShipFullscreenWithMouseAndAutomaticBuffer(t *testing.T) {
	t.Parallel()

	defaults := config.Defaults()
	assert.Equal(t, config.ScreenFullscreen, defaults.TUI.Screen)
	assert.Equal(t, config.AltScreenAuto, defaults.TUI.AltScreen)
	assert.True(t, defaults.TUI.Mouse)
	assert.Equal(t, config.ExitOutputResumeHint, defaults.TUI.ExitOutput)
	assert.True(t, defaults.TUI.ShowThinkingBlocks)
	// Defaults intentionally leave the model unset, so runtime validation of the
	// whole config is not the right check here; validate only the new fields.
	_, err := config.ParseInteractiveScreen(defaults.TUI.Screen)
	require.NoError(t, err)
	_, err = config.ParseAltScreenPolicy(defaults.TUI.AltScreen)
	require.NoError(t, err)
	_, err = config.ParseExitOutput(defaults.TUI.ExitOutput)
	require.NoError(t, err)
}

func TestParseExitOutput(t *testing.T) {
	t.Parallel()

	for _, value := range []string{config.ExitOutputTranscript, config.ExitOutputResumeHint} {
		got, err := config.ParseExitOutput(value)
		require.NoError(t, err)
		assert.Equal(t, value, got)
	}
	for _, value := range []string{"", "full", "resume", "TRANSCRIPT"} {
		_, err := config.ParseExitOutput(value)
		require.ErrorIs(t, err, config.ErrInvalid, "%q", value)
	}
}

func TestLoadAppliesTUIScreenSettings(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path,
		"[tui]\nscreen = \"inline\"\nalt_screen = \"never\"\nmouse = false\n"+
			"exit_output = \"resume-hint\"\nshow_thinking_blocks = false\n")
	loaded, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  config.LookupEnv(func(string) (string, bool) { return "", false }),
	})
	require.NoError(t, err)
	assert.Equal(t, config.ScreenInline, loaded.Config.TUI.Screen)
	assert.Equal(t, config.AltScreenNever, loaded.Config.TUI.AltScreen)
	assert.False(t, loaded.Config.TUI.Mouse)
	assert.Equal(t, config.ExitOutputResumeHint, loaded.Config.TUI.ExitOutput)
	assert.False(t, loaded.Config.TUI.ShowThinkingBlocks)

	source, ok := loaded.Config.Source(config.FieldScreen)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	_, ok = loaded.Config.Source(config.FieldMouse)
	assert.True(t, ok)
	source, ok = loaded.Config.Source(config.FieldExitOutput)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	source, ok = loaded.Config.Source(config.FieldShowThinkingBlocks)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
}

func TestLoadRejectsInvalidTUIScreenSettings(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"[tui]\nscreen = \"native\"\n",
		"[tui]\nalt_screen = \"sometimes\"\n",
		"[tui]\nexit_output = \"scrollback\"\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, body)
		_, err := config.Load(config.LoadOptions{
			ConfigFile: path,
			LookupEnv:  config.LookupEnv(func(string) (string, bool) { return "", false }),
		})
		require.Error(t, err)
		require.ErrorIs(t, err, config.ErrInvalid)
		assert.Contains(t, err.Error(), "tui")
	}
}
