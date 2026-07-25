//nolint:wsl_v5 // Resource fixtures and cross-scope assertions stay locally grouped.
package resource_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadResolvesScopedSkillPrecedenceAndSafeProvenance(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	writeSkill(t, fixture.layout.SkillsDir(), "review", "user instructions", true)
	writeSkill(
		t,
		filepath.Join(fixture.workspace.Root(), filepath.FromSlash(paths.ProjectSkillsDir())),
		"review",
		"project instructions",
		false,
	)

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Tree: fixture.tree, ProjectTrusted: true,
		Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, diagnostics, err := loaded.ResolveSkills(extension.SkillEntry{
		Origin: extension.Origin{ExtensionID: "compiled"},
		Skill: harness.Skill{
			Name: "review", Description: "Review code", Content: "extension instructions",
		},
	})
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "project instructions", skills[0].Content)
	assert.Equal(t, "project:pips/review/SKILL.md", skills[0].Source)
	assert.Empty(t, skills[0].AllowedTools)
	assert.NotContains(t, skills[0].Source, fixture.workspace.Root())
	require.Len(t, diagnostics, 2)
	assert.Equal(t, "project:pips/review/SKILL.md", diagnostics[0].Winner)
	assert.Equal(t, "project:pips/review/SKILL.md", diagnostics[1].Winner)

	// Returned Skills are owned snapshots.
	skills[0].Metadata["changed"] = "true"
	again, _, err := loaded.ResolveSkills()
	require.NoError(t, err)
	assert.NotContains(t, again[0].Metadata, "changed")
}

func TestLoadDiscoversSharedAgentSkillsWithStablePrecedence(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	shared := t.TempDir()
	var err error
	fixture.layout, err = fixture.layout.WithAgentSkillsDir(shared)
	require.NoError(t, err)

	writeSkill(t, shared, "review", "user agents", false)
	writeSkill(t, fixture.layout.SkillsDir(), "review", "user pips", false)
	writeSkill(
		t,
		filepath.Join(fixture.workspace.Root(), filepath.FromSlash(paths.ProjectAgentSkillsDir())),
		"review",
		"project agents",
		false,
	)
	writeSkill(
		t,
		filepath.Join(fixture.workspace.Root(), filepath.FromSlash(paths.ProjectSkillsDir())),
		"review",
		"project pips",
		false,
	)

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Tree: fixture.tree, ProjectTrusted: true,
		Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, diagnostics, err := loaded.ResolveSkills()
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "project pips", skills[0].Content)
	assert.Equal(t, "project:pips/review/SKILL.md", skills[0].Source)
	require.Len(t, diagnostics, 3)
	for _, diagnostic := range diagnostics {
		assert.Equal(t, "skill_suppressed", diagnostic.Code)
		assert.Equal(t, "project:pips/review/SKILL.md", diagnostic.Winner)
	}
}

func TestLoadIsolatesInvalidSkillAndReportsParserDiagnostics(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	writeSkill(t, fixture.layout.SkillsDir(), "valid", "valid instructions", false)
	writeRawSkill(t, fixture.layout.SkillsDir(), "invalid", "---\nname: invalid\n---\nmissing description\n")
	writeRawSkill(t, fixture.layout.SkillsDir(), "compatible", "---\ndescription: Compatible\ncontext: fork\n---\ncompatible instructions\n")

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, diagnostics, err := loaded.ResolveSkills()
	require.NoError(t, err)
	assert.Equal(t, []string{"compatible", "valid"}, loadedSkillNames(skills))
	assert.Contains(t, diagnosticCodes(diagnostics), "skill_invalid")
	assert.Contains(t, diagnosticCodes(diagnostics), "skill_name_defaulted")
	assert.Contains(t, diagnosticCodes(diagnostics), "skill_unsupported_field")
}

func TestLoadUntrustedProjectDoesNotInspectProjectResources(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: layout,
		// A nil Tree is intentionally valid only while the project is untrusted.
		ProjectTrusted: false,
		Limits:         resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, _, err := loaded.ResolveSkills()
	require.NoError(t, err)
	assert.Empty(t, skills)
}

func TestLoadDoesNotExecuteScriptsOrApplyAllowedTools(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	skillDir := writeSkill(t, fixture.layout.SkillsDir(), "review", "instructions", true)
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "scripts", "run.sh"),
		[]byte("exit 99\n"),
		0o600,
	))

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, _, err := loaded.ResolveSkills()
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Empty(t, skills[0].AllowedTools)
	assert.NotContains(t, skills[0].Content, "run.sh")
	require.Len(t, skills[0].Resources, 1)
	assert.Equal(t, "scripts/run.sh", skills[0].Resources[0].Path)
	assert.True(t, skills[0].Resources[0].Text)
}

func TestLoadSnapshotsBoundedSkillResourcesAndSkipsNestedSkills(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	parent := writeSkill(t, fixture.layout.SkillsDir(), "parent", "parent instructions", false)
	require.NoError(t, os.MkdirAll(filepath.Join(parent, "references"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "references", "guide.md"),
		[]byte("guide"),
		0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "binary.dat"), []byte{0xff, 0x00}, 0o600))
	writeSkill(t, parent, "child", "child instructions", false)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(parent, "linked.txt")))

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, diagnostics, err := loaded.ResolveSkills()
	require.NoError(t, err)
	require.Len(t, skills, 2)
	assert.Equal(t, []string{"child", "parent"}, loadedSkillNames(skills))

	parentSkill := skills[1]
	require.Len(t, parentSkill.Resources, 2)
	assert.Equal(t, "binary.dat", parentSkill.Resources[0].Path)
	assert.False(t, parentSkill.Resources[0].Text)
	assert.Empty(t, parentSkill.Resources[0].Content)
	assert.Equal(t, "references/guide.md", parentSkill.Resources[1].Path)
	assert.Equal(t, "guide", parentSkill.Resources[1].Content)
	assert.NotContains(t, resourcePaths(parentSkill.Resources), "child/SKILL.md")
	assert.Contains(t, diagnosticCodes(diagnostics), "skill_resource_ignored")
}

func TestLoadDoesNotFollowProjectSymlinkEscape(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	outside := t.TempDir()
	writeSkill(t, outside, "escaped", "outside instructions", false)

	projectSkills := filepath.Join(
		fixture.workspace.Root(),
		filepath.FromSlash(paths.ProjectSkillsDir()),
	)
	require.NoError(t, os.MkdirAll(projectSkills, 0o750))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, "escaped"),
		filepath.Join(projectSkills, "escaped"),
	))

	loaded, err := resource.Load(t.Context(), resource.Options{
		Paths: fixture.layout, Tree: fixture.tree, ProjectTrusted: true,
		Limits: resource.DefaultLimits(),
	})
	require.NoError(t, err)

	skills, _, err := loaded.ResolveSkills()
	require.NoError(t, err)
	assert.Empty(t, skills)
}

func TestLoadEnforcesDiscoveryAndContentLimits(t *testing.T) {
	t.Parallel()

	t.Run("manifest count", func(t *testing.T) {
		t.Parallel()

		fixture := newFixture(t)
		writeSkill(t, fixture.layout.SkillsDir(), "one", "one", false)
		writeSkill(t, fixture.layout.SkillsDir(), "two", "two", false)

		limits := resource.DefaultLimits()
		limits.MaxSkillManifests = 1
		_, err := resource.Load(t.Context(), resource.Options{Paths: fixture.layout, Limits: limits})
		require.ErrorIs(t, err, resource.ErrLimitExceeded)
	})

	t.Run("content bytes", func(t *testing.T) {
		t.Parallel()

		fixture := newFixture(t)
		writeSkill(t, fixture.layout.SkillsDir(), "large", strings.Repeat("x", 512), false)

		limits := resource.DefaultLimits()
		limits.MaxSkillBytes = 64
		_, err := resource.Load(t.Context(), resource.Options{Paths: fixture.layout, Limits: limits})
		require.ErrorIs(t, err, resource.ErrLimitExceeded)
	})
}

func TestLoadRejectsInsecureUserRoot(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.SkillsDir(), 0o750))
	require.NoError(t, os.Chmod(layout.SkillsDir(), 0o777)) //nolint:gosec // Deliberately unsafe fixture.

	_, err = resource.Load(t.Context(), resource.Options{
		Paths: layout, Limits: resource.DefaultLimits(),
	})
	require.ErrorIs(t, err, resource.ErrInsecure)
}

func TestLoadBundlesDeterministicallyAndRejectsCollisions(t *testing.T) {
	t.Parallel()

	t.Run("ordered", func(t *testing.T) {
		t.Parallel()

		fixture := newFixture(t)
		writeBundle(t, fixture.layout.BundlesDir(), "z-dir", "z-bundle", "z-ext")
		writeBundle(t, fixture.layout.BundlesDir(), "a-dir", "a-bundle", "a-ext")

		loaded, err := resource.Load(t.Context(), resource.Options{
			Paths: fixture.layout, Limits: resource.DefaultLimits(),
		})
		require.NoError(t, err)

		bundles := loaded.Bundles()
		require.Len(t, bundles, 2)
		assert.Equal(t, "a-bundle", bundles[0].Manifest().ID)
		assert.Equal(t, "z-bundle", bundles[1].Manifest().ID)
	})

	t.Run("duplicate extension", func(t *testing.T) {
		t.Parallel()

		fixture := newFixture(t)
		writeBundle(t, fixture.layout.BundlesDir(), "one", "one", "same-ext")
		writeBundle(t, fixture.layout.BundlesDir(), "two", "two", "same-ext")

		_, err := resource.Load(t.Context(), resource.Options{
			Paths: fixture.layout, Limits: resource.DefaultLimits(),
		})
		require.ErrorIs(t, err, resource.ErrDuplicate)
	})
}

type fixture struct {
	layout    paths.Layout
	workspace workspace.Workspace
	tree      *workspace.Tree
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	workspaceRoot := t.TempDir()
	opened, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return fixture{layout: layout, workspace: opened, tree: tree}
}

func writeSkill(
	t *testing.T,
	root string,
	name string,
	content string,
	allowedTools bool,
) string {
	t.Helper()

	directory := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(directory, 0o750))

	allowed := ""
	if allowedTools {
		allowed = "allowed-tools: [shell, read]\n"
	}

	manifest := fmt.Sprintf(
		"---\nname: %s\ndescription: Review code\n%smetadata:\n  owner: test\n---\n%s\n",
		name,
		allowed,
		content,
	)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(manifest), 0o600))

	return directory
}

func writeRawSkill(t *testing.T, root, directory, manifest string) string {
	t.Helper()

	skillDir := filepath.Join(root, directory)
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(manifest), 0o600))

	return skillDir
}

func loadedSkillNames(skills []harness.Skill) []string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
	}

	return names
}

func diagnosticCodes(diagnostics []resource.Diagnostic) []string {
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}

	return codes
}

func resourcePaths(resources []harness.SkillResource) []string {
	values := make([]string, 0, len(resources))
	for _, resource := range resources {
		values = append(values, resource.Path)
	}

	return values
}

func writeBundle(t *testing.T, root, directory, id, extensionID string) {
	t.Helper()

	bundleDir := filepath.Join(root, directory)
	require.NoError(t, os.MkdirAll(bundleDir, 0o750))

	manifest := fmt.Sprintf(`{
  "schema":"pips.bundle/v1alpha1",
  "id":%q,
  "version":"1.0.0",
  "extensions":[%q]
}`, id, extensionID)
	require.NoError(t, os.WriteFile(
		filepath.Join(bundleDir, "pips-bundle.json"),
		[]byte(manifest),
		0o600,
	))
}
