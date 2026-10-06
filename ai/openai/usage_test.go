package openai

import (
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeUsage unmarshals one usage payload into the wire struct, so the test
// covers the JSON tags as well as the normalization.
func decodeUsage[T any](t *testing.T, body string) *T {
	t.Helper()

	var parsed struct {
		Usage *T `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))

	return parsed.Usage
}

// TestUsageFromChatReadsEveryCacheShape pins the prompt-cache field matrix of
// the OpenAI-compatible endpoints pips drives: OpenAI's nested details object,
// DeepSeek's native hit/miss counters, and the top-level cached_tokens alias
// Kimi/Moonshot and DashScope report, plus cache writes for models with
// explicit caching.
func TestUsageFromChatReadsEveryCacheShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want ai.Usage
	}{
		{
			name: "openai nested details",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":5,
				"prompt_tokens_details":{"cached_tokens":60}}}`,
			want: ai.Usage{InputTokens: 100, OutputTokens: 5, CachedInputTokens: 60},
		},
		{
			name: "deepseek native hit and miss counters",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":5,
				"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`,
			want: ai.Usage{InputTokens: 100, OutputTokens: 5, CachedInputTokens: 64},
		},
		{
			name: "deepseek counters reconstruct an omitted prompt size",
			body: `{"usage":{"completion_tokens":5,
				"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`,
			want: ai.Usage{InputTokens: 100, OutputTokens: 5, CachedInputTokens: 64},
		},
		{
			name: "kimi and dashscope top-level alias",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":5,"cached_tokens":80}}`,
			want: ai.Usage{InputTokens: 100, OutputTokens: 5, CachedInputTokens: 80},
		},
		{
			name: "a zeroed nested reading cannot hide a real alias",
			body: `{"usage":{"prompt_tokens":100,"cached_tokens":90,
				"prompt_tokens_details":{"cached_tokens":0}}}`,
			want: ai.Usage{InputTokens: 100, CachedInputTokens: 90},
		},
		{
			name: "openrouter cache writes",
			body: `{"usage":{"prompt_tokens":100,"prompt_tokens_details":
				{"cached_tokens":60,"cache_write_tokens":25}}}`,
			want: ai.Usage{InputTokens: 100, CachedInputTokens: 60, CacheWriteTokens: 25},
		},
		{
			name: "no cache fields reported",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":5}}`,
			want: ai.Usage{InputTokens: 100, OutputTokens: 5},
		},
		{
			name: "usage absent",
			body: `{}`,
			want: ai.Usage{},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.want, usageFromChat(decodeUsage[chatUsage](t, testCase.body)))
		})
	}
}

// TestUsageFromResponsesReadsCacheWrites pins the Responses shape, where the
// details object lives under input_tokens_details.
func TestUsageFromResponsesReadsCacheWrites(t *testing.T) {
	t.Parallel()

	usage := decodeUsage[responsesUsage](t, `{"usage":{"input_tokens":100,"output_tokens":5,
		"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":25}}}`)

	assert.Equal(t, ai.Usage{
		InputTokens:       100,
		OutputTokens:      5,
		CachedInputTokens: 60,
		CacheWriteTokens:  25,
	}, usageFromResponses(usage))
}
