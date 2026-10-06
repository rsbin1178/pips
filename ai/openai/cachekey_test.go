package openai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheKeyModel builds a model on a local server with an explicit compatibility
// profile, so a test can control whether the endpoint claims prompt-cache-key
// support.
func cacheKeyModel(t *testing.T, handler http.HandlerFunc, api openai.API, compat openai.Compatibility) *openai.Model {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New("test-model",
		openai.WithAPIKey("sk-test"),
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(api),
		openai.WithCompatibility(compat),
	)
}

// TestChatForwardsPromptCacheKeyOnlyWhenTheProfileDeclaresSupport pins the
// opt-in contract. Several OpenAI-compatible endpoints answer prompt_cache_key
// with a 400 or 422 instead of ignoring it, so the field travels only from a
// profile that documents support, and only when the request carries a key.
func TestChatForwardsPromptCacheKeyOnlyWhenTheProfileDeclaresSupport(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		compat    openai.Compatibility
		key       string
		wantField bool
	}{
		{
			name:   "declared support forwards the session key",
			compat: openai.Compatibility{PromptCacheKey: true},
			key:    "session-abc", wantField: true,
		},
		{
			name:   "an undeclared profile omits the field",
			compat: openai.Compatibility{},
			key:    "session-abc",
		},
		{
			name:   "an empty key sends nothing",
			compat: openai.Compatibility{PromptCacheKey: true},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any
			model := cacheKeyModel(
				t,
				serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured),
				openai.APIChatCompletions,
				testCase.compat,
			)

			_, err := model.Generate(t.Context(), ai.Request{
				Messages:       []ai.Message{ai.UserText("hi")},
				PromptCacheKey: testCase.key,
			})
			require.NoError(t, err)

			value, ok := captured["prompt_cache_key"]
			if !testCase.wantField {
				assert.False(t, ok, "prompt_cache_key must be absent")

				return
			}
			assert.Equal(t, testCase.key, value)
		})
	}
}

func TestResponsesForwardsPromptCacheKey(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	model := cacheKeyModel(
		t,
		serveResponsesJSON(t, responsesTextResponse, &captured),
		openai.APIResponses,
		openai.Compatibility{PromptCacheKey: true},
	)

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:       []ai.Message{ai.UserText("hi")},
		PromptCacheKey: "session-xyz",
	})
	require.NoError(t, err)

	assert.Equal(t, "session-xyz", captured["prompt_cache_key"])
}
