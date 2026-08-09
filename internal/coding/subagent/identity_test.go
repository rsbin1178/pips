package subagent

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding/agentprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateExecutionPlanDelegationLineage(t *testing.T) {
	t.Parallel()

	plan := testDelegationExecutionPlan(t)
	require.NoError(t, ValidateExecutionPlan(plan))
	digest, err := plan.Digest()
	require.NoError(t, err)

	cloned := plan.Clone()
	cloned.Ancestry[0] = "changed"
	cloned.DelegationTargets[0] = "changed-target"

	assert.Equal(t, "root-agent", plan.Ancestry[0])
	assert.Equal(t, "leaf-agent", plan.DelegationTargets[0])

	changedDigest, err := cloned.Digest()
	require.NoError(t, err)
	assert.NotEqual(t, digest, changedDigest)

	for name, mutate := range map[string]func(*ExecutionPlan){
		"builtin":         func(value *ExecutionPlan) { value.Identity.Kind = AgentKindBuiltin },
		"depth overflow":  func(value *ExecutionPlan) { value.DelegationDepth = 4; value.MaxDelegationDepth = 4 },
		"length mismatch": func(value *ExecutionPlan) { value.Ancestry = []string{"root-agent"} },
		"wrong leaf":      func(value *ExecutionPlan) { value.Ancestry[1] = "other-agent" },
		"cycle":           func(value *ExecutionPlan) { value.Ancestry[1] = "root-agent" },
		"ancestor target": func(value *ExecutionPlan) { value.DelegationTargets[0] = "root-agent" },
		"duplicate target": func(value *ExecutionPlan) {
			value.DelegationTargets = []string{"leaf-agent", "leaf-agent"}
		},
		"target at limit": func(value *ExecutionPlan) {
			value.DelegationDepth = value.MaxDelegationDepth
			value.Ancestry = []string{"root-agent", "middle-agent", value.Identity.ID}
		},
		"background": func(value *ExecutionPlan) { value.Delivery = DeliveryBackground },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			invalid := plan.Clone()
			mutate(&invalid)
			require.ErrorIs(t, ValidateExecutionPlan(invalid), ErrInvalid)
		})
	}
}

func TestValidateExecutionPlanAcceptsPreRecursionPlan(t *testing.T) {
	t.Parallel()

	plan := testDelegationExecutionPlan(t)
	plan.DelegationDepth = 0
	plan.MaxDelegationDepth = 0
	plan.Ancestry = nil
	plan.DelegationTargets = nil
	require.NoError(t, ValidateExecutionPlan(plan))
}

func testDelegationExecutionPlan(t *testing.T) ExecutionPlan {
	t.Helper()

	identity, err := IdentityFromDefinition(agentprofile.Definition{
		ID: "middle-agent", Kind: agentprofile.KindCustom, Scope: agentprofile.ScopeUserPips,
		Source: "user:pips/middle-agent.md", Digest: strings.Repeat("a", 64),
		Schema: agentprofile.SchemaV1Alpha1, Name: "Middle Agent",
	})
	require.NoError(t, err)
	output, err := NewOutputContract(OutputFormatText, "middle-agent-result", nil)
	require.NoError(t, err)

	instructions := "Follow the immutable execution plan."

	return ExecutionPlan{
		Schema:             ExecutionPlanSchema,
		Identity:           identity,
		GenerationID:       1,
		Delivery:           DeliveryForeground,
		Model:              "test/model",
		Instructions:       instructions,
		InstructionsDigest: InstructionsDigest(instructions),
		Capabilities:       []EffectiveCapability{},
		Skills:             []string{},
		PreloadedSkills:    []string{},
		DelegationDepth:    1,
		MaxDelegationDepth: 3,
		Ancestry:           []string{"root-agent", "middle-agent"},
		DelegationTargets:  []string{"leaf-agent"},
		Limits:             NormalizeLimits(ProductionLimits()),
		Output:             output,
	}
}
