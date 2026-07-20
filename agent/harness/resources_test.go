package harness_test

import (
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromptTemplateFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		args     []string
		expected string
	}{
		{
			name:     "positional",
			content:  "Review $1 for $2.",
			args:     []string{"main.go", "bugs"},
			expected: "Review main.go for bugs.",
		},
		{
			name:     "all arguments",
			content:  "Run with: $ARGUMENTS",
			args:     []string{"a", "b", "c"},
			expected: "Run with: a b c",
		},
		{
			name:     "no placeholders appends",
			content:  "Do the thing.",
			args:     []string{"now"},
			expected: "Do the thing.\n\nnow",
		},
		{
			name:     "no args",
			content:  "Static prompt.",
			expected: "Static prompt.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpl := harness.PromptTemplate{Name: "t", Content: tt.content}
			assert.Equal(t, tt.expected, tmpl.Format(tt.args...))
		})
	}
}

func TestValidateTemplates(t *testing.T) {
	t.Parallel()

	require.NoError(t, harness.ValidateTemplates(
		harness.PromptTemplate{Name: "review", Content: "Review $1."},
		harness.PromptTemplate{Name: "summary", Content: "Summarize $1."},
	))

	for _, templates := range [][]harness.PromptTemplate{
		{{Name: "", Content: "content"}},
		{{Name: " review ", Content: "content"}},
		{{Name: "review", Content: ""}},
		{
			{Name: "review", Content: "one"},
			{Name: "review", Content: "two"},
		},
	} {
		require.Error(t, harness.ValidateTemplates(templates...))
	}
}

func TestFormatSkillsPrompt(t *testing.T) {
	t.Parallel()

	assert.Empty(t, harness.FormatSkillsPrompt(nil))

	block := harness.FormatSkillsPrompt([]harness.Skill{
		{Name: "review", Description: "Code review checklist."},
		{Name: "deploy", Description: "Deployment runbook."},
	})

	assert.Contains(t, block, "<available-skills>")
	assert.Contains(t, block, "<name>review</name>")
	assert.Contains(t, block, "<description>Deployment runbook.</description>")
}

func TestHarnessSystemFuncSeesResources(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("ok", 10))
	sess := buildSession(t)

	h, err := harness.New(model, sess,
		harness.WithSkills(harness.Skill{Name: "s1", Description: "d"}),
		harness.WithSystemFunc(func(sc harness.SystemContext) string {
			require.Len(t, sc.Skills, 1)
			require.NotNil(t, sc.Session)

			return "assembled"
		}),
	)
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "hi")
	require.NoError(t, err)

	system := model.Requests()[0].System
	assert.Contains(t, system, "assembled")
	assert.Contains(t, system, "<name>s1</name>", "skills block appended")
}
