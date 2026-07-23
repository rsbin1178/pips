package subagent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoleResultsFailClosed(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()

	tests := []struct {
		name string
		role Role
		text string
	}{
		{
			name: "path escape", role: RoleExplore,
			text: `{"summary":"x","evidence":[{"path":"../secret","start_line":1,"end_line":1,"claim":"x"}],"unknowns":[]}`,
		},
		{
			name: "absolute path", role: RolePlan,
			text: `{"summary":"x","assumptions":[],"steps":[{"title":"x","files":["/tmp/x"],"rationale":"x"}],"risks":[],"verification":[]}`,
		},
		{
			name: "severity enum", role: RoleReview,
			text: `{"summary":"x","findings":[{"severity":"urgent","title":"x","path":"a.go","line":1,"evidence":"x","recommendation":"x"}],"residual_risks":[]}`,
		},
		{
			name: "unknown field", role: RoleExplore,
			text: `{"summary":"x","evidence":[],"unknowns":[],"extra":true}`,
		},
		{
			name: "trailing json", role: RoleReview,
			text: `{"summary":"x","findings":[],"residual_risks":[]} {}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec, err := specFor(test.role)
			require.NoError(t, err)
			_, err = spec.decode(test.text, limits)
			require.ErrorIs(t, err, ErrInvalidResult)
		})
	}
}

func TestRoleRequestAndResultBudgets(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	request := Request{Role: RoleExplore, Task: strings.Repeat("x", limits.MaxTaskBytes+1)}
	require.ErrorIs(t, validateRequest(request, limits), ErrInvalid)
	require.ErrorIs(t, validateRequest(Request{Role: "custom", Task: "x"}, limits), ErrInvalid)
	require.ErrorIs(t, validateRequest(Request{Role: RoleExplore, Task: "x\x00y"}, limits), ErrInvalid)

	spec, err := specFor(RoleExplore)
	require.NoError(t, err)
	_, err = spec.decode(strings.Repeat("x", limits.MaxResultBytes+1), limits)
	assert.ErrorIs(t, err, ErrInvalidResult)
}

func TestLimitsRejectUnsafeOverrides(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTurns = 0
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxDuration = 31 * time.Minute
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
}
