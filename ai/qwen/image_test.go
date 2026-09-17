package qwen_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/qwen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQwenImageAsyncTask(t *testing.T) {
	t.Parallel()

	var (
		submit map[string]any
		polls  atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/services/aigc/text2image/image-synthesis":
			assert.Equal(t, "enable", r.Header.Get("X-DashScope-Async"))

			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.NoError(t, json.Unmarshal(body, &submit))

			_, _ = w.Write([]byte(`{"output":{"task_id":"task-1","task_status":"PENDING"},"request_id":"req-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/tasks/task-1":
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"output":{"task_id":"task-1","task_status":"RUNNING"}}`))

				return
			}

			_, _ = w.Write([]byte(`{
				"request_id": "req-2",
				"output": {
					"task_id": "task-1",
					"task_status": "SUCCEEDED",
					"results": [{
						"orig_prompt": "a cat",
						"actual_prompt": "a cat, high quality",
						"url": "https://example.com/cat.png"
					}]
				},
				"usage": {"image_count": 1, "size": "1664*928"}
			}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	model := qwen.NewImageModel("qwen-image-plus",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
		qwen.WithPollInterval(time.Millisecond),
		qwen.WithPollTimeout(10*time.Second),
	)

	assert.Equal(t, ai.ProviderQwen, model.Provider())
	assert.True(t, model.Capabilities().ImageGeneration)

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a cat",
		N:      1,
		Size:   "1664x928",
	})
	require.NoError(t, err)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://example.com/cat.png", resp.Images[0].URL)
	assert.Equal(t, "a cat, high quality", resp.Images[0].RevisedPrompt)
	assert.Equal(t, "1664*928", resp.Size)

	// The OpenAI-style "1664x928" spelling is normalized to DashScope's star form.
	input, ok := submit["input"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a cat", input["prompt"])

	parameters, ok := submit["parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1664*928", parameters["size"])
}

func TestQwenImageWan26MessagesShape(t *testing.T) {
	t.Parallel()

	var submit map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/services/aigc/image-generation/generation":
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.NoError(t, json.Unmarshal(body, &submit))

			_, _ = w.Write([]byte(`{"output":{"task_id":"task-2","task_status":"PENDING"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/tasks/task-2":
			_, _ = w.Write([]byte(`{
				"output": {
					"task_status": "SUCCEEDED",
					"choices": [{"message": {"role": "assistant", "content": [
						{"image": "https://example.com/a.png", "type": "image"},
						{"image": "https://example.com/b.png", "type": "image"}
					]}}]
				},
				"usage": {"image_count": 2}
			}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	model := qwen.NewImageModel("wan2.6-t2i",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
		qwen.WithPollInterval(time.Millisecond),
	)

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "two cats", N: 2})
	require.NoError(t, err)
	require.Len(t, resp.Images, 2)
	assert.Equal(t, "https://example.com/a.png", resp.Images[0].URL)
	assert.Equal(t, "https://example.com/b.png", resp.Images[1].URL)

	// wan2.6 submits through the messages shape, not the prompt shape.
	input, ok := submit["input"].(map[string]any)
	require.True(t, ok)
	assert.Nil(t, input["prompt"])
	messages, ok := input["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
}

func TestQwenImageTaskFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"output":{"task_id":"task-3","task_status":"PENDING"}}`))

			return
		}

		_, _ = w.Write([]byte(`{"output":{"task_status":"FAILED","code":"DataInspectionFailed","message":"content blocked"}}`))
	}))
	t.Cleanup(server.Close)

	model := qwen.NewImageModel("qwen-image-plus",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
		qwen.WithPollInterval(time.Millisecond),
	)

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "blocked"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "content blocked")
}

func TestQwenImageRejectsEmptyPrompt(t *testing.T) {
	t.Parallel()

	model := qwen.NewImageModel("qwen-image-plus", qwen.WithAPIKey("test"))

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "  "})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
}
