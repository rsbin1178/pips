package harness_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skillFS() fstest.MapFS {
	return fstest.MapFS{
		"review/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: review
description: "Code review checklist."
---

Do reviews carefully.`)},
		"deploy/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: deploy
description: >-
  Deployment runbook
  for production.
metadata:
  ignored: value
---
Deploy steps.`)},
		"docs/readme.md": &fstest.MapFile{Data: []byte("not a skill")},
	}
}

func TestLoadSkillsFS(t *testing.T) {
	t.Parallel()

	skills, err := harness.LoadSkillsFS(skillFS())
	require.NoError(t, err)
	require.Len(t, skills, 2)

	byName := map[string]harness.Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}

	// Quoted scalar.
	review := byName["review"]
	assert.Equal(t, "Code review checklist.", review.Description)
	assert.Equal(t, "Do reviews carefully.", review.Content)
	assert.Equal(t, "review/SKILL.md", review.Source)

	// Folded multi-line scalar; nested metadata ignored.
	deploy := byName["deploy"]
	assert.Equal(t, "Deployment runbook for production.", deploy.Description)
	assert.Equal(t, "Deploy steps.", deploy.Content)
}

func TestLoadSkillFSLoadsOnlyExplicitManifest(t *testing.T) {
	t.Parallel()

	skill, err := harness.LoadSkillFS(skillFS(), "review/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "review", skill.Name)

	_, err = harness.LoadSkillFS(skillFS(), "docs/readme.md")
	require.ErrorContains(t, err, "invalid path")
}

func TestLoadSkillsFromRealDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir+"/root-skill", 0o750))
	require.NoError(t, writeFile(dir+"/root-skill/SKILL.md", "---\nname: root-skill\ndescription: d\n---\nbody"))

	skills, err := harness.LoadSkills(dir)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "root-skill", skills[0].Name)
}

func TestLoadSkillsFSRejectsInvalidManifest(t *testing.T) {
	t.Parallel()

	_, err := harness.LoadSkillsFS(fstest.MapFS{
		"bad/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: Other\ndescription: d\n---\nbody")},
	})
	require.ErrorContains(t, err, "name must be")

	_, err = harness.LoadSkillsFS(fstest.MapFS{
		"bad/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: bad\ndescription: d\nname: duplicate\n---\nbody")},
	})
	require.ErrorContains(t, err, "duplicated")

	_, err = harness.LoadSkillsFS(fstest.MapFS{
		"bad/SKILL.md": &fstest.MapFile{Data: []byte("instructions only")},
	})
	require.ErrorContains(t, err, "requires YAML frontmatter")

	_, err = harness.LoadSkillsFS(fstest.MapFS{
		"bad/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: bad\ndescription: d\ncompatibility: \"\"\n---\nbody")},
	})
	require.ErrorContains(t, err, "compatibility must be between 1 and 500 characters")
}

func TestLoadSkillFSUsesUnicodeCharacterRulesAndNormalization(t *testing.T) {
	t.Parallel()

	name := strings.Repeat("界", 64)
	skill, err := harness.LoadSkillFS(fstest.MapFS{
		name + "/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: " + name + "\ndescription: Unicode name.\ncompatibility: 跨平台\n---\nBody.",
		)},
	}, name+"/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, name, skill.Name)
	assert.Equal(t, "跨平台", skill.Compatibility)

	fullwidthDirectory := "ｒｅｖｉｅｗ"
	skill, diagnostics, err := harness.LoadSkillFSWithDiagnostics(fstest.MapFS{
		fullwidthDirectory + "/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: review\ndescription: Normalized name.\n---\nBody.",
		)},
	}, fullwidthDirectory+"/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "review", skill.Name)
	assert.Empty(t, diagnostics)

	tooLong := strings.Repeat("界", 65)
	_, err = harness.LoadSkillFS(fstest.MapFS{
		tooLong + "/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: " + tooLong + "\ndescription: Too long.\n---\nBody.",
		)},
	}, tooLong+"/SKILL.md")
	require.ErrorContains(t, err, "name must be")
}

func TestLoadSkillFSWithDiagnosticsSupportsEcosystemFrontmatter(t *testing.T) {
	t.Parallel()

	fSys := fstest.MapFS{
		"actual-dir/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: declared-name
description: >-
  Review code with
  the project conventions.
license: MIT
compatibility: Go 1.26+
metadata:
  author: pips
  version: "1"
  openclaw:
    emoji: test
allowed-tools: Read Grep Bash(go:*)
user-invocable: false
disable-model-invocation: false
argument-hint: "[path]"
unknown-extension:
  enabled: true
---
Review the selected code.`)},
	}

	skill, diagnostics, err := harness.LoadSkillFSWithDiagnostics(
		fSys,
		"actual-dir/SKILL.md",
	)
	require.NoError(t, err)
	assert.Equal(t, "declared-name", skill.Name)
	assert.Equal(t, "Review code with the project conventions.", skill.Description)
	assert.Equal(t, []string{"Read", "Grep", "Bash(go:*)"}, skill.AllowedTools)
	assert.Equal(t, map[string]string{"author": "pips", "version": "1"}, skill.Metadata)
	assert.False(t, skill.UserInvocable())
	assert.True(t, skill.ModelInvocable())
	assert.Equal(t, harness.SkillInvocationModelOnly, skill.Invocation)

	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}

	assert.ElementsMatch(t, []string{
		"client_field",
		"client_field",
		"directory_name_mismatch",
		"metadata_value_ignored",
		"unsupported_field",
		"unknown_field",
	}, codes)
}

func TestLoadSkillFSWithDiagnosticsDefaultsMissingName(t *testing.T) {
	t.Parallel()

	skill, diagnostics, err := harness.LoadSkillFSWithDiagnostics(
		fstest.MapFS{
			"review/SKILL.md": &fstest.MapFile{Data: []byte("---\ndescription: Review code.\n---\nInstructions.")},
		},
		"review/SKILL.md",
	)
	require.NoError(t, err)
	assert.Equal(t, "review", skill.Name)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "name_defaulted", diagnostics[0].Code)
}

func TestLoadSkillFSWithDiagnosticsNormalizesInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		frontmatter            string
		expected               harness.SkillInvocation
		expectedUserInvocable  bool
		expectedModelInvocable bool
	}{
		{
			name:                   "default",
			expected:               harness.SkillInvocationDefault,
			expectedUserInvocable:  true,
			expectedModelInvocable: true,
		},
		{
			name:                   "user only",
			frontmatter:            "disable-model-invocation: true\n",
			expected:               harness.SkillInvocationUserOnly,
			expectedUserInvocable:  true,
			expectedModelInvocable: false,
		},
		{
			name:                   "model only",
			frontmatter:            "user-invocable: false\n",
			expected:               harness.SkillInvocationModelOnly,
			expectedUserInvocable:  false,
			expectedModelInvocable: true,
		},
		{
			name:                   "disabled",
			frontmatter:            "user-invocable: false\ndisable-model-invocation: true\n",
			expected:               harness.SkillInvocationDisabled,
			expectedUserInvocable:  false,
			expectedModelInvocable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manifest := "---\nname: review\ndescription: Review code.\n" +
				tt.frontmatter + "---\nInstructions."
			skill, _, err := harness.LoadSkillFSWithDiagnostics(
				fstest.MapFS{
					"review/SKILL.md": &fstest.MapFile{Data: []byte(manifest)},
				},
				"review/SKILL.md",
			)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, skill.Invocation)
			assert.Equal(t, tt.expectedUserInvocable, skill.UserInvocable())
			assert.Equal(t, tt.expectedModelInvocable, skill.ModelInvocable())
		})
	}
}

func TestLoadTemplatesFS(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"review.md": &fstest.MapFile{Data: []byte(`---
description: Review a file.
---
Review $1.`)},
		"plain.md":       &fstest.MapFile{Data: []byte("No frontmatter here.")},
		"skill/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: s\n---\nskill, not a template")},
	}

	templates, err := harness.LoadTemplatesFS(fsys)
	require.NoError(t, err)
	require.Len(t, templates, 2)

	byName := map[string]harness.PromptTemplate{}
	for _, tmpl := range templates {
		byName[tmpl.Name] = tmpl
	}

	assert.Equal(t, "Review a file.", byName["review"].Description)
	assert.Equal(t, "Review $1.", byName["review"].Content)
	assert.Equal(t, "No frontmatter here.", byName["plain"].Content)
}

func TestLoadTemplateFSLoadsOnlyExplicitTemplate(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"prompts/review.md": &fstest.MapFile{Data: []byte("Review $1.")},
		"prompts/other.md":  &fstest.MapFile{Data: []byte("Other.")},
	}

	template, err := harness.LoadTemplateFS(fsys, "prompts/review.md")
	require.NoError(t, err)
	assert.Equal(t, "review", template.Name)
	assert.Equal(t, "Review $1.", template.Content)

	_, err = harness.LoadTemplateFS(fsys, "prompts")
	require.ErrorContains(t, err, "invalid path")
}

func TestWithSkillsFSReachesSystemPrompt(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("ok", 10))
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithSkillsFS(skillFS()))
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "hi")
	require.NoError(t, err)

	system := requestSystemText(t, model.Requests()[0])
	assert.Contains(t, system, "<name>review</name>")
	assert.Contains(t, system, `Use the "skill" tool`)
	assert.NotContains(t, system, "<location>")
}

func TestWithSkillsDirLoadFailure(t *testing.T) {
	t.Parallel()

	_, err := harness.New(newScriptedModel("m"), buildSession(t),
		harness.WithSkillsDir(t.TempDir()+"/missing"))
	require.Error(t, err)
}

// Smoke-test against the repository's real installed skills.
func TestLoadSkillsRealTree(t *testing.T) {
	t.Parallel()

	skills, err := harness.LoadSkills("../../.agents/skills")
	require.NoError(t, err)
	require.NotEmpty(t, skills)

	for _, s := range skills {
		assert.NotEmpty(t, s.Name)
		assert.NotEmpty(t, s.Content)
	}
}
