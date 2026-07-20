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
}
