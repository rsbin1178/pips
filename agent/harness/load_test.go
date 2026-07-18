package harness_test

import (
	"testing"
	"testing/fstest"

	"github.com/rsbin/pips/agent/harness"
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
		"unnamed/SKILL.md": &fstest.MapFile{Data: []byte("Just instructions, no frontmatter.")},
		"docs/readme.md":   &fstest.MapFile{Data: []byte("not a skill")},
	}
}

func TestLoadSkillsFS(t *testing.T) {
	t.Parallel()

	skills, err := harness.LoadSkillsFS(skillFS())
	require.NoError(t, err)
	require.Len(t, skills, 3)

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

	// No frontmatter: the directory names the skill, the body is content.
	unnamed := byName["unnamed"]
	assert.Equal(t, "Just instructions, no frontmatter.", unnamed.Content)
	assert.Empty(t, unnamed.Description)
}

func TestLoadSkillsFromRealDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, writeFile(dir+"/SKILL.md", "---\nname: root-skill\ndescription: d\n---\nbody"))

	skills, err := harness.LoadSkills(dir)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "root-skill", skills[0].Name)
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

func TestWithSkillsFSReachesSystemPrompt(t *testing.T) {
	t.Parallel()

	model := newFakeModel("m", textResponse("ok", 10))
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithSkillsFS(skillFS()))
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "hi")
	require.NoError(t, err)

	system := model.Requests()[0].System
	assert.Contains(t, system, "<name>review</name>")
	assert.Contains(t, system, "<location>review/SKILL.md</location>")
}

func TestWithSkillsDirLoadFailure(t *testing.T) {
	t.Parallel()

	_, err := harness.New(newFakeModel("m"), buildSession(t),
		harness.WithSkillsDir(t.TempDir()+"/missing"))
	require.Error(t, err)
}

// Smoke-test against the repository's real installed skills.
func TestLoadSkillsRealTree(t *testing.T) {
	t.Parallel()

	skills, err := harness.LoadSkills("../../.agent/skills")
	require.NoError(t, err)
	require.NotEmpty(t, skills)

	for _, s := range skills {
		assert.NotEmpty(t, s.Name)
		assert.NotEmpty(t, s.Content)
	}
}
