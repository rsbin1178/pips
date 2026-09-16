package compat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openai/compat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrokBuiltinToolFactories(t *testing.T) {
	t.Parallel()

	t.Run("grok web search", func(t *testing.T) {
		t.Parallel()

		tool := compat.GrokWebSearch()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "web_search", tool.Name)
		assert.Equal(t, "web_search", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
		assert.True(t, tool.IsProviderExecuted())
	})

	t.Run("grok x search", func(t *testing.T) {
		t.Parallel()

		tool := compat.GrokXSearch()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "x_search", tool.Name)
		assert.Equal(t, "x_search", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
		assert.True(t, tool.IsProviderExecuted())
	})

	t.Run("grok code execution", func(t *testing.T) {
		t.Parallel()

		tool := compat.GrokCodeExecution()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "code_execution", tool.Name)
		assert.Equal(t, "code_execution", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
	})

	t.Run("disabled with option", func(t *testing.T) {
		t.Parallel()

		tool := compat.GrokWebSearch(openai.WithEnabled(false))
		assert.False(t, tool.IsEnabled())
		assert.True(t, tool.Disabled)
	})
}

func TestGrokBuiltinToolsWireResponses(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_grok_1",
			"model": "grok-beta",
			"status": "completed",
			"output": [{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "here is information from X and web"}]
			}]
		}`))
	}))
	defer server.Close()

	model := compat.XAI("grok-beta",
		openai.WithAPIKey("test-xai-key"),
		openai.WithBaseURL(server.URL),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	)

	t.Run("emits web_search and x_search with Responses wire shape", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("search x")},
			Tools: []ai.Tool{
				{Name: "local_fn", Description: "local helper"},
				compat.GrokWebSearch(),
				compat.GrokXSearch(),
				compat.GrokCodeExecution(),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 4)

		t0 := rawTools[0].(map[string]any)
		assert.Equal(t, "function", t0["type"])
		assert.Equal(t, "local_fn", t0["name"])

		t1 := rawTools[1].(map[string]any)
		assert.Equal(t, "web_search", t1["type"])
		assert.Nil(t, t1["name"])

		t2 := rawTools[2].(map[string]any)
		assert.Equal(t, "x_search", t2["type"])
		assert.Nil(t, t2["name"])

		t3 := rawTools[3].(map[string]any)
		assert.Equal(t, "code_execution", t3["type"])
		assert.Nil(t, t3["name"])
	})

	t.Run("request option DisableBuiltinTools filters Grok tools", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "local_fn", Description: "local helper"},
				compat.GrokWebSearch(),
				compat.GrokXSearch(),
			},
			ProviderOptions: map[ai.Provider]any{
				ai.ProviderXAI: openai.RequestOptions{
					DisableBuiltinTools: true,
				},
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 1)
		t0 := rawTools[0].(map[string]any)
		assert.Equal(t, "local_fn", t0["name"])
	})
}
