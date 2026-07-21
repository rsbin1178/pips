//nolint:wsl_v5 // Merge fixtures keep assertions adjacent to type checks.
package jsonx_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/rsbin/pips/ai/internal/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeExtraFieldsRecursivelyAddsWithoutMutation(t *testing.T) {
	t.Parallel()

	body := struct {
		Config map[string]any `json:"config"`
	}{Config: map[string]any{"temperature": 0.2}}
	extra := map[string]any{
		"config":       map[string]any{"mediaResolution": "high"},
		"service_tier": "flex",
	}

	merged, err := jsonx.MergeExtraFields(body, extra, "config.temperature")
	require.NoError(t, err)
	result, ok := merged.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "flex", result["service_tier"])
	configValue, ok := result["config"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "high", configValue["mediaResolution"])
	assert.NotContains(t, body.Config, "mediaResolution")
}

func TestMergeExtraFieldsRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		extra    map[string]any
		reserved []string
	}{
		{name: "collision", extra: map[string]any{"model": "override"}},
		{name: "reserved absent", extra: map[string]any{"temperature": 1}, reserved: []string{"temperature"}},
		{name: "nested reserved", extra: map[string]any{"config": map[string]any{"temperature": 1}}, reserved: []string{"config.temperature"}},
		{name: "credential", extra: map[string]any{"nested": map[string]any{"api_key": "secret"}}},
		{name: "credential token", extra: map[string]any{"nested": map[string]any{"refresh_token": "secret"}}},
		{name: "non finite", extra: map[string]any{"value": math.Inf(1)}},
		{name: "datetime", extra: map[string]any{"value": time.Now()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jsonx.MergeExtraFields(
				map[string]any{"model": "gpt"},
				tt.extra,
				tt.reserved...,
			)
			require.ErrorIs(t, err, jsonx.ErrUnsafeExtension)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestMergeExtraFieldsPreservesLargeInteger(t *testing.T) {
	t.Parallel()

	merged, err := jsonx.MergeExtraFields(
		struct{}{},
		map[string]any{"seed": int64(9007199254740993)},
	)
	require.NoError(t, err)
	result, ok := merged.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, json.Number("9007199254740993"), result["seed"])
}

func TestMergeExtraFieldsLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		extra  map[string]any
		limits jsonx.MergeLimits
	}{
		{name: "bytes", extra: map[string]any{"key": "value"}, limits: jsonx.MergeLimits{MaxBytes: 2}},
		{name: "depth", extra: map[string]any{"one": map[string]any{"two": true}}, limits: jsonx.MergeLimits{MaxDepth: 2}},
		{name: "nodes", extra: map[string]any{"one": true}, limits: jsonx.MergeLimits{MaxNodes: 1}},
		{name: "object keys", extra: map[string]any{"one": true, "two": true}, limits: jsonx.MergeLimits{MaxObjectKeys: 1}},
		{name: "array items", extra: map[string]any{"values": []any{1, 2}}, limits: jsonx.MergeLimits{MaxArrayItems: 1}},
		{name: "key bytes", extra: map[string]any{"long": true}, limits: jsonx.MergeLimits{MaxKeyBytes: 2}},
		{name: "string bytes", extra: map[string]any{"value": "long"}, limits: jsonx.MergeLimits{MaxStringBytes: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := jsonx.MergeExtraFieldsWithLimits(struct{}{}, tt.extra, tt.limits)
			require.ErrorIs(t, err, jsonx.ErrUnsafeExtension)
		})
	}
}

func FuzzMergeExtraFieldsDoesNotMutateInput(f *testing.F) {
	f.Add([]byte(`{"service_tier":"flex"}`))
	f.Add([]byte(`{"nested":{"value":1},"items":[true,null,"ok"]}`))
	f.Add([]byte(`{"model":"override"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var extra map[string]any
		if err := json.Unmarshal(data, &extra); err != nil || extra == nil {
			return
		}
		before, err := json.Marshal(extra)
		if err != nil {
			return
		}

		merged, _ := jsonx.MergeExtraFields(
			map[string]any{"model": "fixed"},
			extra,
			"model",
		)
		after, err := json.Marshal(extra)
		require.NoError(t, err)
		assert.JSONEq(t, string(before), string(after))
		if result, ok := merged.(map[string]any); ok {
			assert.Equal(t, "fixed", result["model"])
		}
	})
}
