//nolint:wsl_v5,paralleltest // Profile fixture construction includes filesystem-mode cases that remain serial.
package agentprofile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadResolvesScopedPrecedenceAndRetainsSuppressedEntries(t *testing.T) {
	t.Parallel()

	fixture := newProfileFixture(t)
	shared := t.TempDir()
	var err error
	fixture.layout, err = fixture.layout.WithSharedAgentsDir(shared)
	require.NoError(t, err)

	writeProfile(t, shared, "go-helper.md", basicProfile("Shared", "shared", "shared instructions"))
	writeProfile(t, fixture.layout.AgentsDir(), "go-helper.md", basicProfile("Pips", "pips", "pips instructions"))
	writeProfile(
		t,
		projectDirectory(fixture, paths.ProjectSharedAgentsDir()),
		"go-helper.md",
		basicProfile("Project shared", "project shared", "project shared instructions"),
	)
	writeProfile(
		t,
		projectDirectory(fixture, paths.ProjectAgentsDir()),
		"go-helper.md",
		basicProfile("Project Pips", "project pips", "project pips instructions"),
	)

	registry, err := Load(t.Context(), Options{
		Paths: fixture.layout, Tree: fixture.tree, ProjectTrusted: true, Limits: DefaultLimits(),
	})
	require.NoError(t, err)

	definition, ok := registry.Lookup("go-helper")
	require.True(t, ok)
	assert.Equal(t, ScopeProjectPips, definition.Scope)
	assert.Equal(t, "project:pips/go-helper.md", definition.Source)
	assert.Equal(t, "project pips instructions", definition.Instructions)
	assert.NotContains(t, definition.Source, fixture.workspace.Root())

	customEntries := filterEntries(registry.Entries(), "go-helper")
	require.Len(t, customEntries, 4)
	assert.Equal(t, 1, countEntryStatus(customEntries, EntryAvailable))
	assert.Equal(t, 3, countEntryStatus(customEntries, EntrySuppressed))
	assert.ElementsMatch(t, []string{
		"user:shared/go-helper.md",
		"user:pips/go-helper.md",
		"project:shared/go-helper.md",
		"project:pips/go-helper.md",
	}, entrySources(customEntries))
	assert.Equal(t, []string{"agent_suppressed", "agent_suppressed", "agent_suppressed"}, diagnosticCodes(registry.Diagnostics()))
}

func TestLoadSkipsProjectRootsBeforeTrust(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	registry, err := Load(t.Context(), Options{
		Paths: layout, ProjectTrusted: false, Limits: DefaultLimits(),
	})
	require.NoError(t, err)
	_, found := registry.Lookup("untrusted")
	assert.False(t, found)
	assert.Len(t, registry.List(), len(BuiltinDefinitions()))
}

func TestLoadRejectsBuiltinCollisionWithoutReplacingBuiltin(t *testing.T) {
	t.Parallel()

	fixture := newProfileFixture(t)
	writeProfile(t, fixture.layout.AgentsDir(), "explore.md", basicProfile("Not explore", "collision", "instructions"))

	registry, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: DefaultLimits()})
	require.NoError(t, err)

	definition, ok := registry.Lookup("explore")
	require.True(t, ok)
	assert.Equal(t, KindBuiltin, definition.Kind)
	assert.Equal(t, "builtin:explore", definition.Source)
	entries := filterEntries(registry.Entries(), "explore")
	require.Len(t, entries, 2)
	assert.Equal(t, []EntryStatus{EntryAvailable, EntryInvalid}, entryStatuses(entries))
	assert.Equal(t, []string{"builtin_collision"}, diagnosticCodes(registry.Diagnostics()))
}

func TestLoadIsolatesInvalidDefinitionsAndEnforcesSameScopeCollision(t *testing.T) {
	t.Parallel()

	fixture := newProfileFixture(t)
	writeProfile(t, fixture.layout.AgentsDir(), "valid.md", basicProfile("Valid", "valid", "instructions"))
	writeProfile(t, fixture.layout.AgentsDir(), "unknown.md", "---\nschema: pips.agent/v1alpha1\nname: Unknown\ndescription: unknown\npermissions: full\n---\ninstructions\n")
	writeProfile(t, fixture.layout.AgentsDir(), "duplicate.md", "---\nschema: pips.agent/v1alpha1\nname: One\nname: Two\ndescription: duplicate\n---\ninstructions\n")
	writeProfile(t, filepath.Join(fixture.layout.AgentsDir(), "one"), "same.md", basicProfile("One", "same", "one"))
	writeProfile(t, filepath.Join(fixture.layout.AgentsDir(), "two"), "same.md", basicProfile("Two", "same", "two"))

	registry, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: DefaultLimits()})
	require.NoError(t, err)

	definition, ok := registry.Lookup("valid")
	require.True(t, ok)
	assert.Equal(t, "instructions", definition.Instructions)
	_, found := registry.Lookup("same")
	assert.False(t, found)
	assert.Equal(t, []string{"duplicate_field", "duplicate_id", "duplicate_id", "unsupported_field"}, diagnosticCodes(registry.Diagnostics()))
}

func TestLoadClonesRegistryValuesAndFiltersVisibility(t *testing.T) {
	t.Parallel()

	fixture := newProfileFixture(t)
	writeProfile(t, fixture.layout.AgentsDir(), "private.md", `---
schema: pips.agent/v1alpha1
name: Private
description: Private profile
visibility:
  user: true
  model: false
delivery: [foreground, background]
tools:
  allow: [tool:read]
skills:
  allow: [golang-testing]
  preload: [golang-testing]
output:
  format: json_schema
  schema:
    type: object
    additionalProperties: false
    required: [summary]
    properties:
      summary: {type: string, maxLength: 100}
---

private instructions
`)

	registry, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: DefaultLimits()})
	require.NoError(t, err)
	definitions := registry.List()
	private := findDefinition(t, definitions, "private")
	private.Delivery[0] = DeliveryBackground
	private.Tools.Allow[0].Value = "mutated"
	private.Skills.Allow[0] = "mutated"
	private.Output.Schema[0] = '{'

	again, ok := registry.Lookup("private")
	require.True(t, ok)
	assert.Equal(t, DeliveryForeground, again.Delivery[0])
	assert.Equal(t, "read", again.Tools.Allow[0].Value)
	assert.Equal(t, "golang-testing", again.Skills.Allow[0])
	assert.True(t, slices.Equal([]Delivery{DeliveryForeground, DeliveryBackground}, again.Delivery))
	assert.Len(t, registry.VisibleFor(AudienceUser), len(BuiltinDefinitions())+1)
	assert.Len(t, registry.VisibleFor(AudienceModel), len(BuiltinDefinitions()))
}

func TestLoadRejectsStrictProfileContracts(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "unsupported privilege field",
			body: "---\nschema: pips.agent/v1alpha1\nname: Unsafe\ndescription: Unsafe\nsandbox: full\n---\ninstructions\n",
			code: "unsupported_field",
		},
		{
			name: "invalid selector",
			body: "---\nschema: pips.agent/v1alpha1\nname: Selector\ndescription: Selector\ntools:\n  allow: [tool:*]\n---\ninstructions\n",
			code: "invalid_selector",
		},
		{
			name: "required selector omitted from allow",
			body: "---\nschema: pips.agent/v1alpha1\nname: Required\ndescription: Required\ntools:\n  allow: [tool:read]\n  require: [tool:shell]\n---\ninstructions\n",
			code: "invalid_tools",
		},
		{
			name: "remote schema reference",
			body: "---\nschema: pips.agent/v1alpha1\nname: Remote\ndescription: Remote\noutput:\n  format: json_schema\n  schema:\n    $ref: https://example.com/schema\n---\ninstructions\n",
			code: "invalid_schema",
		},
		{
			name: "unsupported schema keyword",
			body: "---\nschema: pips.agent/v1alpha1\nname: Keyword\ndescription: Keyword\noutput:\n  format: json_schema\n  schema:\n    type: object\n    format: uri\n---\ninstructions\n",
			code: "unsupported_schema_keyword",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProfileFixture(t)
			writeProfile(t, fixture.layout.AgentsDir(), "invalid.md", test.body)

			registry, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: DefaultLimits()})
			require.NoError(t, err)
			assert.Equal(t, []string{test.code}, diagnosticCodes(registry.Diagnostics()))
			_, found := registry.Lookup("invalid")
			assert.False(t, found)
		})
	}
}

func TestParseOneShotUsesTheDiscoveredDefinitionContractWithoutPersistence(t *testing.T) {
	t.Parallel()

	definition, err := ParseOneShot(
		"review-slice",
		[]byte(basicProfile("Review slice", "Review one bounded change", "Review the delegated change.")),
		DefaultLimits(),
	)
	require.NoError(t, err)
	assert.Equal(t, "review-slice", definition.ID)
	assert.Equal(t, KindEphemeral, definition.Kind)
	assert.Equal(t, ScopeEphemeral, definition.Scope)
	assert.Equal(t, "one-shot:review-slice", definition.Source)
	assert.NotEmpty(t, definition.Digest)

	_, err = ParseOneShot("Not Valid", []byte("---\nschema: pips.agent/v1alpha1\n---\nbody"), DefaultLimits())
	require.ErrorIs(t, err, ErrInvalid)
}

func TestParseOneShotDelegationAllowIsStrictBoundedAndDetached(t *testing.T) {
	t.Parallel()

	definition, err := ParseOneShot("root-agent", []byte(`---
schema: pips.agent/v1alpha1
name: Root
description: Delegates exact work
delegation:
  allow: [go-reviewer, test-runner]
---
Delegate carefully.
`), DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, []string{"go-reviewer", "test-runner"}, definition.Delegation.Allow)

	cloned := definition.Clone()
	cloned.Delegation.Allow[0] = "changed"
	assert.Equal(t, "go-reviewer", definition.Delegation.Allow[0])

	for _, body := range []string{
		"delegation:\n  allow: [go-reviewer, go-reviewer]",
		"delegation:\n  allow: [Explore]",
		"delegation:\n  allow: [../escape]",
		"delegation:\n  deny: [go-reviewer]",
	} {
		data := []byte("---\nschema: pips.agent/v1alpha1\nname: Root\ndescription: Delegates exact work\n" + body + "\n---\nbody\n")
		_, err := ParseOneShot("root-agent", data, DefaultLimits())
		require.ErrorIs(t, err, ErrInvalid, body)
	}
}

func TestLoadRejectsUnsafeUserRootAndProjectSymlinkEscape(t *testing.T) {
	t.Run("unsafe user root", func(t *testing.T) {
		fixture := newProfileFixture(t)
		writeProfile(t, fixture.layout.AgentsDir(), "valid.md", basicProfile("Valid", "valid", "instructions"))
		require.NoError(t, os.Chmod(fixture.layout.AgentsDir(), 0o777)) //nolint:gosec // Deliberately unsafe fixture.

		_, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: DefaultLimits()})
		require.ErrorIs(t, err, ErrInsecure)
	})

	t.Run("project symlink escape", func(t *testing.T) {
		fixture := newProfileFixture(t)
		outside := t.TempDir()
		writeProfile(t, outside, "escaped.md", basicProfile("Escaped", "escaped", "outside"))
		projectRoot := projectDirectory(fixture, paths.ProjectAgentsDir())
		require.NoError(t, os.MkdirAll(projectRoot, 0o750))
		require.NoError(t, os.Symlink(filepath.Join(outside, "escaped.md"), filepath.Join(projectRoot, "escaped.md")))

		registry, err := Load(t.Context(), Options{
			Paths: fixture.layout, Tree: fixture.tree, ProjectTrusted: true, Limits: DefaultLimits(),
		})
		require.NoError(t, err)
		_, found := registry.Lookup("escaped")
		assert.False(t, found)
	})
}

func TestLoadEnforcesIndividualAndAggregateLimits(t *testing.T) {
	t.Run("individual definition becomes diagnostic", func(t *testing.T) {
		fixture := newProfileFixture(t)
		writeProfile(t, fixture.layout.AgentsDir(), "large.md", basicProfile("Large", "large", strings.Repeat("x", 128)))
		limits := DefaultLimits()
		limits.MaxDefinitionBytes = 64
		limits.MaxFrontMatterBytes = 32
		limits.MaxBodyBytes = 32

		registry, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: limits})
		require.NoError(t, err)
		assert.Equal(t, []string{"definition_too_large"}, diagnosticCodes(registry.Diagnostics()))
	})

	t.Run("aggregate content fails closed", func(t *testing.T) {
		fixture := newProfileFixture(t)
		writeProfile(t, fixture.layout.AgentsDir(), "one.md", basicProfile("One", "one", "one"))
		writeProfile(t, fixture.layout.AgentsDir(), "two.md", basicProfile("Two", "two", "two"))
		limits := DefaultLimits()
		limits.MaxTotalDefinitionBytes = int64(len(basicProfile("One", "one", "one"))) + 1

		_, err := Load(t.Context(), Options{Paths: fixture.layout, Limits: limits})
		require.ErrorIs(t, err, ErrLimitExceeded)
	})
}

func TestLoadHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	fixture := newProfileFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Load(ctx, Options{Paths: fixture.layout, Limits: DefaultLimits()})
	require.ErrorIs(t, err, context.Canceled)
}

type profileFixture struct {
	layout    paths.Layout
	workspace workspace.Workspace
	tree      *workspace.Tree
}

func newProfileFixture(t *testing.T) profileFixture {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	return profileFixture{layout: layout, workspace: ws, tree: tree}
}

func projectDirectory(fixture profileFixture, relative string) string {
	return filepath.Join(fixture.workspace.Root(), filepath.FromSlash(relative))
}

func writeProfile(t *testing.T, directory, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600))
}

func basicProfile(name, description, instructions string) string {
	return fmt.Sprintf("---\nschema: pips.agent/v1alpha1\nname: %s\ndescription: %s\n---\n%s\n", name, description, instructions)
}

func filterEntries(entries []Entry, id string) []Entry {
	result := make([]Entry, 0)
	for _, entry := range entries {
		if entry.ID == id {
			result = append(result, entry)
		}
	}

	return result
}

func entryStatuses(entries []Entry) []EntryStatus {
	result := make([]EntryStatus, len(entries))
	for index := range entries {
		result[index] = entries[index].Status
	}

	return result
}

func entrySources(entries []Entry) []string {
	result := make([]string, len(entries))
	for index := range entries {
		result[index] = entries[index].Source
	}

	return result
}

func countEntryStatus(entries []Entry, want EntryStatus) int {
	count := 0
	for _, entry := range entries {
		if entry.Status == want {
			count++
		}
	}

	return count
}

func diagnosticCodes(diagnostics []Diagnostic) []string {
	result := make([]string, len(diagnostics))
	for index := range diagnostics {
		result[index] = diagnostics[index].Code
	}

	return result
}

func findDefinition(t *testing.T, definitions []Definition, id string) Definition {
	t.Helper()
	for _, definition := range definitions {
		if definition.ID == id {
			return definition
		}
	}
	t.Fatalf("definition %q not found", id)

	return Definition{}
}
