package config_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSubagentPresets(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[subagents.models]
reviewer = "openai/gpt-5"

[subagents.roles.reviewer]
description = "Reviews code"
default_capability_mode = "read-only"
model = "openai/gpt-5"
reasoning_effort = "high"
prompt_file = "reviewer.md"
default_isolation = "none"

[subagents.personas.scout]
description = "Reconnaissance"
instructions = "Report only what you observed."
model = "openai/gpt-5-mini"
reasoning_effort = "low"

[[subagents.personas.scout.outputs]]
name = "summary"
description = "One paragraph"
`)
	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	subagent := result.Config.Subagent
	assert.Equal(t, map[string]string{"reviewer": "openai/gpt-5"}, subagent.Models)

	role, ok := subagent.Roles["reviewer"]
	require.True(t, ok)
	assert.Equal(t, "Reviews code", role.Description)
	assert.Equal(t, "read-only", role.DefaultCapabilityMode)
	assert.Equal(t, "high", role.ReasoningEffort)
	assert.Equal(t, "reviewer.md", role.PromptFile)
	assert.Equal(t, "none", role.DefaultIsolation)

	persona, ok := subagent.Personas["scout"]
	require.True(t, ok)
	assert.Equal(t, "Report only what you observed.", persona.Instructions)
	require.Len(t, persona.Outputs, 1)
	assert.Equal(t, "summary", persona.Outputs[0].Name)

	for _, field := range []config.Field{
		config.FieldSubagentModels, config.FieldSubagentRoles, config.FieldSubagentPersonas,
	} {
		source, ok := result.Config.Source(field)
		require.True(t, ok, field)
		assert.Equal(t, config.SourceConfigFile, source.Kind, field)
		assert.Equal(t, path, source.Detail, field)
	}
}

func TestLoadSubagentPresetDefaultsAreEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents]\nmax_concurrent = 2\n")

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Empty(t, result.Config.Subagent.Roles)
	assert.Empty(t, result.Config.Subagent.Personas)
	assert.Empty(t, result.Config.Subagent.Models)
	source, ok := result.Config.Source(config.FieldSubagentRoles)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)
}

func TestLoadSubagentRoleRequiresDescription(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents.roles.reviewer]\nmodel = \"openai/gpt-5\"\n")

	_, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.ErrorIs(t, err, config.ErrInvalid)
}

func TestLoadSubagentPersonaRequiresIOFieldDescription(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents.personas.scout]\ninstructions = \"x\"\n\n[[subagents.personas.scout.inputs]]\nname = \"target\"\n")

	_, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.ErrorIs(t, err, config.ErrInvalid)
}

func TestLoadSubagentPresetsRejectUnknownKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[subagents.roles.reviewer]\ndescription = \"d\"\ncolour = \"red\"\n")

	_, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.ErrorIs(t, err, config.ErrDecode)
}

func TestSubagentConfigIsZeroDetectsLegacyBoundary(t *testing.T) {
	t.Parallel()

	assert.True(t, config.SubagentConfig{}.IsZero())
	assert.False(t, config.DefaultSubagentConfig().IsZero())
	assert.False(t, config.SubagentConfig{Models: map[string]string{"a": "b"}}.IsZero())
}
