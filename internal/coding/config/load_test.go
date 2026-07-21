//nolint:wsl_v5 // File safety setup and assertions stay adjacent.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRegistryAndSelectionPrecedence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
model = "openai/gpt-file"
variant = "balanced"
reasoning = "medium"
tool_search = true

[providers.local]
api = "chat_completions"
base_url = "http://127.0.0.1:11434/v1"
allow_http = true
allow_private_ips = true

[[models]]
id = "openai/gpt-file"
context_window = 200000
reasoning_levels = ["low", "medium", "high"]
default_reasoning_level = "medium"
default_variant = "balanced"

[models.options]
max_output_tokens = 4096
temperature = 0.2
stop = ["END"]

[models.options.extra_body]
service_tier = "flex"

[models.variants.balanced]
reasoning_level = "medium"
max_output_tokens = 8192
`)

	flagRef := config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"}
	flagVariant := "deep"
	flagReasoning := config.ReasoningLevel("high")
	result, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv: mapLookup(map[string]string{
			config.ModelEnv: "gemini/gemini-env",
		}),
		FlagOverrides: config.Patch{
			Model: &flagRef, Variant: &flagVariant, Reasoning: &flagReasoning,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, flagRef, result.Config.Model)
	assert.Equal(t, flagVariant, result.Config.Variant)
	assert.Equal(t, flagReasoning, *result.Config.Reasoning)
	assert.Equal(t, config.FileStateLoaded, result.ConfigFile.State)
	require.Len(t, result.Config.Models, 1)
	assert.Equal(t, 200000, result.Config.Models[0].ContextWindow)
	assert.Equal(t, 4096, *result.Config.Models[0].Options.MaxOutputTokens)
	assert.InDelta(t, 0.2, *result.Config.Models[0].Options.Temperature, 1e-9)
	assert.Equal(t, 8192, *result.Config.Models[0].Variants["balanced"].Options.MaxOutputTokens)
	assert.Equal(t, config.APIChatCompletions, result.Config.Providers["local"].API)

	source, ok := result.Config.Source(config.FieldModel)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, source.Kind)
	assert.Equal(t, "--model", source.Detail)

	targetDefaults, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv: mapLookup(map[string]string{
			config.ModelEnv: "anthropic/claude",
		}),
	})
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude", targetDefaults.Config.Model.String())
	assert.Empty(t, targetDefaults.Config.Variant)
	assert.Nil(t, targetDefaults.Config.Reasoning)
	variantSource, ok := targetDefaults.Config.Source(config.FieldVariant)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, variantSource.Kind)
	assert.Equal(t, config.ModelEnv, variantSource.Detail)
}

func TestLoadRejectsLegacyAndInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		want     error
		wantText string
	}{
		{name: "legacy model table", content: "[model]\nprovider = \"openai\"\nid = \"gpt\"\n", want: config.ErrMigration},
		{
			name:     "removed model output limit",
			content:  "[[models]]\nid = \"openai/gpt\"\nmax_output_tokens = 8192\n",
			want:     config.ErrMigration,
			wantText: "move the value to [models.options]",
		},
		{name: "unknown top level", content: "api_key = \"secret\"\n", want: config.ErrDecode},
		{name: "unknown option", content: "[[models]]\nid = \"openai/gpt\"\n[models.options]\ntypo = true\n", want: config.ErrDecode},
		{name: "duplicate model", content: "[[models]]\nid = \"openai/gpt\"\n[[models]]\nid = \"openai/gpt\"\n", want: config.ErrInvalid},
		{name: "invalid default variant", content: "[[models]]\nid = \"openai/gpt\"\ndefault_variant = \"missing\"\n", want: config.ErrInvalid},
		{name: "credential shaped extra", content: "[[models]]\nid = \"openai/gpt\"\n[models.options.extra_body]\napi_key = \"secret\"\n", want: config.ErrInvalid},
		{name: "datetime extra", content: "[[models]]\nid = \"openai/gpt\"\n[models.options.extra_body]\ncreated_at = 2026-07-21T12:00:00Z\n", want: config.ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.content)
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.ErrorIs(t, err, tt.want)
			if tt.wantText != "" {
				assert.ErrorContains(t, err, tt.wantText)
			}
		})
	}
}

func TestLoadRejectsRemovedEnvironment(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"PIPS_PROVIDER", "PIPS_MODEL_API"} {
		_, err := config.Load(config.LoadOptions{LookupEnv: mapLookup(map[string]string{name: "value"})})
		require.ErrorIs(t, err, config.ErrMigration)
		assert.Contains(t, err.Error(), name)
	}
}

func TestLoadMissingUnsafeAndSymlinkFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	result, err := config.Load(config.LoadOptions{ConfigFile: filepath.Join(dir, "missing.toml")})
	require.NoError(t, err)
	assert.Equal(t, config.FileStateAbsent, result.ConfigFile.State)

	_, err = config.Load(config.LoadOptions{ConfigFile: dir})
	require.ErrorIs(t, err, config.ErrFile)

	oversized := filepath.Join(dir, "oversized.toml")
	writeFile(t, oversized, strings.Repeat("#", (1<<20)+1))
	_, err = config.Load(config.LoadOptions{ConfigFile: oversized})
	require.ErrorIs(t, err, config.ErrFile)

	target := filepath.Join(dir, "target.toml")
	link := filepath.Join(dir, "link.toml")
	writeFile(t, target, "tool_search = true\n")
	require.NoError(t, os.Symlink(target, link))
	result, err = config.Load(config.LoadOptions{ConfigFile: link})
	require.NoError(t, err)
	assert.True(t, result.Config.ToolSearch)
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
