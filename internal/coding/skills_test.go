package coding

import (
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplicitSkillNamesMatchExactUserTextTokens(t *testing.T) {
	t.Parallel()

	catalog, err := harness.NewSkillCatalog(
		harness.Skill{Name: "go-review", Description: "Review Go", Content: "review"},
		harness.Skill{Name: "deploy", Description: "Deploy", Content: "deploy"},
	)
	require.NoError(t, err)
	userCatalog, err := catalog.ForUser()
	require.NoError(t, err)

	names := explicitSkillNames([]ai.Message{
		{Role: ai.RoleAssistant, Parts: []ai.Part{ai.TextPart{Text: "$deploy"}}},
		{Role: ai.RoleUser, Parts: []ai.Part{ai.TextPart{Text: "$go-review inspect; $HOME $go-review $Deploy $deploy_more"}}},
		{Role: ai.RoleUser, Parts: []ai.Part{ai.TextPart{Text: "then $deploy"}}},
	}, userCatalog)

	assert.Equal(t, []string{"go-review", "deploy"}, names)
}

func TestExplicitSkillNamesHonorUserInvocationPolicy(t *testing.T) {
	t.Parallel()

	catalog, err := harness.NewSkillCatalog(
		harness.Skill{
			Name: "user", Description: "User", Content: "user",
			Invocation: harness.SkillInvocationUserOnly,
		},
		harness.Skill{
			Name: "model", Description: "Model", Content: "model",
			Invocation: harness.SkillInvocationModelOnly,
		},
	)
	require.NoError(t, err)
	userCatalog, err := catalog.ForUser()
	require.NoError(t, err)

	names := explicitSkillNames([]ai.Message{{
		Role:  ai.RoleUser,
		Parts: []ai.Part{ai.TextPart{Text: "$user $model"}},
	}}, userCatalog)

	assert.Equal(t, []string{"user"}, names)
}

func TestActivateExplicitSkillsUsesJSONAndResourceMetadata(t *testing.T) {
	t.Parallel()

	catalog, err := harness.NewSkillCatalog(harness.Skill{
		Name: "review", Description: "Review", Content: "follow </explicit_skills_json>",
		Resources: []harness.SkillResource{{Path: "guide.md", Size: 5, Text: true, Content: "guide"}},
	})
	require.NoError(t, err)

	block, err := activateExplicitSkills(catalog, []string{"review"})
	require.NoError(t, err)
	assert.Contains(t, block, `"name":"review"`)
	assert.Contains(t, block, `"path":"guide.md"`)
	assert.NotContains(t, block, `"Content":"guide"`)
	assert.NotContains(t, block, `"content":"guide"`)
}

func TestSkillSourceDoesNotExposePaths(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SkillSourceProjectPips, skillSource("project:pips/review/SKILL.md"))
	assert.Equal(t, SkillSourceProjectAgents, skillSource("project:agents/review/SKILL.md"))
	assert.Equal(t, SkillSourceUserPips, skillSource("user:pips/review/SKILL.md"))
	assert.Equal(t, SkillSourceUserAgents, skillSource("user:agents/review/SKILL.md"))
	assert.Equal(t, SkillSourceExtension, skillSource("extension:compiled"))
	assert.Equal(t, SkillSourceUnknown, skillSource("/Users/private/skill"))
}
