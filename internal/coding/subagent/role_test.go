package subagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinPromptAndResultSchema(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"explore", "plan", "review"} {
		prompt, err := BuiltinPrompt(id, false, true)
		require.NoError(t, err, id)
		assert.Contains(t, prompt, "Return your final answer as plain text", id)

		schema, ok := BuiltinResultSchema(id)
		require.True(t, ok, id)
		assert.True(t, json.Valid(schema), id)

		structured, err := BuiltinPrompt(id, true, true)
		require.NoError(t, err, id)
		assert.Contains(t, structured, "Return only the JSON object required by your response schema", id)

		portable, err := BuiltinPrompt(id, true, false)
		require.NoError(t, err, id)
		assert.Contains(t, portable, "Native structured output is unavailable", id)
	}

	prompt, err := BuiltinPrompt("general-purpose", true, true)
	require.NoError(t, err)
	assert.Contains(t, prompt, "Return your final answer as plain text")

	_, ok := BuiltinResultSchema("general-purpose")
	assert.False(t, ok)

	_, err = BuiltinPrompt("unknown", false, true)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestBuiltinResultSchemaMatchesRoleStruct(t *testing.T) {
	t.Parallel()

	schema, ok := BuiltinResultSchema("explore")
	require.True(t, ok)

	var decoded struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(schema, &decoded))
	assert.Equal(t, "object", decoded.Type)
	assert.Contains(t, decoded.Properties, "summary")
	assert.Contains(t, decoded.Properties, "evidence")
	assert.Contains(t, decoded.Properties, "unknowns")
}

func TestDispatchRequestValidation(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	_, err := normalizeDispatchRequest(Request{AgentID: "explore", Task: "x"})
	require.NoError(t, err)
	_, err = normalizeDispatchRequest(Request{Task: "x"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = normalizeDispatchRequest(Request{AgentID: "Explore", Task: "x"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = normalizeDispatchRequest(Request{AgentID: "explore", Role: RolePlan, Task: "x"})
	require.ErrorIs(t, err, ErrInvalid)

	require.ErrorIs(t, validateDispatchRequest(Request{AgentID: "explore", Task: strings.Repeat("x", limits.MaxTaskBytes+1)}, limits), ErrInvalid)
	require.ErrorIs(t, validateDispatchRequest(Request{AgentID: "explore", Task: "x\x00y"}, limits), ErrInvalid)
	require.ErrorIs(t, validateDispatchRequest(Request{AgentID: "explore", Task: "  "}, limits), ErrInvalid)
	require.NoError(t, validateDispatchRequest(Request{AgentID: "explore", Task: "x"}, limits))
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
	limits.RepeatedToolCallLimit = maximumRepeatedToolCalls + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedProblematicToolCallLimit = maximumRepeatedToolCalls + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedToolCallStopLimit = maximumRepeatedToolCalls + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedProblematicToolCallStopLimit = maximumRepeatedToolCalls + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedToolCallLimit = limits.RepeatedToolCallStopLimit
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.RepeatedProblematicToolCallLimit = limits.RepeatedProblematicToolCallStopLimit
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxActivityTools = 1025
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxDuration = 24 * time.Hour
	require.NoError(t, validateLimits(limits))
	limits.MaxDuration++
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxTurns = maximumTurns + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxTokens = maximumTokens + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
	limits = DefaultLimits()
	limits.MaxToolCalls = maximumToolCalls + 1
	require.ErrorIs(t, validateLimits(limits), ErrInvalid)
}

// TestDefaultLimitsAreUnlimitedAndGuarded pins the Phase L default policy: the
// package default caps no execution, and only the payload and live-projection
// guards are positive.
func TestDefaultLimitsAreUnlimitedAndGuarded(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	assert.Zero(t, limits.MaxTurns)
	assert.Zero(t, limits.FinalizationTurns)
	assert.Zero(t, limits.MaxTokens)
	assert.Zero(t, limits.MaxToolCalls)
	assert.Zero(t, limits.MaxDuration)
	assert.Positive(t, limits.RepeatedToolCallLimit)
	assert.Positive(t, limits.RepeatedProblematicToolCallLimit)
	assert.Positive(t, limits.RepeatedToolCallStopLimit)
	assert.Positive(t, limits.RepeatedProblematicToolCallStopLimit)
	assert.Positive(t, limits.MaxActivityTools)
	assert.Positive(t, limits.MaxOutputTokens)
	assert.Positive(t, limits.MaxTaskBytes)
	assert.Positive(t, limits.MaxResultBytes)
	assert.Positive(t, limits.MaxResultItems)
	assert.Positive(t, limits.MaxFieldBytes)
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

// TestLimitsDoNotExpandCoversTheRepeatThresholds pins the authority boundary for
// the Phase M repeat-guard thresholds: a plan may not raise any of them above the
// Manager ceiling.
func TestLimitsDoNotExpandCoversTheRepeatThresholds(t *testing.T) {
	t.Parallel()

	ceiling := DefaultLimits()
	assert.True(t, limitsDoNotExpand(ceiling, ceiling))

	for _, test := range []struct {
		name   string
		mutate func(*Limits)
	}{
		{"nudge", func(value *Limits) { value.RepeatedToolCallLimit = ceiling.RepeatedToolCallLimit + 1 }},
		{"read nudge", func(value *Limits) {
			value.RepeatedProblematicToolCallLimit = ceiling.RepeatedProblematicToolCallLimit + 1
		}},
		{"stop", func(value *Limits) { value.RepeatedToolCallStopLimit = ceiling.RepeatedToolCallStopLimit + 1 }},
		{"read stop", func(value *Limits) {
			value.RepeatedProblematicToolCallStopLimit = ceiling.RepeatedProblematicToolCallStopLimit + 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			candidate := DefaultLimits()
			test.mutate(&candidate)
			assert.False(t, limitsDoNotExpand(ceiling, candidate))
		})
	}
}
