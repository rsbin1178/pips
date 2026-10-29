package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptBytesValidationAndDefaults(t *testing.T) {
	t.Parallel()

	cfg := Defaults()
	cfg.Model.Provider = ai.ProviderOpenAI
	cfg.Model.Model = "test-model"
	assert.Equal(t, DefaultTranscriptBytes, cfg.TUI.TranscriptBytes)

	cfg.TUI.TranscriptBytes = MinTranscriptBytes - 1
	require.ErrorIs(t, cfg.ValidateRuntime(), ErrInvalid)

	cfg.TUI.TranscriptBytes = MaxTranscriptBytes + 1
	require.ErrorIs(t, cfg.ValidateRuntime(), ErrInvalid)

	cfg.TUI.TranscriptBytes = 2 << 20
	require.NoError(t, cfg.ValidateRuntime())
}

func TestTranscriptBytesLoadsFromFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[tui]\ntranscript_bytes = 2097152\n"), 0o600))

	loaded, err := Load(LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, 2<<20, loaded.Config.TUI.TranscriptBytes)
	source, ok := loaded.Config.Source(FieldTranscriptBytes)
	require.True(t, ok)
	assert.Equal(t, SourceConfigFile, source.Kind)
}
