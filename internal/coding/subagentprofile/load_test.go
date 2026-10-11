//nolint:wsl_v5 // Preset fixture construction includes filesystem-mode cases that remain serial.
package subagentprofile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type presetFixture struct {
	layout    paths.Layout
	workspace workspace.Workspace
	tree      *workspace.Tree
}

func newPresetFixture(t *testing.T) presetFixture {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return presetFixture{layout: layout, workspace: ws, tree: tree}
}

func writePreset(t *testing.T, directory, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600))
}

func (f presetFixture) userDir(kind string) string { return filepath.Join(f.layout.Root(), kind) }

func (f presetFixture) projectDir(kind string) string {
	return filepath.Join(f.workspace.Root(), paths.ProjectRoot(), kind)
}

func (f presetFixture) load(t *testing.T, options Options) Registry {
	t.Helper()
	options.Paths = f.layout
	options.Tree = f.tree
	if options.Limits == (Limits{}) {
		options.Limits = DefaultLimits()
	}
	registry, err := Load(t.Context(), options)
	require.NoError(t, err)

	return registry
}

func TestLoadResolvesPrecedenceWithInlineWinning(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	writePreset(t, fixture.userDir("roles"), "reviewer.toml", "description = \"user\"\nmodel = \"user/model\"\n")
	writePreset(t, fixture.projectDir("roles"), "reviewer.toml", "description = \"project\"\nmodel = \"project/model\"\n")

	registry := fixture.load(t, Options{
		ProjectTrusted: true,
		InlineRoles: map[string]config.SubagentRole{
			"reviewer": {Description: "inline", Model: "inline/model"},
		},
	})

	role, ok := registry.Role("reviewer")
	require.True(t, ok)
	assert.Equal(t, "inline", role.Description)
	assert.Equal(t, "inline/model", role.Model)
	assert.Equal(t, config.SubagentRoleSourceInline+"/reviewer", role.Source)

	codes := make([]string, 0, 2)
	for _, diagnostic := range registry.Diagnostics() {
		codes = append(codes, diagnostic.Code)
	}
	assert.Equal(t, []string{"role_suppressed", "role_suppressed"}, codes)
}

func TestLoadResolvesRolePromptAndPersonaInstructions(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	roles := fixture.userDir("roles")
	writePreset(t, roles, "reviewer.toml", "description = \"reviews\"\nprompt_file = \"reviewer.md\"\n")
	writePreset(t, roles, "reviewer.md", "Be terse and evidence-backed.\n")

	personas := fixture.userDir("personas")
	writePreset(t, personas, "scout.toml", `description = "recon"
instructions = "Inline guidance."
instructions_file = "scout.md"

[[inputs]]
name = "target"
description = "What to inspect"
`)
	writePreset(t, personas, "scout.md", "File guidance.\n")

	registry := fixture.load(t, Options{})

	role, ok := registry.Role("reviewer")
	require.True(t, ok)
	assert.Equal(t, "Be terse and evidence-backed.\n", role.Prompt)

	persona, ok := registry.Persona("scout")
	require.True(t, ok)
	assert.Equal(t, "Inline guidance.\n\nFile guidance.\n", persona.Reminder)
	require.Len(t, persona.Inputs, 1)
	assert.Equal(t, "target", persona.Inputs[0].Name)
	assert.Equal(t, "file", persona.Inputs[0].Type)
	assert.Empty(t, registry.Diagnostics())
}

func TestLoadSkipsProjectPresetsBeforeTrust(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	writePreset(t, fixture.projectDir("roles"), "reviewer.toml", "description = \"project\"\n")
	writePreset(t, fixture.projectDir("personas"), "scout.toml", "instructions = \"project\"\n")

	registry := fixture.load(t, Options{ProjectTrusted: false})

	_, ok := registry.Role("reviewer")
	assert.False(t, ok)
	_, ok = registry.Persona("scout")
	assert.False(t, ok)
}

func TestLoadRejectsInvalidDeclarationsWithoutFailingTheLoad(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	roles := fixture.userDir("roles")
	writePreset(t, roles, "no-description.toml", "model = \"m\"\n")
	writePreset(t, roles, "bad-mode.toml", "description = \"d\"\ndefault_capability_mode = \"root\"\n")
	writePreset(t, roles, "bad-effort.toml", "description = \"d\"\nreasoning_effort = \"turbo\"\n")
	writePreset(t, roles, "bad-isolation.toml", "description = \"d\"\ndefault_isolation = \"container\"\n")
	writePreset(t, roles, "unknown-key.toml", "description = \"d\"\ncolour = \"red\"\n")
	writePreset(t, roles, "BadName.toml", "description = \"d\"\n")
	writePreset(t, roles, "escaping.toml", "description = \"d\"\nprompt_file = \"../secret.md\"\n")
	writePreset(t, fixture.userDir("personas"), "bad-io.toml", "[[outputs]]\nname = \"\"\ndescription = \"d\"\n")

	registry := fixture.load(t, Options{})

	for _, name := range []string{
		"no-description", "bad-mode", "bad-effort", "bad-isolation", "unknown-key", "BadName", "escaping",
	} {
		_, ok := registry.Role(name)
		assert.False(t, ok, name)
	}
	_, ok := registry.Persona("bad-io")
	assert.False(t, ok)
	assert.Len(t, registry.Diagnostics(), 8)
}

func TestLoadRejectsInsecureUserRoot(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	directory := fixture.userDir("roles")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	// The test deliberately makes the user root group/other writable.
	require.NoError(t, os.Chmod(directory, 0o777))       //nolint:gosec // G302: intentional insecure root.
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) }) //nolint:gosec // G302: restore the fixture mode.

	_, err := Load(t.Context(), Options{
		Paths: fixture.layout, Tree: fixture.tree, Limits: DefaultLimits(),
	})
	require.ErrorIs(t, err, ErrInsecure)
}

func TestLoadResolvesInlinePromptFileInsideWorkspace(t *testing.T) {
	t.Parallel()

	fixture := newPresetFixture(t)
	writePreset(t, fixture.workspace.Root(), "role-notes.md", "Inline role body.\n")

	registry := fixture.load(t, Options{
		InlineRoles: map[string]config.SubagentRole{
			"reviewer": {Description: "reviews", PromptFile: "role-notes.md"},
		},
	})

	role, ok := registry.Role("reviewer")
	require.True(t, ok)
	assert.Equal(t, "Inline role body.\n", role.Prompt)
}

func TestLoadRejectsMissingWorkspaceTree(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	_, err = Load(t.Context(), Options{Paths: layout, Limits: DefaultLimits()})
	require.ErrorIs(t, err, ErrInvalid)
}
