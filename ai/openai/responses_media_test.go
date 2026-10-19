package openai_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolResultMessages builds the assistant tool call plus one tool result so the
// adapter has to render a function_call_output item.
func toolResultMessages(content ...ai.Part) ai.Messages {
	return ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "call_w1", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "call_w1",
			Name:       "read",
			Content:    content,
		}),
	}
}

// functionCallOutput returns the function_call_output item from the captured
// request body.
func functionCallOutput(t *testing.T, captured map[string]any) map[string]any {
	t.Helper()

	input := as[[]any](t, captured["input"])
	require.Len(t, input, 3)

	output := as[map[string]any](t, input[2])
	require.Equal(t, "function_call_output", output["type"])

	return output
}

// TestResponsesToolResultMediaWireFormat covers the tool-result media matrix:
// text-only keeps the string shape, and image, file, and mixed results become
// the input content-part array.
func TestResponsesToolResultMediaWireFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []ai.Part
		want    any
	}{
		{
			name:    "text only keeps the string shape",
			content: []ai.Part{ai.Text(`{"temp":21}`)},
			want:    `{"temp":21}`,
		},
		{
			name:    "image becomes an input_image part",
			content: []ai.Part{ai.ImageData("image/png", []byte{1, 2, 3})},
			want: []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AQID"},
			},
		},
		{
			name:    "file becomes an input_file part",
			content: []ai.Part{ai.FileData("inline.pdf", "application/pdf", []byte("%PDF"))},
			want: []any{
				map[string]any{"type": "input_file", "filename": "inline.pdf", "file_data": "data:application/pdf;base64,JVBERg=="},
			},
		},
		{
			name: "mixed keeps text and image together",
			content: []ai.Part{
				ai.Text("here is the screen"),
				ai.ImageData("image/png", []byte{1, 2, 3}),
			},
			want: []any{
				map[string]any{"type": "input_text", "text": "here is the screen"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AQID"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any

			model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

			_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(tt.content...)})
			require.NoError(t, err)

			assert.Equal(t, tt.want, functionCallOutput(t, captured)["output"])
		})
	}
}

// TestResponsesToolResultTextOnlyWireBytes pins the exact bytes of a text-only
// function_call_output item. Output became an `any` so it could also carry a
// content-part array; the text path must stay byte-identical to the string
// field it replaced, including field order and the absent empty value.
func TestResponsesToolResultTextOnlyWireBytes(t *testing.T) {
	t.Parallel()

	var (
		body    []byte
		readErr error
	)

	model := newResponsesModel(t, func(w http.ResponseWriter, r *http.Request) {
		body, readErr = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesTextResponse))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(ai.Text(`{"temp":21}`))})
	require.NoError(t, err)
	require.NoError(t, readErr)

	assert.Contains(t, string(body),
		`{"type":"function_call_output","call_id":"call_w1","output":"{\"temp\":21}"}`)
}

// TestResponsesToolResultTextOnlyOmitsEmptyOutput pins the byte-level shape of
// an empty text result: the field stays absent exactly as the string field's
// omitempty made it before media parts were expressible.
func TestResponsesToolResultTextOnlyOmitsEmptyOutput(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(ai.Text(""))})
	require.NoError(t, err)

	assert.NotContains(t, functionCallOutput(t, captured), "output")
}

// TestResponsesToolResultUnsupportedPartIsTypedError covers the unsupported
// variant: a tool-result part the Responses content array cannot express is
// refused with a typed error naming the provider and the part kind instead of
// being silently dropped. The refusal happens before any provider call.
func TestResponsesToolResultUnsupportedPartIsTypedError(t *testing.T) {
	t.Parallel()

	model := newResponsesModel(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the adapter must refuse unsupported tool media before any provider call")

		w.WriteHeader(http.StatusBadRequest)
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(
		ai.ReasoningPart{Text: "hidden chain"},
	)})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrUnsupportedMedia)
	require.ErrorIs(t, err, ai.ErrUnsupported)

	var mediaErr *ai.UnsupportedMediaError
	require.ErrorAs(t, err, &mediaErr)
	assert.Equal(t, ai.ProviderOpenAI, mediaErr.Provider)
	assert.Equal(t, "reasoning", mediaErr.Kind)
	assert.Contains(t, err.Error(), "reasoning")
}
