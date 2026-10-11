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
	limits.RepeatedToolCallLimit = 17
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

func TestProductionLimitsBoundExecutionWithoutChangingEmbedderDefaults(t *testing.T) {
	t.Parallel()

	embedder := DefaultLimits()
	assert.Zero(t, embedder.MaxTurns)
	assert.Zero(t, embedder.MaxTokens)
	assert.Zero(t, embedder.MaxToolCalls)
	assert.Zero(t, embedder.MaxDuration)
	require.NoError(t, validateLimits(embedder))

	production := ProductionLimits()
	assert.Positive(t, production.MaxTurns)
	assert.Positive(t, production.FinalizationTurns)
	assert.Positive(t, production.MaxTokens)
	assert.Positive(t, production.MaxToolCalls)
	assert.Positive(t, production.MaxDuration)
	assert.Equal(t, embedder.MaxOutputTokens, production.MaxOutputTokens)
	assert.Equal(t, embedder.MaxResultBytes, production.MaxResultBytes)
	require.NoError(t, validateLimits(production))
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
