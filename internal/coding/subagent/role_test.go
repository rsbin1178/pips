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
			name: "path escape", role: RoleReview,
			text: `{"summary":"x","findings":[{"severity":"high","title":"x","path":"../secret","line":1,"evidence":"x","recommendation":"x"}],"residual_risks":[]}`,
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

func TestExploreResultOmitsInvalidEvidenceItems(t *testing.T) {
	t.Parallel()

	spec, err := specFor(RoleExplore)
	require.NoError(t, err)

	result, err := spec.decode(
		`{"summary":"Found the entry point.","evidence":[`+
			`{"path":"cmd/pips/main.go","start_line":10,"end_line":12,"claim":"Starts the CLI."},`+
			`{"path":"internal/coding/runtime.go","start_line":44,"end_line":0,"claim":"Invalid range."},`+
			`{"path":"../secret","start_line":1,"end_line":1,"claim":"Escapes the workspace."},`+
			`{"path":"internal/coding/open.go","start_line":20,"end_line":25,"claim":"Opens the runtime."}`+
			`],"unknowns":["The provider path was not inspected."]}`,
		DefaultLimits(),
	)
	require.NoError(t, err)

	want := ExploreResult{
		Summary: "Found the entry point.",
		Evidence: []Evidence{
			{
				Path: "cmd/pips/main.go", StartLine: 10, EndLine: 12,
				Claim: "Starts the CLI.",
			},
			{
				Path: "internal/coding/open.go", StartLine: 20, EndLine: 25,
				Claim: "Opens the runtime.",
			},
		},
		Unknowns: []string{
			"The provider path was not inspected.",
			exploreEvidenceOmissionNotice,
		},
	}
	assert.Equal(t, want, result)
}

func TestExploreResultAlwaysRecordsEvidenceOmissionWithinLimit(t *testing.T) {
	t.Parallel()

	spec, err := specFor(RoleExplore)
	require.NoError(t, err)

	limits := DefaultLimits()
	limits.MaxResultItems = 2

	result, err := spec.decode(
		`{"summary":"x","evidence":[`+
			`{"path":"bad.go","start_line":2,"end_line":1,"claim":"bad"}`+
			`],"unknowns":["first","second"]}`,
		limits,
	)
	require.NoError(t, err)

	want := ExploreResult{
		Summary:  "x",
		Evidence: []Evidence{},
		Unknowns: []string{"first", exploreEvidenceOmissionNotice},
	}
	assert.Equal(t, want, result)
}

func TestRoleResultsRequireExplicitCollections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role Role
		text string
	}{
		{
			name: "explore missing evidence", role: RoleExplore,
			text: `{"summary":"x","unknowns":[]}`,
		},
		{
			name: "plan null steps", role: RolePlan,
			text: `{"summary":"x","assumptions":[],"steps":null,"risks":[],"verification":[]}`,
		},
		{
			name: "plan step missing files", role: RolePlan,
			text: `{"summary":"x","assumptions":[],"steps":[{"title":"x","rationale":"x"}],"risks":[],"verification":[]}`,
		},
		{
			name: "review missing residual risks", role: RoleReview,
			text: `{"summary":"x","findings":[]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec, err := specFor(test.role)
			require.NoError(t, err)
			_, err = spec.decode(test.text, DefaultLimits())
			require.ErrorIs(t, err, ErrInvalidResult)
		})
	}
}

func TestRolePlainTextFallbackProducesSummaryOnlyResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role Role
		text string
		want any
	}{
		{
			name: "explore Chinese text",
			role: RoleExplore,
			text: "列出了工作区内容。",
			want: ExploreResult{
				Summary: "列出了工作区内容。", Evidence: []Evidence{}, Unknowns: []string{},
			},
		},
		{
			name: "plan sentence",
			role: RolePlan,
			text: "plan subagent 工作正常",
			want: PlanResult{
				Summary:      "plan subagent 工作正常",
				Assumptions:  []string{},
				Steps:        []PlanStep{},
				Risks:        []string{},
				Verification: []string{},
			},
		},
		{
			name: "review sentence",
			role: RoleReview,
			text: "review subagent 工作正常",
			want: ReviewResult{
				Summary:       "review subagent 工作正常",
				Findings:      []ReviewFinding{},
				ResidualRisks: []string{},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec, err := specFor(test.role)
			require.NoError(t, err)

			result, err := spec.decodeResult(test.text, DefaultLimits(), true)
			require.NoError(t, err)
			assert.Equal(t, test.want, result)
		})
	}
}

func TestRoleCompatibilityUnwrapsOnlyExactJSONFence(t *testing.T) {
	t.Parallel()

	spec, err := specFor(RoleExplore)
	require.NoError(t, err)

	result, err := spec.decodeResult(
		"```json\n"+
			`{"summary":"x","evidence":[],"unknowns":[]}`+
			"\n```",
		DefaultLimits(),
		true,
	)
	require.NoError(t, err)
	assert.Equal(t, ExploreResult{
		Summary: "x", Evidence: []Evidence{}, Unknowns: []string{},
	}, result)

	_, err = spec.decodeResult("```json\n{\"summary\":\n```", DefaultLimits(), true)
	require.ErrorIs(t, err, ErrInvalidResult)
	_, err = spec.decodeResult(
		"```json\n"+`{"summary":"x","evidence":[],"unknowns":[]}`+"\n```",
		DefaultLimits(),
		false,
	)
	require.ErrorIs(t, err, ErrInvalidResult)

	inner := `{"summary":"x","evidence":[],"unknowns":[]}`
	fenced := "```json\n" + inner + "\n```"
	limits := DefaultLimits()
	limits.MaxResultBytes = len(inner)
	limits.MaxFieldBytes = len(inner)
	_, err = spec.decodeResult(fenced, limits, true)
	require.ErrorIs(t, err, ErrInvalidResult)
}

func TestRolePlainTextFallbackKeepsJSONAndNativePathsStrict(t *testing.T) {
	t.Parallel()

	spec, err := specFor(RoleExplore)
	require.NoError(t, err)

	_, err = spec.decodeResult(`{"summary":`, DefaultLimits(), true)
	require.ErrorIs(t, err, ErrInvalidResult)
	_, err = spec.decodeResult(
		`{"summary":"x","evidence":[],"unknowns":[],"extra":true}`,
		DefaultLimits(),
		true,
	)
	require.ErrorIs(t, err, ErrInvalidResult)

	_, err = spec.decodeResult("plain summary", DefaultLimits(), false)
	require.ErrorIs(t, err, ErrInvalidResult)

	limits := DefaultLimits()
	_, err = spec.decodeResult(strings.Repeat("x", limits.MaxFieldBytes+1), limits, true)
	require.ErrorIs(t, err, ErrInvalidResult)

	invalidUTF8 := string([]byte{
		'{', '"', 's', 'u', 'm', 'm', 'a', 'r', 'y', '"', ':', '"', 0xff,
		'"', ',', '"', 'e', 'v', 'i', 'd', 'e', 'n', 'c', 'e', '"', ':', '[', ']',
		',', '"', 'u', 'n', 'k', 'n', 'o', 'w', 'n', 's', '"', ':', '[', ']', '}',
	})
	_, err = spec.decodeResult(invalidUTF8, DefaultLimits(), true)
	require.ErrorIs(t, err, ErrInvalidResult)
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
	require.NoError(t, validateLimits(limits))
	limits.MaxTurns = -1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxDuration = -time.Nanosecond
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxTurns = 3
	limits.FinalizationTurns = limits.MaxTurns
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.FinalizationTurns = 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedToolCallLimit = 17
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxActivityTools = 1025
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxDuration = 24 * time.Hour
	require.NoError(t, validateLimits(limits))
}

func TestNormalizeLimitsPreservesOlderPolicyRecords(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTurns = 26
	limits.FinalizationTurns = 0
	limits.RepeatedToolCallLimit = 0
	limits.MaxActivityTools = 0

	normalized := normalizeLimits(limits)
	assert.Equal(t, 1, normalized.FinalizationTurns)
	assert.Equal(t, DefaultLimits().RepeatedToolCallLimit, normalized.RepeatedToolCallLimit)
	assert.Equal(t, DefaultLimits().MaxActivityTools, normalized.MaxActivityTools)
	require.NoError(t, validateLimits(normalized))

	unlimited := DefaultLimits()
	unlimited.FinalizationTurns = 0
	assert.Zero(t, normalizeLimits(unlimited).FinalizationTurns)
}
