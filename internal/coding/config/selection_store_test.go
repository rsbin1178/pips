//nolint:gosec,wsl_v5 // Persistence cases stay adjacent to the source they assert.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveSelectionReplacesScalarsAndMovesDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	original := `# keep this header
mode = "agent"
variant = "balanced" # keep this comment
reasoning = "low"

[providers.openai.models."gpt-5"]
default = true
reasoning_levels = ["low", "high"]

[providers.openai.models."gpt-5".variants.deep]
reasoning_level = "high"

[providers.anthropic.models.claude]
reasoning_levels = ["low", "high", "max"]

[providers.anthropic.models.claude.variants.deep]
reasoning_level = "high"
`
	writeFile(t, path, original)

	level := config.ReasoningLevel("high")
	selection := config.SelectionPreference{
		Model:     config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"},
		Variant:   "deep",
		Reasoning: &level,
	}
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `variant = "deep" # keep this comment`)
	assert.Contains(t, content, `reasoning = "high"`)
	assert.Contains(t, content, "[providers.openai.models.\"gpt-5\"]\ndefault = false")
	assert.Contains(t, content, "[providers.anthropic.models.claude]\ndefault = true")
	assert.NotContains(t, content, "[providers.openai.models.\"gpt-5\"]\ndefault = true")

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude", loaded.Config.Model.String())
	assert.Equal(t, "deep", loaded.Config.Variant)
	require.NotNil(t, loaded.Config.Reasoning)
	assert.Equal(t, config.ReasoningLevel("high"), *loaded.Config.Reasoning)
}

func TestSaveSelectionAppendsScalarsAndMissingModelTable(t *testing.T) {
	t.Parallel()

	t.Run("missing model table", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, "mode = \"agent\"\n")

		selection := config.SelectionPreference{
			Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
		}
		require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		want := "mode = \"agent\"\n\n[providers.openai.models.gpt-5]\ndefault = true\n"
		assert.Equal(t, want, string(data))

		loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
		require.NoError(t, err)
		assert.Equal(t, "openai/gpt-5", loaded.Config.Model.String())
	})

	t.Run("existing model table without top-level selection", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, `mode = "agent"

[providers.openai.models.gpt-5]
reasoning_levels = ["high"]

[providers.openai.models.gpt-5.variants.deep]
reasoning_level = "high"
`)

		level := config.ReasoningLevel("high")
		selection := config.SelectionPreference{
			Model:     config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
			Variant:   "deep",
			Reasoning: &level,
		}
		require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		content := string(data)
		assert.Contains(t, content, "variant = \"deep\"")
		assert.Contains(t, content, "reasoning = \"high\"")
		assert.Contains(t, content, "[providers.openai.models.gpt-5]\ndefault = true\nreasoning_levels")

		loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
		require.NoError(t, err)
		assert.Equal(t, "openai/gpt-5", loaded.Config.Model.String())
		assert.Equal(t, "deep", loaded.Config.Variant)
		require.NotNil(t, loaded.Config.Reasoning)
		assert.Equal(t, config.ReasoningLevel("high"), *loaded.Config.Reasoning)
	})
}

func TestSaveSelectionMovesDefaultToANewTable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[providers.openai.models.gpt-5]
default = true
`)

	selection := config.SelectionPreference{
		Model: config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"},
	}
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "[providers.openai.models.gpt-5]\ndefault = false")
	assert.Contains(t, content, "[providers.anthropic.models.claude]\ndefault = true")

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude", loaded.Config.Model.String())
}

func TestSaveSelectionRemovesEmptyScalarsAndKeepsComments(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "variant = \"deep\" # keep this\nreasoning = \"high\"\nmode = \"agent\"\n")

	selection := config.SelectionPreference{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
	}
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.NotContains(t, content, "variant =")
	assert.NotContains(t, content, "reasoning =")
	assert.Contains(t, content, "# keep this")
	assert.Contains(t, content, "[providers.openai.models.gpt-5]\ndefault = true")

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Empty(t, loaded.Config.Variant)
	assert.Nil(t, loaded.Config.Reasoning)
}

func TestSaveSelectionRejectsInlineModelShapes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[providers.openai.models]\ngpt-5 = { default = true }\n")

	err := config.SaveSelectionWithOptions(
		path,
		config.SelectionPreference{Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"}},
		config.TUIConfigSaveOptions{},
	)
	require.ErrorIs(t, err, config.ErrSelectionEditUnsupported)
}

func TestSaveSelectionRequiresASupportedVariantAndLevel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[providers.openai.models.gpt-5]
default = true
`)
	level := config.ReasoningLevel("high")
	selection := config.SelectionPreference{
		Model:     config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
		Variant:   "deep",
		Reasoning: &level,
	}
	err := config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{})
	require.ErrorIs(t, err, config.ErrInvalid)

	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "[providers.openai.models.gpt-5]\ndefault = true\n", string(data),
		"a rejected save leaves the source untouched")
}

func TestSaveSelectionMatchesQuotedModelKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[providers.openai.models."org/model.v1"]
default = true
`)

	selection := config.SelectionPreference{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "org/model.v1"},
	}
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "openai/org/model.v1", loaded.Config.Model.String())
}

func TestSaveSelectionCreatesOnlyWhenAllowed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	selection := config.SelectionPreference{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
	}
	require.ErrorIs(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}), config.ErrFile)
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{AllowCreate: true}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "[providers.openai.models.gpt-5]\ndefault = true\n", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveSelectionPreservesUnrelatedTables(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	original := strings.Join([]string{
		"[tui]",
		"theme = \"nord\" # keep",
		"",
		"[providers.openai.models.gpt-5]",
		"default = true",
		"context_window = 128000",
		"reasoning_levels = [\"high\"]",
		"",
		"[providers.openai.models.\"gpt-5\".variants.deep]",
		"reasoning_level = \"high\"",
		"",
	}, "\n")
	writeFile(t, path, original)

	level := config.ReasoningLevel("high")
	selection := config.SelectionPreference{
		Model:     config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-5"},
		Variant:   "deep",
		Reasoning: &level,
	}
	require.NoError(t, config.SaveSelectionWithOptions(path, selection, config.TUIConfigSaveOptions{}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "theme = \"nord\" # keep")
	assert.Contains(t, content, "context_window = 128000")
	assert.Contains(t, content, "reasoning_level = \"high\"")
}
