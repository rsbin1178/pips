package openai_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveSSE streams a .sse fixture file.
func serveSSE(t *testing.T, name string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixture path built from test constants
		assert.NoError(t, err)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(data)
	}
}

func TestChatStreamText(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, "chat_stream_text.sse"))

	var events []ai.StreamEvent

	for ev, err := range model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	}) {
		require.NoError(t, err)

		events = append(events, ev)
	}

	require.NotEmpty(t, events)
	assert.Equal(t, ai.StreamMessageStart, events[0].Type)
	assert.Equal(t, "chatcmpl-s1", events[0].ID)
	assert.Equal(t, "gpt-4o-2024-11-20", events[0].Model)

	last := events[len(events)-1]
	assert.Equal(t, ai.StreamMessageEnd, last.Type)
	assert.Equal(t, ai.FinishStop, last.FinishReason)
	require.NotNil(t, last.Usage)
	assert.Equal(t, 9, last.Usage.InputTokens)
	assert.Equal(t, 3, last.Usage.OutputTokens)

	// Collect assembles the same stream into a full response.
	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	}))
	require.NoError(t, err)
	assert.Equal(t, "Hello!", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
}

func TestChatStreamToolCalls(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, "chat_stream_tools.sse"))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather and time?")},
	}))
	require.NoError(t, err)

	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "call_a", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris"}`, string(calls[0].Args))
	assert.Equal(t, "call_b", calls[1].ID)
	assert.JSONEq(t, `{}`, string(calls[1].Args))
}

func TestChatStreamRequestsUsage(t *testing.T) {
	t.Parallel()

	var sawStreamOptions bool

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, jsonDecode(r, &body))

		_, sawStreamOptions = body["stream_options"]
		assert.Equal(t, true, body["stream"])

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	for _, err := range model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("x")}}) {
		require.NoError(t, err)
	}

	assert.True(t, sawStreamOptions, "stream_options.include_usage should be requested")
}

func TestChatStreamHTTPErrorSurfacesOnFirstIteration(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
	})

	var events, errs int

	for _, err := range model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("x")}}) {
		if err != nil {
			errs++

			require.ErrorIs(t, err, ai.ErrOverloaded)

			break
		}

		events++
	}

	assert.Zero(t, events)
	assert.Equal(t, 1, errs)
}

// TestChatStreamEarlyBreakCancelsRequest verifies AC4: breaking out of the
// range loop tears down the HTTP request (the server observes cancellation).
func TestChatStreamEarlyBreakCancelsRequest(t *testing.T) {
	t.Parallel()

	requestGone := make(chan struct{})
	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := as[http.Flusher](t, w)

		// First chunk, then hold the connection open until the client bails.
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-x","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"first"}}]}` + "\n\n"))

		flusher.Flush()

		<-r.Context().Done()
		close(requestGone)
	})

	sawText := false

	for ev, err := range model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("x")}}) {
		require.NoError(t, err)

		if ev.Type == ai.StreamTextDelta {
			sawText = true
			break // early exit must cancel the underlying request
		}
	}

	require.True(t, sawText)

	select {
	case <-requestGone:
		// Server saw the disconnect: the stream was torn down.
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed cancellation after early break")
	}
}

func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close() //nolint:errcheck // test helper

	return json.NewDecoder(r.Body).Decode(v)
}
