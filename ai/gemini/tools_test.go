package gemini_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiBuiltinToolFactories(t *testing.T) {
	t.Parallel()

	t.Run("google search enabled by default", func(t *testing.T) {
		t.Parallel()

		tool := gemini.GoogleSearch()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "google_search", tool.Name)
		assert.Equal(t, "google_search", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
		assert.True(t, tool.IsProviderExecuted())
	})

	t.Run("google search disabled with option", func(t *testing.T) {
		t.Parallel()

		tool := gemini.GoogleSearch(gemini.WithEnabled(false))
		assert.False(t, tool.IsEnabled())
		assert.True(t, tool.Disabled)
	})

	t.Run("code execution enabled by default", func(t *testing.T) {
		t.Parallel()

		tool := gemini.CodeExecution()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "code_execution", tool.Name)
		assert.Equal(t, "code_execution", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
	})
}

func TestGeminiBuiltinToolsWireRequest(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "hello"}]},
				"finishReason": "STOP"
			}]
		}`))
	}))
	defer server.Close()

	model := gemini.New("gemini-2.5-flash",
		gemini.WithAPIKey("test-key"),
		gemini.WithBaseURL(server.URL),
		gemini.WithAllowHTTP(),
		gemini.WithAllowPrivateIPs(),
	)

	t.Run("emits google_search and code_execution alongside function declarations", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "get_weather", Description: "weather tool"},
				gemini.GoogleSearch(),
				gemini.CodeExecution(),
			},
		}

		resp, err := model.Generate(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, "hello", resp.Text())

		toolsRaw, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, toolsRaw, 3)

		// Verify function declarations
		funcTool := toolsRaw[0].(map[string]any)
		assert.Contains(t, funcTool, "functionDeclarations")

		// Verify google_search tool
		searchTool := toolsRaw[1].(map[string]any)
		assert.Contains(t, searchTool, "google_search")

		// Verify code_execution tool
		codeTool := toolsRaw[2].(map[string]any)
		assert.Contains(t, codeTool, "code_execution")
	})

	t.Run("disables via request options", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				gemini.GoogleSearch(),
				gemini.CodeExecution(),
			},
			ProviderOptions: map[ai.Provider]any{
				ai.ProviderGemini: gemini.RequestOptions{
					DisableSearchGrounding: true,
					DisableCodeExecution:   true,
				},
			},
		}

		_, err := model.Generate(t.Context(), req)
		require.NoError(t, err)

		// Tools should be omitted completely when all provider tools are disabled
		assert.Nil(t, capturedBody["tools"])
	})

	t.Run("disabled tool is omitted", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				gemini.GoogleSearch(gemini.WithEnabled(false)),
			},
		}

		_, err := model.Generate(t.Context(), req)
		require.NoError(t, err)
		assert.Nil(t, capturedBody["tools"])
	})
}

func TestGeminiGroundingCitationsExtraction(t *testing.T) {
	t.Parallel()

	responseJSON := `{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [{"text": "Go 1.26 was released with new features."}]
			},
			"finishReason": "STOP",
			"groundingMetadata": {
				"webSearchQueries": ["golang 1.26 release notes"],
				"groundingChunks": [
					{"web": {"uri": "https://go.dev/doc/go1.26", "title": "Go 1.26 Release Notes"}},
					{"web": {"uri": "https://tip.golang.org", "title": "Go Tip Documentation"}}
				],
				"groundingSupports": [
					{
						"groundingChunkIndices": [0],
						"segment": {
							"startIndex": 0,
							"endIndex": 38,
							"text": "Go 1.26 was released with new features."
						}
					}
				]
			}
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 15
		}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseJSON))
	}))
	defer server.Close()

	model := gemini.New("gemini-2.5-flash",
		gemini.WithAPIKey("test-key"),
		gemini.WithBaseURL(server.URL),
		gemini.WithAllowHTTP(),
		gemini.WithAllowPrivateIPs(),
	)

	req := ai.Request{
		Messages: ai.Messages{ai.UserText("What is new in Go 1.26?")},
		Tools:    []ai.Tool{gemini.GoogleSearch()},
	}

	resp, err := model.Generate(t.Context(), req)
	require.NoError(t, err)

	require.Len(t, resp.Citations, 2)
	assert.Equal(t, "https://go.dev/doc/go1.26", resp.Citations[0].URL)
	assert.Equal(t, "Go 1.26 Release Notes", resp.Citations[0].Title)
	assert.Equal(t, 0, resp.Citations[0].Index)
	require.NotNil(t, resp.Citations[0].TextRange)
	assert.Equal(t, 0, resp.Citations[0].TextRange.Start)
	assert.Equal(t, 38, resp.Citations[0].TextRange.End)
	assert.Equal(t, "Go 1.26 was released with new features.", resp.Citations[0].Snippet)

	assert.Equal(t, "https://tip.golang.org", resp.Citations[1].URL)
	assert.Equal(t, "Go Tip Documentation", resp.Citations[1].Title)

	require.NotNil(t, resp.Grounding)
	assert.Equal(t, []string{"golang 1.26 release notes"}, resp.Grounding.WebSearchQueries)
}
