package gemini_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolResultMessages builds the assistant tool call plus one tool result so the
// adapter has to render a functionResponse part.
func toolResultMessages(content ...ai.Part) ai.Messages {
	return ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "call_0", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "call_0",
			Name:       "read",
			Content:    content,
		}),
	}
}

// functionResponse returns the functionResponse object from the captured
// request body.
func functionResponse(t *testing.T, captured map[string]any) map[string]any {
	t.Helper()

	contents := as[[]any](t, captured["contents"])
	require.Len(t, contents, 3)

	toolTurn := as[map[string]any](t, contents[2])
	require.Equal(t, "user", toolTurn["role"])

	return as[map[string]any](t, as[map[string]any](t, as[[]any](t, toolTurn["parts"])[0])["functionResponse"])
}

// TestGeminiToolResultMediaWireFormat covers the tool-result media matrix:
// text-only keeps the object-only shape, and image, file, and mixed results add
// the documented inlineData/fileData function-response parts.
func TestGeminiToolResultMediaWireFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   []ai.Part
		wantParts []any
	}{
		{
			name:    "text only omits media parts",
			content: []ai.Part{ai.Text(`{"temp":21}`)},
		},
		{
			name:    "image becomes an inlineData part",
			content: []ai.Part{ai.ImageData("image/png", []byte{1, 2, 3})},
			wantParts: []any{
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "AQID"}},
			},
		},
		{
			name:    "inline file becomes an inlineData part",
			content: []ai.Part{ai.FileData("doc.pdf", "application/pdf", []byte("%PDF"))},
			wantParts: []any{
				map[string]any{"inlineData": map[string]any{"mimeType": "application/pdf", "data": "JVBERg=="}},
			},
		},
		{
			name:    "remote file becomes a fileData part",
			content: []ai.Part{ai.FileURL("doc.pdf", "application/pdf", "https://example.com/doc.pdf")},
			wantParts: []any{
				map[string]any{"fileData": map[string]any{"mimeType": "application/pdf", "fileUri": "https://example.com/doc.pdf"}},
			},
		},
		{
			name: "mixed keeps the text response and the image part",
			content: []ai.Part{
				ai.Text("here is the screenshot"),
				ai.ImageData("image/jpeg", []byte{0xff, 0xd8}),
			},
			wantParts: []any{
				map[string]any{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": "/9g="}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any

			model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

			_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(tt.content...)})
			require.NoError(t, err)

			fr := functionResponse(t, captured)

			if tt.wantParts == nil {
				assert.NotContains(t, fr, "parts")
			} else {
				assert.Equal(t, tt.wantParts, as[[]any](t, fr["parts"]))
			}
		})
	}
}

// TestGeminiToolResultUnsupportedMediaIsTypedError covers the unsupported
// variant: a provider file ID has no function-response form, so it is refused
// with a typed error naming the provider and the part kind. The refusal happens
// before any provider call.
func TestGeminiToolResultUnsupportedMediaIsTypedError(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the adapter must refuse unsupported tool media before any provider call")

		w.WriteHeader(http.StatusBadRequest)
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: toolResultMessages(
		ai.FileID("doc.pdf", "application/pdf", "files/abc"),
	)})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrUnsupportedMedia)
	require.ErrorIs(t, err, ai.ErrUnsupported)

	var mediaErr *ai.UnsupportedMediaError
	require.ErrorAs(t, err, &mediaErr)
	assert.Equal(t, ai.ProviderGemini, mediaErr.Provider)
	assert.Equal(t, "file", mediaErr.Kind)
}
