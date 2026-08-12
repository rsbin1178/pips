package openai_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type weatherOut struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_c"`
}

// TestGenerateTypedThroughChat covers AC6 for OpenAI: the derived schema goes
// out as response_format and the JSON text decodes into the struct.
func TestGenerateTypedThroughChat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, jsonDecode(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"{\"city\":\"Paris\",\"temp_c\":21.5}"},"finish_reason":"stop"}]}`))
	})

	got, resp, err := ai.GenerateTyped[weatherOut](t.Context(), model, ai.Request{
		Messages: []ai.Message{ai.UserText("weather in paris")},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, weatherOut{City: "Paris", TempC: 21.5}, got)

	// The derived schema was sent as json_schema response_format.
	rf := as[map[string]any](t, captured["response_format"])
	assert.Equal(t, "json_schema", rf["type"])
	js := as[map[string]any](t, rf["json_schema"])
	schema := as[map[string]any](t, js["schema"])
	props := as[map[string]any](t, schema["properties"])
	assert.Contains(t, props, "temp_c")
}
