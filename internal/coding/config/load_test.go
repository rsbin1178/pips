//nolint:wsl_v5 // File safety setup and assertions stay adjacent.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/statusline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadThemeSelectionDefaultsAndFileSource(t *testing.T) {
	t.Parallel()

	result, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, config.ThemeAuto, result.Config.TUI.Theme)
	assert.Equal(t, statusline.Default(), result.Config.TUI.StatusLine)
	statusSource, ok := result.Config.Source(config.FieldStatusLine)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, statusSource.Kind)
	source, ok := result.Config.Source(config.FieldTheme)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"dracula\"\n")
	result, err = config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "dracula", result.Config.TUI.Theme)
	source, ok = result.Config.Source(config.FieldTheme)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)
}

func TestLoadDynamicSubagentsGatePrecedenceAndProvenance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "dynamic_subagents = true\n")
	result, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.DynamicSubagentsEnv: "false"}),
	})
	require.NoError(t, err)
	assert.False(t, result.Config.DynamicSubagents)
	source, ok := result.Config.Source(config.FieldDynamicSubagents)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.DynamicSubagentsEnv, source.Detail)

	enabled := true
	result, err = config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.DynamicSubagentsEnv: "false"}),
		FlagOverrides: config.Patch{
			DynamicSubagents: &enabled,
		},
	})
	require.NoError(t, err)
	assert.True(t, result.Config.DynamicSubagents)
	source, ok = result.Config.Source(config.FieldDynamicSubagents)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, source.Kind)
	assert.Equal(t, "--dynamic-subagents", source.Detail)
}

func TestLoadSubagentsEnabledSpellingMergesWithTopLevelSwitch(t *testing.T) {
	t.Parallel()

	// A `[subagents]` table that only tunes limits never turns subagents off.
	tuningOnly := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, tuningOnly, "[subagents]\nmax_concurrent = 4\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: tuningOnly})
	require.NoError(t, err)
	assert.True(t, result.Config.DynamicSubagents)
	assert.Equal(t, 4, result.Config.Subagent.MaxConcurrent)
	for _, field := range []config.Field{config.FieldDynamicSubagents, config.FieldSubagentEnabled} {
		source, ok := result.Config.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceDefault, source.Kind)
	}

	// The table spelling is the same switch, so it reports the same value and
	// the same deciding tier as the top-level spelling.
	tableSpelling := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, tableSpelling, "[subagents]\nenabled = false\n")
	result, err = config.Load(config.LoadOptions{ConfigFile: tableSpelling})
	require.NoError(t, err)
	assert.False(t, result.Config.DynamicSubagents)
	for _, field := range []config.Field{config.FieldDynamicSubagents, config.FieldSubagentEnabled} {
		source, ok := result.Config.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceConfigFile, source.Kind)
		assert.Equal(t, tableSpelling, source.Detail)
	}

	// An agreeing pair is one setting written twice, not a contradiction.
	agreeing := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, agreeing, "dynamic_subagents = false\n\n[subagents]\nenabled = false\n")
	result, err = config.Load(config.LoadOptions{ConfigFile: agreeing})
	require.NoError(t, err)
	assert.False(t, result.Config.DynamicSubagents)

	// Disagreeing spellings are one layer contradicting itself.
	disagreeing := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, disagreeing, "dynamic_subagents = false\n\n[subagents]\nenabled = true\n")
	_, err = config.Load(config.LoadOptions{ConfigFile: disagreeing})
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "dynamic_subagents")
	require.ErrorContains(t, err, "[subagents] enabled")

	// Environment stays above both file spellings.
	result, err = config.Load(config.LoadOptions{
		ConfigFile: agreeing,
		LookupEnv:  mapLookup(map[string]string{config.DynamicSubagentsEnv: "true"}),
	})
	require.NoError(t, err)
	assert.True(t, result.Config.DynamicSubagents)
	source, ok := result.Config.Source(config.FieldSubagentEnabled)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.DynamicSubagentsEnv, source.Detail)
}

func TestLoadNoSubagentsFlagSpellingAndConflict(t *testing.T) {
	t.Parallel()

	// `--no-subagents` resolves the switch to false and reports its own name.
	disabled := true
	result, err := config.Load(config.LoadOptions{
		FlagOverrides: config.Patch{NoSubagents: &disabled},
	})
	require.NoError(t, err)
	assert.False(t, result.Config.DynamicSubagents)
	source, ok := result.Config.Source(config.FieldDynamicSubagents)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, source.Kind)
	assert.Equal(t, "--no-subagents", source.Detail)
	source, ok = result.Config.Source(config.FieldSubagentEnabled)
	require.True(t, ok)
	assert.Equal(t, "--no-subagents", source.Detail)

	// An explicit `--no-subagents=false` asks for the switch to stay on.
	enabled := false
	result, err = config.Load(config.LoadOptions{
		FlagOverrides: config.Patch{NoSubagents: &enabled},
	})
	require.NoError(t, err)
	assert.True(t, result.Config.DynamicSubagents)

	// Both flag spellings may be passed when they agree.
	disabledFlag := true
	off := false
	result, err = config.Load(config.LoadOptions{
		FlagOverrides: config.Patch{DynamicSubagents: &off, NoSubagents: &disabledFlag},
	})
	require.NoError(t, err)
	assert.False(t, result.Config.DynamicSubagents)
	source, ok = result.Config.Source(config.FieldDynamicSubagents)
	require.True(t, ok)
	assert.Equal(t, "--dynamic-subagents", source.Detail)

	// Two explicit flag spellings that disagree are refused, and the flag tier
	// stays above a contradicting file.
	on := true
	contradictingFile := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, contradictingFile, "[subagents]\nenabled = false\n")
	_, err = config.Load(config.LoadOptions{
		ConfigFile:    contradictingFile,
		FlagOverrides: config.Patch{DynamicSubagents: &on, NoSubagents: &disabledFlag},
	})
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "--dynamic-subagents")
	require.ErrorContains(t, err, "--no-subagents")

	result, err = config.Load(config.LoadOptions{
		ConfigFile:    contradictingFile,
		FlagOverrides: config.Patch{DynamicSubagents: &on},
	})
	require.NoError(t, err)
	assert.True(t, result.Config.DynamicSubagents)
}

func TestLoadSubagentToggleTable(t *testing.T) {
	t.Parallel()

	// An absent table enables every type.
	result, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Config.Subagent.Toggle)
	assert.True(t, result.Config.Subagent.TypeEnabled("explore"))
	source, ok := result.Config.Source(config.FieldSubagentToggle)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents.toggle]\nexplore = false\ngo-checker = true\n")
	result, err = config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"explore": false, "go-checker": true}, result.Config.Subagent.Toggle)
	assert.False(t, result.Config.Subagent.TypeEnabled("explore"))
	assert.True(t, result.Config.Subagent.TypeEnabled("go-checker"))
	// An absent key means enabled, and the lookup is exact and case-sensitive.
	assert.True(t, result.Config.Subagent.TypeEnabled("plan"))
	assert.True(t, result.Config.Subagent.TypeEnabled("Explore"))
	source, ok = result.Config.Source(config.FieldSubagentToggle)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)

	// A table that only carries a toggle still keeps the other defaults.
	assert.Equal(t, config.DefaultSubagentConfig().MaxConcurrent, result.Config.Subagent.MaxConcurrent)
}

func TestLoadActiveAgentMessagesGatePrecedenceAndProvenance(t *testing.T) {
	t.Parallel()

	// The messaging gate is closed by default.
	result, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.False(t, result.Config.ActiveAgentMessages)
	source, ok := result.Config.Source(config.FieldActiveAgentMessages)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "active_agent_messages = true\n")
	result, err = config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.True(t, result.Config.ActiveAgentMessages)
	source, ok = result.Config.Source(config.FieldActiveAgentMessages)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)

	result, err = config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.ActiveAgentMessagesEnv: "false"}),
	})
	require.NoError(t, err)
	assert.False(t, result.Config.ActiveAgentMessages)
	source, ok = result.Config.Source(config.FieldActiveAgentMessages)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.ActiveAgentMessagesEnv, source.Detail)

	_, err = config.Load(config.LoadOptions{
		LookupEnv: mapLookup(map[string]string{config.ActiveAgentMessagesEnv: "sometimes"}),
	})
	require.ErrorIs(t, err, config.ErrInvalid)
}

func TestLoadToolSearchNamePrecedenceAndProviderOverride(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
tool_search_name = "file_search"

[providers.proxy]
protocol = "openai/responses"
base_url = "https://proxy.example/v1"
tool_search_name = "proxy_search"

[providers.proxy.models."gpt"]
`)

	defaults, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "pips_tool_search", defaults.Config.ToolSearchName)
	assert.Equal(t, "pips_tool_search", defaults.Config.ToolSearchNameFor("anything"))
	source, ok := defaults.Config.Source(config.FieldToolSearchName)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	fromFile, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "file_search", fromFile.Config.ToolSearchName)
	assert.Equal(t, "file_search", fromFile.Config.ToolSearchNameFor("openai"))
	assert.Equal(t, "proxy_search", fromFile.Config.ToolSearchNameFor("proxy"))
	assert.Equal(t, "proxy_search", fromFile.Config.Providers["proxy"].ToolSearchName)
	source, ok = fromFile.Config.Source(config.FieldToolSearchName)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	require.NoError(t, fromFile.Config.ValidateRuntime())

	fromEnv, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.ToolSearchNameEnv: " env_search "}),
	})
	require.NoError(t, err)
	assert.Equal(t, "env_search", fromEnv.Config.ToolSearchName)
	assert.Equal(t, "proxy_search", fromEnv.Config.ToolSearchNameFor("proxy"))
	source, ok = fromEnv.Config.Source(config.FieldToolSearchName)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.ToolSearchNameEnv, source.Detail)

	flagName := "flag-search"
	fromFlag, err := config.Load(config.LoadOptions{
		ConfigFile:    path,
		LookupEnv:     mapLookup(map[string]string{config.ToolSearchNameEnv: "env_search"}),
		FlagOverrides: config.Patch{ToolSearchName: &flagName},
	})
	require.NoError(t, err)
	assert.Equal(t, "flag-search", fromFlag.Config.ToolSearchName)
	source, ok = fromFlag.Config.Source(config.FieldToolSearchName)
	require.True(t, ok)
	assert.Equal(t, config.SourceFlag, source.Kind)
	assert.Equal(t, "--tool-search-name", source.Detail)
}

func TestLoadRejectsInvalidToolSearchName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, content := range map[string]string{
		"space":    "tool_search_name = \"tool search\"\n",
		"dot":      "tool_search_name = \"tool.search\"\n",
		"empty":    "tool_search_name = \"\"\n",
		"too-long": "tool_search_name = \"" + strings.Repeat("a", 65) + "\"\n",
		"provider": "[providers.proxy]\ntool_search_name = \"bad name\"\n",
	} {
		path := filepath.Join(dir, name+".toml")
		writeFile(t, path, content)
		_, err := config.Load(config.LoadOptions{ConfigFile: path})
		require.ErrorIs(t, err, config.ErrInvalid, name)
		require.ErrorContains(t, err, "tool_search_name", name)
	}

	_, err := config.Load(config.LoadOptions{
		LookupEnv: mapLookup(map[string]string{config.ToolSearchNameEnv: "bad/name"}),
	})
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, config.ToolSearchNameEnv)

	invalid := "bad name"
	_, err = config.Load(config.LoadOptions{FlagOverrides: config.Patch{ToolSearchName: &invalid}})
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "flags")
}

func TestLoadSubagentBudgetsAndFieldProvenance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[subagents]
max_depth = 2
max_concurrent = 6
`)
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, config.SubagentConfig{
		MaxDepth:      2,
		MaxConcurrent: 6,
		LimitBehavior: config.LimitBehaviorQueue,
	}, result.Config.Subagent)
	for _, field := range []config.Field{
		config.FieldSubagentMaxDepth,
		config.FieldSubagentMaxConcurrent,
	} {
		source, ok := result.Config.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceConfigFile, source.Kind)
		assert.Equal(t, path, source.Detail)
	}
}

// TestLoadSubagentRemovedKeysFailWithMigration pins the Phase L and Phase M
// migrations: the per-child execution keys and the two admission keys no longer
// exist, so a file that still carries one is named explicitly instead of
// loading or failing as a generic unknown field.
func TestLoadSubagentRemovedKeysFailWithMigration(t *testing.T) {
	t.Parallel()

	removed := []string{
		"max_turns", "max_tokens", "max_tool_calls", "max_duration_minutes",
		"max_spawned_per_root_interaction", "max_auto_follow_ups",
	}
	for _, key := range removed {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, "[subagents]\n"+key+" = 1\n")
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.ErrorIs(t, err, config.ErrMigration)
			assert.Contains(t, err.Error(), key)
		})
	}
}

func TestLoadSubagentPartialTableRetainsDefaultFieldSources(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents]\nmax_depth = 2\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Config.Subagent.MaxDepth)
	assert.Equal(t, config.DefaultSubagentConfig().MaxConcurrent, result.Config.Subagent.MaxConcurrent)
	source, ok := result.Config.Source(config.FieldSubagentMaxDepth)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	source, ok = result.Config.Source(config.FieldSubagentMaxConcurrent)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)
}

func TestLoadSubagentStructuredResults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents]\nstructured_results = true\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.True(t, result.Config.Subagent.StructuredResults)
	source, ok := result.Config.Source(config.FieldSubagentStructuredResults)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)

	// A partial table that omits the field keeps the free-text default.
	partial := filepath.Join(t.TempDir(), "partial.toml")
	writeFile(t, partial, "[subagents]\nmax_concurrent = 2\n")
	kept, err := config.Load(config.LoadOptions{ConfigFile: partial})
	require.NoError(t, err)
	assert.False(t, kept.Config.Subagent.StructuredResults)
	source, ok = kept.Config.Source(config.FieldSubagentStructuredResults)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)
}

// TestLoadTodoGate pins the opt-in gate's key: it defaults to off, and a config
// file that sets it wins and reports its own provenance.
func TestLoadTodoGate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "todo_gate = true\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.True(t, result.Config.TodoGate)
	source, ok := result.Config.Source(config.FieldTodoGate)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)

	// With no key the gate stays off with default provenance.
	empty := filepath.Join(t.TempDir(), "empty.toml")
	writeFile(t, empty, "[subagents]\nmax_concurrent = 2\n")
	kept, err := config.Load(config.LoadOptions{ConfigFile: empty})
	require.NoError(t, err)
	assert.False(t, kept.Config.TodoGate)
	source, ok = kept.Config.Source(config.FieldTodoGate)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	assert.Contains(t, config.Fields(), config.FieldTodoGate)
}

func TestLoadSubagentLimitBehaviorDefaultsAndSources(t *testing.T) {
	t.Parallel()

	// A partial table keeps the queue default with default provenance.
	partial := filepath.Join(t.TempDir(), "partial.toml")
	writeFile(t, partial, "[subagents]\nmax_concurrent = 2\n")
	kept, err := config.Load(config.LoadOptions{ConfigFile: partial})
	require.NoError(t, err)
	assert.Equal(t, config.LimitBehaviorQueue, kept.Config.Subagent.LimitBehavior)
	source, ok := kept.Config.Source(config.FieldSubagentLimitBehavior)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents]\nlimit_behavior = \"fail\"\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, config.LimitBehaviorFail, result.Config.Subagent.LimitBehavior)
	source, ok = result.Config.Source(config.FieldSubagentLimitBehavior)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)

	invalid := filepath.Join(t.TempDir(), "invalid.toml")
	writeFile(t, invalid, "[subagents]\nlimit_behavior = \"retry\"\n")
	_, err = config.Load(config.LoadOptions{ConfigFile: invalid})
	require.ErrorIs(t, err, config.ErrInvalid)
}

func TestLoadSubagentLimitBehaviorEnvPrecedence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents]\nlimit_behavior = \"fail\"\n")
	result, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.SubagentLimitBehaviorEnv: "queue"}),
	})
	require.NoError(t, err)
	assert.Equal(t, config.LimitBehaviorQueue, result.Config.Subagent.LimitBehavior)
	source, ok := result.Config.Source(config.FieldSubagentLimitBehavior)
	require.True(t, ok)
	assert.Equal(t, config.SourceEnvironment, source.Kind)
	assert.Equal(t, config.SubagentLimitBehaviorEnv, source.Detail)

	// An unrecognized value falls back to queue instead of failing the load.
	fallback, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{config.SubagentLimitBehaviorEnv: "retry"}),
	})
	require.NoError(t, err)
	assert.Equal(t, config.LimitBehaviorQueue, fallback.Config.Subagent.LimitBehavior)
}

func TestLoadSubagentBudgetsRejectInvalidAndUnknownValues(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"[subagents]\nmax_depth = 4\n",
		"[subagents]\nunknown = 1\n",
	} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, content)
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.Error(t, err)
		})
	}
}

func TestLoadStatusLineSelectionFileOverrideAndProvenance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"nord\"\nstatus_line = [\"workspace\", \"phase\", \"model\"]\n")

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, []statusline.Item{statusline.Workspace, statusline.Phase, statusline.Model}, result.Config.TUI.StatusLine)
	source, ok := result.Config.Source(config.FieldStatusLine)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	assert.Equal(t, path, source.Detail)
}

func TestLoadStatusLineSelectionPreservesExplicitEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\nstatus_line = []\n")

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.NotNil(t, result.Config.TUI.StatusLine)
	assert.Empty(t, result.Config.TUI.StatusLine)
	source, ok := result.Config.Source(config.FieldStatusLine)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
}

func TestLoadStatusLineSelectionRejectsUnknownAndDuplicateItems(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"[tui]\nstatus_line = [\"workspace\", \"unknown\"]\n",
		"[tui]\nstatus_line = [\"workspace\", \"workspace\"]\n",
	} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, content)
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.ErrorIs(t, err, config.ErrInvalid)
		})
	}
}

func TestLoadThemeSelectionEmptyNormalizesToAuto(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"\"\n")
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, config.ThemeAuto, result.Config.TUI.Theme)
	source, ok := result.Config.Source(config.FieldTheme)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
}

func TestLoadThemeSelectionIsFileOnly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[tui]\ntheme = \"nord\"\n")
	result, err := config.Load(config.LoadOptions{
		ConfigFile: path,
		LookupEnv:  mapLookup(map[string]string{"PIPS_THEME": "dracula"}),
	})
	require.NoError(t, err)
	assert.Equal(t, "nord", result.Config.TUI.Theme)
}

func TestLoadRejectsInvalidThemeSchema(t *testing.T) {
	t.Parallel()

	tests := []string{
		"[tui]\ntheme = \"Dracula\"\n",
		"[tui]\ntheme = \"theme_name\"\n",
		"[tui]\nunknown = true\n",
		"theme = \"dracula\"\n",
		"[tui]\ntheme = \"dracula\"\n[tui]\ntheme = \"nord\"\n",
	}
	for _, content := range tests {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, content)
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.Error(t, err)
		})
	}
}

func TestLoadReadOnlySandbox(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
sandbox = "read-only"

[providers.openai.models.gpt]
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, config.SandboxReadOnly, result.Config.Sandbox)
	assert.Equal(t, config.SandboxNetworkOnRequest, result.Config.SandboxWorkspaceWrite.Network)
	source, ok := result.Config.Source(config.FieldSandbox)
	require.True(t, ok)
	assert.Equal(t, config.SourceConfigFile, source.Kind)
	require.NoError(t, result.Config.ValidateRuntime())
}

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
	expectedCompaction := config.DefaultCompactionConfig()
	expectedCompaction.ReserveTokens = 32768
	expectedCompaction.KeepRecentTokens = 24000
	expectedCompaction.SummaryMaxTokens = 4096
	assert.Equal(t, expectedCompaction, result.Config.Compaction)
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

[providers.opencode-go.models."deepseek-v4.1-flash"]
context_window = 1000000
request.max_output_tokens = 65536
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, "opencode-go/deepseek-v4.1-flash", result.Config.Model.String())
	require.Len(t, result.Config.Models, 1)
	assert.Equal(t, 1000000, result.Config.Models[0].ContextWindow)
	assert.Equal(t, 65536, *result.Config.Models[0].Options.MaxOutputTokens)
}

func TestLoadStreamContinuationPerModel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai.models."flash"]
default = true
stream_continuation = true

[providers.openai.models."pro"]
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	require.Len(t, result.Config.Models, 2)
	byRef := make(map[string]config.ModelConfig, len(result.Config.Models))
	for _, model := range result.Config.Models {
		byRef[model.Ref.String()] = model
	}
	assert.True(t, byRef["openai/flash"].StreamContinuation)
	assert.False(t, byRef["openai/pro"].StreamContinuation,
		"an absent stream_continuation keeps the regenerate default")
}

func TestLoadStreamIdleTimeoutPerProviderAndModel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai]
stream_idle_timeout_seconds = 900

[providers.openai.models."slow"]
default = true
stream_idle_timeout_seconds = 1800

[providers.openai.models."inherits"]
`)
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, 15*time.Minute, result.Config.Providers[ai.ProviderOpenAI].StreamIdleTimeout)

	byRef := make(map[string]config.ModelConfig, len(result.Config.Models))
	for _, model := range result.Config.Models {
		byRef[model.Ref.String()] = model
	}
	assert.Equal(t, 30*time.Minute, byRef["openai/slow"].StreamIdleTimeout,
		"the model's own bound wins over the provider's")
	assert.Zero(t, byRef["openai/inherits"].StreamIdleTimeout,
		"an absent key declares nothing and inherits the provider's value later")
}

func TestLoadRejectsStreamIdleTimeoutOutOfRange(t *testing.T) {
	t.Parallel()

	// The bound is a liveness guard, so an explicit zero is refused rather than
	// read as "not declared": it would switch the guard off.
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "provider zero",
			content: "[providers.openai]\nstream_idle_timeout_seconds = 0\n",
		},
		{
			name:    "provider negative",
			content: "[providers.openai]\nstream_idle_timeout_seconds = -1\n",
		},
		{
			name:    "provider above the ceiling",
			content: "[providers.openai]\nstream_idle_timeout_seconds = 3601\n",
		},
		{
			name:    "model zero",
			content: "[providers.openai.models.\"m\"]\nstream_idle_timeout_seconds = 0\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, test.content)

			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.ErrorIs(t, err, config.ErrInvalid)
			assert.Contains(t, err.Error(), "stream_idle_timeout_seconds")
		})
	}
}

func TestLoadUserAgentPerProviderAndModel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai]
user_agent = "gateway-probe/1"

[providers.openai.models."named"]
default = true
user_agent = "pips-coder"

[providers.openai.models."silent"]
user_agent_enabled = false

[providers.openai.models."inherits"]
`)
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, new("gateway-probe/1"), result.Config.Providers[ai.ProviderOpenAI].UserAgent)

	byRef := make(map[string]config.ModelConfig, len(result.Config.Models))
	for _, model := range result.Config.Models {
		byRef[model.Ref.String()] = model
	}

	assert.Equal(t, new("pips-coder"), byRef["openai/named"].UserAgent,
		"the model's own value wins over the provider's")
	assert.Equal(t, new(false), byRef["openai/silent"].UserAgentEnabled,
		"a model switches the header off on its own")
	assert.Nil(t, byRef["openai/inherits"].UserAgent, "an absent key declares nothing")
	assert.Nil(t, byRef["openai/inherits"].UserAgentEnabled, "an absent switch declares nothing")
}

func TestLoadRejectsUnusableUserAgent(t *testing.T) {
	t.Parallel()

	// "Send nothing" has its own switch, so an empty value is a mistake to
	// report rather than a second way of saying it, and declaring a value beside
	// a disabled switch is a contradiction rather than a precedence rule.
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "provider declares an empty value",
			content: "[providers.openai]\nuser_agent = \"\"\n",
			want:    "user_agent is empty",
		},
		{
			name:    "model declares a blank value",
			content: "[providers.openai.models.\"m\"]\nuser_agent = \"   \"\n",
			want:    "user_agent is empty",
		},
		{
			name:    "provider declares both fields",
			content: "[providers.openai]\nuser_agent_enabled = false\nuser_agent = \"x/1\"\n",
			want:    "user_agent_enabled is false",
		},
		{
			name:    "model declares both fields",
			content: "[providers.openai.models.\"m\"]\nuser_agent_enabled = false\nuser_agent = \"x/1\"\n",
			want:    "user_agent_enabled is false",
		},
		{
			name:    "a value carries a control character",
			content: "[providers.openai]\nuser_agent = \"probe\\u0001\"\n",
			want:    "unsupported characters",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, test.content)

			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.ErrorIs(t, err, config.ErrInvalid)
			assert.Contains(t, err.Error(), test.want)
		})
	}
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
			name:    "legacy subagent table",
			content: "[subagent]\nmax_concurrent = 2\n",
			want:    config.ErrMigration, wantText: "[subagents]",
		},
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
		{name: "invalid compaction", content: "[compaction]\nsummary_max_tokens = -1\n", want: config.ErrInvalid},
		{
			name:    "invalid reasoning history",
			content: "[providers.local.compatibility]\nreasoning_history = \"future\"\n",
			want:    config.ErrInvalid,
		},
		{name: "unknown request", content: "[providers.openai.models.gpt.request]\ntypo = true\n", want: config.ErrDecode},
		{
			name:    "unknown model stream continuation key",
			content: "[providers.openai.models.gpt]\nstream_continuations = true\n",
			want:    config.ErrDecode,
		},
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

func TestLoadMCPReadOnlyTools(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeFile(t, path, `
[mcp_read_only_tools]
exa = ["web_search_exa"]
docs = ["*"]
`)
	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		"exa": {"web_search_exa"}, "docs": {config.MCPReadOnlyWildcard},
	}, loaded.Config.MCPReadOnlyTools)

	cloned := loaded.Config.Clone()
	cloned.MCPReadOnlyTools["exa"][0] = "mutated"
	assert.Equal(t, "web_search_exa", loaded.Config.MCPReadOnlyTools["exa"][0])

	defaults, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.Empty(t, defaults.Config.MCPReadOnlyTools)

	for name, content := range map[string]string{
		"empty-list": "[mcp_read_only_tools]\nexa = []\n",
		"empty-name": "[mcp_read_only_tools]\nexa = [\"\"]\n",
		"empty-id":   "[mcp_read_only_tools]\n\"\" = [\"x\"]\n",
	} {
		invalid := filepath.Join(dir, name+".toml")
		writeFile(t, invalid, content)
		_, err := config.Load(config.LoadOptions{ConfigFile: invalid})
		require.ErrorIs(t, err, config.ErrInvalid, name)
		require.ErrorContains(t, err, "mcp_read_only_tools", name)
	}
}

// TestLoadDecodesThePromptCacheKeyCompatKnob pins the opt-in gate for endpoints
// that reject prompt_cache_key: an undeclared provider keeps the field off, and
// a gateway can turn it on from its own configuration.
func TestLoadDecodesThePromptCacheKeyCompatKnob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.toml")
	writeFile(t, absent, "[providers.openai.models.gpt]\n")

	loaded, err := config.Load(config.LoadOptions{ConfigFile: absent})
	require.NoError(t, err)
	require.Len(t, loaded.Config.Models, 1)
	assert.Nil(t, loaded.Config.Models[0].Compatibility.PromptCacheKey)

	declared := filepath.Join(dir, "declared.toml")
	writeFile(t, declared, `
[providers.local]
protocol = "openai/chat_completions"
base_url = "http://127.0.0.1:11434/v1"
allow_http = true
allow_private_ips = true

[providers.local.compatibility]
prompt_cache_key = true
`)

	loaded, err = config.Load(config.LoadOptions{ConfigFile: declared})
	require.NoError(t, err)

	provider, ok := loaded.Config.Providers[ai.Provider("local")]
	require.True(t, ok, "the declared provider must be registered")
	require.NotNil(t, provider.Compatibility.PromptCacheKey)
	assert.True(t, *provider.Compatibility.PromptCacheKey)
}
