package harness_test

import (
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillCatalogDiscoveryActivationAndCopies(t *testing.T) {
	t.Parallel()

	catalog, err := harness.NewSkillCatalog(
		harness.Skill{Name: "go-review", Description: "Review Go code", Content: "full review instructions", Metadata: map[string]string{"area": "quality"}},
		harness.Skill{Name: "deploy", Description: "Deploy safely", Content: "full deploy instructions"},
	)
	require.NoError(t, err)

	listed := catalog.List()
	require.Len(t, listed, 2)

	for _, skill := range listed {
		assert.Empty(t, skill.Content)

		if skill.Name == "go-review" {
			skill.Metadata["area"] = "mutated"
		}
	}

	found := catalog.Search("go quality")
	require.Len(t, found, 1)
	assert.Equal(t, "go-review", found[0].Name)
	assert.Empty(t, found[0].Content)
	assert.Equal(t, "quality", found[0].Metadata["area"])

	activation, err := catalog.Activate("go-review")
	require.NoError(t, err)
	assert.Equal(t, "full review instructions", activation.Skill.Content)
	assert.False(t, activation.ActivatedAt.IsZero())

	records := catalog.Activations()
	require.Len(t, records, 1)
	records[0].Skill.Metadata["area"] = "changed"
	assert.Equal(t, "quality", catalog.Activations()[0].Skill.Metadata["area"])
}

func TestSkillCatalogRejectsInvalidSkill(t *testing.T) {
	t.Parallel()

	_, err := harness.NewSkillCatalog(harness.Skill{Name: "Bad", Description: "d", Content: "c"})
	require.ErrorContains(t, err, "skill name")

	_, err = harness.NewSkillCatalog(harness.Skill{Name: "good", Content: "c"})
	require.ErrorContains(t, err, "description")

	_, err = harness.NewSkillCatalog(harness.Skill{
		Name:        "good",
		Description: "description",
		Content:     "content",
		Resources: []harness.SkillResource{
			{Path: "../outside", Size: 1, Text: true, Content: "x"},
		},
	})
	require.ErrorContains(t, err, "invalid resource path")
}

func TestSkillCatalogFiltersInvocationAndReadsResources(t *testing.T) {
	t.Parallel()

	catalog, err := harness.NewSkillCatalog(
		harness.Skill{
			Name:        "both",
			Description: "Both paths",
			Content:     "instructions",
			Resources: []harness.SkillResource{
				{Path: "references/guide.md", Size: 5, Text: true, Content: "guide"},
				{Path: "assets/image.png", Size: 10},
			},
		},
		harness.Skill{
			Name:        "user-only",
			Description: "User path",
			Content:     "instructions",
			Invocation:  harness.SkillInvocationUserOnly,
		},
		harness.Skill{
			Name:        "model-only",
			Description: "Model path",
			Content:     "instructions",
			Invocation:  harness.SkillInvocationModelOnly,
		},
		harness.Skill{
			Name:        "disabled",
			Description: "Neither path",
			Content:     "instructions",
			Invocation:  harness.SkillInvocationDisabled,
		},
	)
	require.NoError(t, err)

	userCatalog, err := catalog.ForUser()
	require.NoError(t, err)
	modelCatalog, err := catalog.ForModel()
	require.NoError(t, err)
	toolCatalog, err := catalog.ForModelWith("user-only")
	require.NoError(t, err)

	assert.Equal(t, []string{"both", "user-only"}, skillNames(userCatalog.List()))
	assert.Equal(t, []string{"both", "model-only"}, skillNames(modelCatalog.List()))
	assert.Equal(t, []string{"both", "model-only", "user-only"}, skillNames(toolCatalog.List()))

	listed := catalog.List()
	require.NotEmpty(t, listed[0].Resources)
	assert.Empty(t, listed[0].Resources[0].Content)

	resource, err := catalog.Resource("both", "references/guide.md")
	require.NoError(t, err)
	assert.Equal(t, "guide", resource.Content)

	resource.Content = "mutated"
	resource, err = catalog.Resource("both", "references/guide.md")
	require.NoError(t, err)
	assert.Equal(t, "guide", resource.Content)

	_, err = catalog.Resource("both", "missing.md")
	require.ErrorContains(t, err, `unknown resource "missing.md"`)
}

func skillNames(skills []harness.Skill) []string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
	}

	return names
}
