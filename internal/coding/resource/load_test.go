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
	assert.Equal(t, "project:.pips/skills/review/SKILL.md", skills[0].Source)
	assert.Empty(t, skills[0].AllowedTools)
	assert.NotContains(t, skills[0].Source, fixture.workspace.Root())
	require.Len(t, diagnostics, 2)
	assert.Equal(t, "project:.pips/skills/review/SKILL.md", diagnostics[0].Winner)
	assert.Equal(t, "project:.pips/skills/review/SKILL.md", diagnostics[1].Winner)

	// Returned Skills are owned snapshots.
	skills[0].Metadata["changed"] = "true"
	again, _, err := loaded.ResolveSkills()
	require.NoError(t, err)
	assert.NotContains(t, again[0].Metadata, "changed")
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
