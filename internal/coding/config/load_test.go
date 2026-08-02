//nolint:wsl_v5 // File safety setup and assertions stay adjacent.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRegistryAndSelectionPrecedence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
variant = "balanced"
reasoning = "medium"
tool_search = true
mode = "plan"

[sandbox_workspace_write]
network = "allow"

[compaction]
enabled = true
reserve_tokens = 32768
keep_recent_tokens = 24000
summary_max_tokens = 4096

[providers.local]
protocol = "openai/chat_completions"
base_url = "http://127.0.0.1:11434/v1"
allow_http = true
allow_private_ips = true

[providers.local.compatibility]
max_tokens_field = "max_tokens"

[providers.openai.models."gpt-file"]
context_window = 200000
reasoning_levels = ["low", "medium", "high"]
default_reasoning_level = "medium"
default_variant = "balanced"

[providers.openai.models."gpt-file".reasoning_budgets]
high = 16000

[providers.openai.models."gpt-file".compatibility]
stream_usage = "omit"

[providers.openai.models."gpt-file".request]
max_output_tokens = 4096
temperature = 0.2
stop = ["END"]

[providers.openai.models."gpt-file".request.extra_body]
service_tier = "flex"

[providers.openai.models."gpt-file".variants.balanced]
reasoning_level = "medium"

[providers.openai.models."gpt-file".variants.balanced.request]
max_output_tokens = 8192
`)

	flagRef := config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"}
	flagVariant := "deep"
	flagReasoning := config.ReasoningLevel("high")
	flagMode := config.ModePlan
	result, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv: mapLookup(map[string]string{
			config.ModelEnv: "gemini/gemini-env",
			config.ModeEnv:  "agent",
		}),
		FlagOverrides: config.Patch{
			Model: &flagRef, Variant: &flagVariant, Reasoning: &flagReasoning, Mode: &flagMode,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, flagRef, result.Config.Model)
	assert.Equal(t, flagVariant, result.Config.Variant)
	assert.Equal(t, flagReasoning, *result.Config.Reasoning)
	assert.Equal(t, config.ModePlan, result.Config.Mode)
	assert.Equal(t, config.SandboxNetworkAllow, result.Config.SandboxWorkspaceWrite.Network)
	assert.Equal(t, config.FileStateLoaded, result.ConfigFile.State)
	require.Len(t, result.Config.Models, 1)
	assert.Equal(t, 200000, result.Config.Models[0].ContextWindow)
	assert.Equal(t, 4096, *result.Config.Models[0].Options.MaxOutputTokens)
	assert.InDelta(t, 0.2, *result.Config.Models[0].Options.Temperature, 1e-9)
	assert.Equal(t, 8192, *result.Config.Models[0].Variants["balanced"].Options.MaxOutputTokens)
	assert.Equal(t, 16000, result.Config.Models[0].ReasoningBudgets["high"])
	assert.Equal(t, openai.StreamUsageOmit, *result.Config.Models[0].Compatibility.StreamUsage)
	assert.Equal(t, config.CompactionConfig{
		Enabled: true, ReserveTokens: 32768, KeepRecentTokens: 24000, SummaryMaxTokens: 4096,
	}, result.Config.Compaction)
	assert.Equal(
		t,
		openai.MaxTokensFieldLegacy,
		*result.Config.Providers["local"].Compatibility.MaxTokensField,
	)
	assert.Equal(
		t,
		config.ProtocolOpenAIChatCompletions,
		result.Config.Providers["local"].Protocol,
	)

	source, ok := result.Config.Source(config.FieldModel)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, source.Kind)
	assert.Equal(t, "--model", source.Detail)
	modeSource, ok := result.Config.Source(config.FieldMode)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, modeSource.Kind)
	assert.Equal(t, "--mode", modeSource.Detail)

	targetDefaults, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv: mapLookup(map[string]string{
			config.ModelEnv: "anthropic/claude",
			config.ModeEnv:  "agent",
		}),
	})
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude", targetDefaults.Config.Model.String())
	assert.Empty(t, targetDefaults.Config.Variant)
	assert.Nil(t, targetDefaults.Config.Reasoning)
	assert.Equal(t, config.ModeAgent, targetDefaults.Config.Mode)
	variantSource, ok := targetDefaults.Config.Source(config.FieldVariant)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, variantSource.Kind)
	assert.Equal(t, config.ModelEnv, variantSource.Detail)
	modeSource, ok = targetDefaults.Config.Source(config.FieldMode)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, modeSource.Kind)
	assert.Equal(t, config.ModeEnv, modeSource.Detail)
}

func TestLoadInfersAndSortsNestedModels(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.zeta]
protocol = "openai/chat_completions"
base_url = "https://zeta.example/v1"

[providers.zeta.models."org/model-b"]

[providers.zeta.models."model-a"]
default = true

[providers.alpha.models."model-c"]
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, "zeta/model-a", result.Config.Model.String())
	require.Len(t, result.Config.Models, 3)
	assert.Equal(t, "alpha/model-c", result.Config.Models[0].Ref.String())
	assert.Equal(t, "zeta/model-a", result.Config.Models[1].Ref.String())
	assert.Equal(t, "zeta/org/model-b", result.Config.Models[2].Ref.String())
}

func TestLoadInfersOnlyNestedModel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.opencode-go]
protocol = "openai/chat_completions"
base_url = "https://opencode.ai/zen/go/v1"

[providers.opencode-go.models."deepseek-v4-flash"]
context_window = 1000000
request.max_output_tokens = 65536
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, "opencode-go/deepseek-v4-flash", result.Config.Model.String())
	require.Len(t, result.Config.Models, 1)
	assert.Equal(t, 1000000, result.Config.Models[0].ContextWindow)
	assert.Equal(t, 65536, *result.Config.Models[0].Options.MaxOutputTokens)
}

func TestLoadMultipleModelsRequireDefaultOrProcessSelection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai.models.one]
[providers.openai.models.two]
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Empty(t, result.Config.Model.String())
	err = result.Config.ValidateRuntime()
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "default = true")

	result, err = config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.ModelEnv: "openai/two"}),
	})
	require.NoError(t, err)
	assert.Equal(t, "openai/two", result.Config.Model.String())
}

func TestLoadStructuredReasoningHistoryModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode openai.ReasoningHistoryField
	}{
		{name: "openrouter details", mode: openai.ReasoningHistoryDetails},
		{name: "mistral content chunks", mode: openai.ReasoningHistoryContentChunks},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, `
[providers.local]
protocol = "openai/chat_completions"
base_url = "https://example.com/v1"

[providers.local.compatibility]
reasoning_history = "`+string(tc.mode)+`"

[providers.local.models.reasoner]
default = true
`)

			result, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.NoError(t, err)
			require.NotNil(t, result.Config.Providers["local"].Compatibility.ReasoningHistory)
			assert.Equal(t, tc.mode, *result.Config.Providers["local"].Compatibility.ReasoningHistory)
		})
	}
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
			name:     "legacy model selector",
			content:  "model = \"openai/gpt\"\n",
			want:     config.ErrMigration,
			wantText: "default = true",
		},
		{name: "legacy flat models", content: "[[models]]\nid = \"openai/gpt\"\n", want: config.ErrMigration},
		{
			name:    "legacy provider api",
			content: "[providers.openai]\napi = \"responses\"\n",
			want:    config.ErrMigration, wantText: "protocol",
		},
		{
			name:    "legacy model api",
			content: "[providers.openai.models.gpt]\napi = \"responses\"\n",
			want:    config.ErrMigration, wantText: "protocol",
		},
		{
			name:    "legacy model options",
			content: "[providers.openai.models.gpt.options]\nmax_output_tokens = 8192\n",
			want:    config.ErrMigration, wantText: "request",
		},
		{
			name:    "legacy variant request fields",
			content: "[providers.openai.models.gpt.variants.deep]\nmax_output_tokens = 8192\n",
			want:    config.ErrMigration, wantText: "under request",
		},
		{name: "unknown top level", content: "api_key = \"secret\"\n", want: config.ErrDecode},
		{name: "unknown compaction", content: "[compaction]\ntypo = true\n", want: config.ErrDecode},
		{
			name:    "unknown workspace sandbox setting",
			content: "[sandbox_workspace_write]\ntypo = true\n",
			want:    config.ErrDecode,
		},
		{
			name:    "invalid workspace sandbox network",
			content: "[sandbox_workspace_write]\nnetwork = \"always\"\n",
			want:    config.ErrInvalid,
		},
		{name: "invalid compaction", content: "[compaction]\nsummary_max_tokens = 30000\n", want: config.ErrInvalid},
		{
			name:    "invalid reasoning history",
			content: "[providers.local.compatibility]\nreasoning_history = \"future\"\n",
			want:    config.ErrInvalid,
		},
		{name: "unknown request", content: "[providers.openai.models.gpt.request]\ntypo = true\n", want: config.ErrDecode},
		{name: "duplicate nested model", content: "[providers.openai.models.gpt]\n[providers.openai.models.gpt]\n", want: config.ErrDecode},
		{name: "multiple defaults", content: "[providers.openai.models.one]\ndefault = true\n[providers.openai.models.two]\ndefault = true\n", want: config.ErrInvalid},
		{name: "invalid default variant", content: "[providers.openai.models.gpt]\ndefault_variant = \"missing\"\n", want: config.ErrInvalid},
		{name: "credential shaped extra", content: "[providers.openai.models.gpt.request.extra_body]\napi_key = \"secret\"\n", want: config.ErrInvalid},
		{name: "datetime extra", content: "[providers.openai.models.gpt.request.extra_body]\ncreated_at = 2026-07-21T12:00:00Z\n", want: config.ErrInvalid},
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
