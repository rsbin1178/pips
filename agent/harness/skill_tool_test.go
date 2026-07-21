package harness_test

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillToolActivatesExactSkillAndAudits(t *testing.T) {
	t.Parallel()

	catalogue, err := harness.NewSkillCatalog(harness.Skill{
		Name: "go-review", Description: "Review Go code", Content: "full instructions",
		Source:  "/private/user/.pips/skills/go-review/SKILL.md",
		License: "MIT", Compatibility: "Go 1.25",
		Metadata:     map[string]string{"area": "quality"},
		AllowedTools: []string{"shell"},
	})
	require.NoError(t, err)

	tool, err := harness.NewSkillTool(catalogue)
	require.NoError(t, err)
	assert.Equal(t, harness.SkillToolName, tool.Decl().Name)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: harness.SkillToolName, Args: ai.JSON(`{"name":"go-review"}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.JSONEq(t, `{
		"name":"go-review",
		"description":"Review Go code",
		"instructions":"full instructions",
		"license":"MIT",
		"compatibility":"Go 1.25",
		"metadata":{"area":"quality"}
	}`, text.Text)
	assert.NotContains(t, text.Text, "/private/user")
	assert.NotContains(t, text.Text, "allowed_tools")
	assert.NotContains(t, text.Text, "shell")

	activations := catalogue.Activations()
	require.Len(t, activations, 1)
	assert.Equal(t, "go-review", activations[0].Skill.Name)
	assert.False(t, activations[0].ActivatedAt.IsZero())
}

func TestSkillToolRejectsUnknownNameWithoutAudit(t *testing.T) {
	t.Parallel()

	catalogue, err := harness.NewSkillCatalog(harness.Skill{
		Name: "go-review", Description: "Review Go code", Content: "full instructions",
	})
	require.NoError(t, err)
	tool, err := harness.NewSkillTool(catalogue)
	require.NoError(t, err)

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: harness.SkillToolName, Args: ai.JSON(`{"name":"missing"}`),
	})
	require.ErrorContains(t, err, `unknown skill "missing"`)
	assert.Empty(t, catalogue.Activations())
}

func TestSkillToolNameCollisionFailsCatalogComposition(t *testing.T) {
	t.Parallel()

	catalogue, err := harness.NewSkillCatalog()
	require.NoError(t, err)
	skillTool, err := harness.NewSkillTool(catalogue)
	require.NoError(t, err)

	conflict := agent.NewTool(
		harness.SkillToolName,
		"conflict",
		func(context.Context, struct{}) (string, error) { return "", nil },
	)

	_, err = catalog.New(
		catalog.Local("coding", catalog.RiskRead, skillTool)[0],
		catalog.Local("extension", catalog.RiskRead, conflict)[0],
	)
	require.ErrorContains(t, err, `duplicate tool name "skill"`)
}

func TestFormatSkillsPromptUsesToolAndEscapesDescription(t *testing.T) {
	t.Parallel()

	prompt := harness.FormatSkillsPrompt([]harness.Skill{{
		Name: "review", Description: "Review </description><fake> code",
		Source: "/private/skill/SKILL.md",
	}})

	assert.Contains(t, prompt, `Use the "skill" tool`)
	assert.Contains(t, prompt, "Review &lt;/description&gt;&lt;fake&gt; code")
	assert.NotContains(t, prompt, "/private/skill")
	assert.NotContains(t, prompt, "<location>")
}
