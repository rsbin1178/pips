package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIBuiltinToolFactories(t *testing.T) {
	t.Parallel()

	t.Run("web search", func(t *testing.T) {
		t.Parallel()

		tool := openai.WebSearch()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "web_search", tool.Name)
		assert.Equal(t, "web_search", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
		assert.True(t, tool.IsProviderExecuted())
	})

	t.Run("web search preview", func(t *testing.T) {
		t.Parallel()

		tool := openai.WebSearchPreview()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "web_search_preview", tool.Name)
		assert.Equal(t, "web_search_preview", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
	})

	t.Run("disabled with option", func(t *testing.T) {
		t.Parallel()

		tool := openai.WebSearch(openai.WithEnabled(false))
		assert.False(t, tool.IsEnabled())
		assert.True(t, tool.Disabled)
	})

	t.Run("code interpreter", func(t *testing.T) {
		t.Parallel()

		tool := openai.CodeInterpreter()
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "code_interpreter", tool.Name)
		assert.Equal(t, "code_interpreter", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
	})

	t.Run("file search with vector store IDs", func(t *testing.T) {
		t.Parallel()

		tool := openai.FileSearch([]string{"vs_abc", "vs_xyz"})
		assert.Equal(t, ai.ToolKindProviderExecuted, tool.Kind)
		assert.Equal(t, "file_search", tool.Name)
		assert.Equal(t, "file_search", tool.ProviderType)
		assert.True(t, tool.IsEnabled())
		data, ok := tool.ProviderData.(openai.FileSearchData)
		require.True(t, ok)
		assert.Equal(t, []string{"vs_abc", "vs_xyz"}, data.VectorStoreIDs)
	})
}

func TestOpenAIBuiltinToolsWireResponses(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_123",
			"model": "gpt-4o",
			"status": "completed",
			"output": [{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "hello"}]
			}]
		}`))
	}))
	defer server.Close()

	model := openai.New("gpt-4o",
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIResponses),
	)

	t.Run("emits built-in and function tools with correct wire shape", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "get_weather", Description: "weather tool"},
				openai.WebSearch(),
				openai.CodeInterpreter(),
				openai.FileSearch([]string{"vs_1"}),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 4)

		// Tool 0: function
		t0 := rawTools[0].(map[string]any)
		assert.Equal(t, "function", t0["type"])
		assert.Equal(t, "get_weather", t0["name"])

		// Tool 1: web_search
		t1 := rawTools[1].(map[string]any)
		assert.Equal(t, "web_search", t1["type"])
		assert.Nil(t, t1["name"])

		// Tool 2: code_interpreter
		t2 := rawTools[2].(map[string]any)
		assert.Equal(t, "code_interpreter", t2["type"])
		assert.Nil(t, t2["name"])

		// Tool 3: file_search
		t3 := rawTools[3].(map[string]any)
		assert.Equal(t, "file_search", t3["type"])
		assert.Nil(t, t3["name"])
		assert.Equal(t, []any{"vs_1"}, t3["vector_store_ids"])
	})

	t.Run("skips disabled tool", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "get_weather", Description: "weather tool"},
				openai.WebSearch(openai.WithEnabled(false)),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 1)
		t0 := rawTools[0].(map[string]any)
		assert.Equal(t, "function", t0["type"])
	})

	t.Run("request option DisableBuiltinTools filters out provider tools", func(t *testing.T) {
		capturedBody = nil

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "get_weather", Description: "weather tool"},
				openai.WebSearch(),
				openai.CodeInterpreter(),
			},
			ProviderOptions: map[ai.Provider]any{
				ai.ProviderOpenAI: openai.RequestOptions{
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
		assert.Equal(t, "get_weather", t0["name"])
	})
}

func TestOpenAIBuiltinToolsGatingModes(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_123",
			"model": "test-model",
			"status": "completed",
			"output": [{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "hello"}]
			}]
		}`))
	}))
	defer server.Close()

	t.Run("BuiltinToolsStrip strips built-in tools silently", func(t *testing.T) {
		capturedBody = nil

		model := openai.New("test-model",
			openai.WithAPIKey("test-key"),
			openai.WithBaseURL(server.URL),
			openai.WithAllowHTTP(),
			openai.WithAllowPrivateIPs(),
			openai.WithAPI(openai.APIResponses),
			openai.WithCompatibility(openai.Compatibility{
				BuiltinTools: openai.BuiltinToolsStrip,
			}),
		)

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "calc", Description: "calc"},
				openai.WebSearch(),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 1)
		t0 := rawTools[0].(map[string]any)
		assert.Equal(t, "calc", t0["name"])
	})

	t.Run("BuiltinToolsReject returns ErrUnsupported", func(t *testing.T) {
		model := openai.New("test-model",
			openai.WithAPIKey("test-key"),
			openai.WithBaseURL(server.URL),
			openai.WithAllowHTTP(),
			openai.WithAllowPrivateIPs(),
			openai.WithAPI(openai.APIResponses),
			openai.WithCompatibility(openai.Compatibility{
				BuiltinTools: openai.BuiltinToolsReject,
			}),
		)

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("test")},
			Tools: []ai.Tool{
				{Name: "calc", Description: "calc"},
				openai.WebSearch(),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.Error(t, err)
		assert.ErrorIs(t, err, ai.ErrUnsupported)
	})
}

func TestOpenAIChatCompletionsBuiltinTools(t *testing.T) {
	t.Parallel()

	var capturedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_123",
			"model": "gpt-4o-mini",
			"choices": [{
				"message": {"role": "assistant", "content": "hi"},
				"finish_reason": "stop"
			}]
		}`))
	}))
	defer server.Close()

	t.Run("rejects provider tools when BuiltinToolsAllow on Chat Completions", func(t *testing.T) {
		model := openai.New("gpt-4o-mini",
			openai.WithAPIKey("test-key"),
			openai.WithBaseURL(server.URL),
			openai.WithAllowHTTP(),
			openai.WithAllowPrivateIPs(),
			openai.WithAPI(openai.APIChatCompletions),
		)

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("hi")},
			Tools:    []ai.Tool{openai.WebSearch()},
		}

		_, err := model.Generate(context.Background(), req)
		require.Error(t, err)
		assert.ErrorIs(t, err, ai.ErrUnsupported)
		assert.Contains(t, err.Error(), "requires Responses API")
	})

	t.Run("strips provider tools when BuiltinToolsStrip on Chat Completions", func(t *testing.T) {
		capturedBody = nil

		model := openai.New("gpt-4o-mini",
			openai.WithAPIKey("test-key"),
			openai.WithBaseURL(server.URL),
			openai.WithAllowHTTP(),
			openai.WithAllowPrivateIPs(),
			openai.WithAPI(openai.APIChatCompletions),
			openai.WithCompatibility(openai.Compatibility{
				BuiltinTools: openai.BuiltinToolsStrip,
			}),
		)

		req := ai.Request{
			Messages: ai.Messages{ai.UserText("hi")},
			Tools: []ai.Tool{
				{Name: "my_tool", Description: "local tool"},
				openai.WebSearch(),
			},
		}

		_, err := model.Generate(context.Background(), req)
		require.NoError(t, err)

		rawTools, ok := capturedBody["tools"].([]any)
		require.True(t, ok)
		require.Len(t, rawTools, 1)
	})
}

func TestOpenAIResponsesCitationsAndGrounding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_grounding_1",
			"model": "gpt-4o",
			"status": "completed",
			"output": [
				{
					"type": "web_search_call",
					"id": "call_ws_1",
					"action": {
						"query": "golang release schedule"
					}
				},
				{
					"type": "message",
					"role": "assistant",
					"content": [
						{
							"type": "output_text",
							"text": "Go is released twice a year.",
							"annotations": [
								{
									"type": "url_citation",
									"url": "https://go.dev/doc/devel/release",
									"title": "Go Release History",
									"start_index": 0,
									"end_index": 28
								}
							]
						}
					]
				}
			]
		}`))
	}))
	defer server.Close()

	model := openai.New("gpt-4o",
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIResponses),
	)

	req := ai.Request{
		Messages: ai.Messages{ai.UserText("When is Go released?")},
		Tools:    []ai.Tool{openai.WebSearch()},
	}

	resp, err := model.Generate(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, resp.Citations, 1)
	assert.Equal(t, "https://go.dev/doc/devel/release", resp.Citations[0].URL)
	assert.Equal(t, "Go Release History", resp.Citations[0].Title)
	require.NotNil(t, resp.Citations[0].TextRange)
	assert.Equal(t, 0, resp.Citations[0].TextRange.Start)
	assert.Equal(t, 28, resp.Citations[0].TextRange.End)

	require.NotNil(t, resp.Grounding)
	assert.Equal(t, []string{"golang release schedule"}, resp.Grounding.WebSearchQueries)
}

func TestOpenAIResponsesStreamingCitations(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		events := []string{
			`data: {"type": "response.created", "response": {"id": "resp_s1", "model": "gpt-4o"}}` + "\n\n",
			`data: {"type": "response.output_text.delta", "delta": "Go 1.25 is out."}` + "\n\n",
			`data: {"type": "response.output_item.done", "item": {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Go 1.25 is out.", "annotations": [{"type": "url_citation", "url": "https://go.dev", "title": "Go", "start_index": 0, "end_index": 15}]}]}}` + "\n\n",
			`data: {"type": "response.completed", "response": {"id": "resp_s1", "model": "gpt-4o", "status": "completed", "output": [{"type": "web_search_call", "action": {"query": "go 1.25"}}]}}` + "\n\n",
		}

		for _, ev := range events {
			_, _ = w.Write([]byte(ev))
			flusher.Flush()
		}
	}))
	defer server.Close()

	model := openai.New("gpt-4o",
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIResponses),
	)

	req := ai.Request{
		Messages: ai.Messages{ai.UserText("test stream")},
		Tools:    []ai.Tool{openai.WebSearch()},
	}

	stream := model.Stream(context.Background(), req)

	var citations []ai.Citation
	for ev, err := range stream {
		require.NoError(t, err)
		if ev.Type == ai.StreamCitation && ev.Citation != nil {
			citations = append(citations, *ev.Citation)
		}
	}

	require.Len(t, citations, 1)
	assert.Equal(t, "https://go.dev", citations[0].URL)
	assert.Equal(t, "Go", citations[0].Title)
}
