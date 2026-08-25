package ai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolResultMCPKindJSONRoundTrip(t *testing.T) {
	t.Parallel()

	message := ToolMessage{Parts: []ToolResultPart{{
		ToolCallID: "call-1",
		Name:       "search",
		Content: []Part{
			Text("summary"),
			StructuredContentPart{Data: JSON(`{"status":"ok","count":2}`)},
			ResourceLinkPart{
				URI:         "file:///reports/q3.pdf",
				Name:        "q3.pdf",
				Title:       "Q3 Report",
				Description: "Quarterly report",
				MIMEType:    "application/pdf",
			},
			EmbeddedResourcePart{URI: "file:///notes/todo.md", MIMEType: "text/markdown", Text: "# Todo"},
			EmbeddedResourcePart{URI: "test://blob", MIMEType: "application/pdf", Blob: []byte("pdf")},
		},
	}}}

	encoded, err := json.Marshal(message)
	require.NoError(t, err)

	var decoded ToolMessage
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, message, decoded)
}

func TestStructuredContentConstructorValidatesObject(t *testing.T) {
	t.Parallel()

	part, err := StructuredContent(JSON(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, string(part.Data))

	for _, invalid := range []JSON{nil, JSON(``), JSON(`  `), JSON(`[1,2]`), JSON(`{"a":}`)} {
		_, err := StructuredContent(invalid)
		require.ErrorContains(t, err, "structured content must be")
	}
}

func TestValidateRejectsInvalidMCPKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []Part
		wantErr string
	}{
		{
			name:    "resource link without uri",
			content: []Part{ResourceLinkPart{Name: "orphan"}},
			wantErr: "resource link: uri is required",
		},
		{
			name:    "embedded resource without uri",
			content: []Part{EmbeddedResourcePart{Text: "body"}},
			wantErr: "embedded resource: uri is required",
		},
		{
			name:    "embedded blob without mime type",
			content: []Part{EmbeddedResourcePart{URI: "test://blob", Blob: []byte("pdf")}},
			wantErr: "mime type is required for blob resources",
		},
		{
			name:    "structured content that is not an object",
			content: []Part{StructuredContentPart{Data: JSON("[1]")}},
			wantErr: "structured content must be a JSON object",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ToolMessage{Parts: []ToolResultPart{{ToolCallID: "call-1", Content: test.content}}}.Validate()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestMCPKindsRejectedOutsideToolResults(t *testing.T) {
	t.Parallel()

	// A user message carrying a structured_content part must not decode.
	var user UserMessage

	invalidUser := `{"role":"user","parts":[{"type":"structured_content","data":{"a":1}}]}`

	userErr := json.Unmarshal([]byte(invalidUser), &user)
	require.ErrorContains(t, userErr, "unsupported concrete type")

	// The same holds for assistant messages.
	var assistant AssistantMessage

	invalidAssistant := `{"role":"assistant","parts":[{"type":"resource_link","uri":"file:///x"}]}`

	assistantErr := json.Unmarshal([]byte(invalidAssistant), &assistant)
	require.ErrorContains(t, assistantErr, "unsupported concrete type")
}

func TestProviderPartsLeavesLegacyKindsUntouched(t *testing.T) {
	t.Parallel()

	parts := []Part{
		Text("plain"),
		ImagePart{Source: MediaSource{URL: "https://example.invalid/a.png"}},
		FilePart{Name: "doc.pdf"},
		ToolCallPart{ID: "call-1", Name: "search"},
		ToolResultPart{ToolCallID: "call-1"},
	}

	assert.Equal(t, parts, ProviderParts(parts))
	assert.Nil(t, ProviderParts(nil))
}

func TestCloneMessageCopiesMCPKindMutableFields(t *testing.T) {
	t.Parallel()

	source := ToolMessage{Parts: []ToolResultPart{{
		ToolCallID: "call-1",
		Content: []Part{
			StructuredContentPart{Data: JSON(`{"a":1}`)},
			EmbeddedResourcePart{URI: "test://blob", MIMEType: "application/pdf", Blob: []byte("pdf")},
		},
	}}}

	clonedMessage, err := CloneMessage(source)
	require.NoError(t, err)

	cloned, ok := clonedMessage.(ToolMessage)
	require.True(t, ok)

	structuredClone, okStructured := cloned.Parts[0].Content[0].(StructuredContentPart)
	require.True(t, okStructured)

	structuredClone.Data[0] = '{'

	sourceStructured, sourceOK := source.Parts[0].Content[0].(StructuredContentPart)
	require.True(t, sourceOK)
	assert.Equal(t, byte('{'), sourceStructured.Data[0])

	blobClone, okBlob := cloned.Parts[0].Content[1].(EmbeddedResourcePart)
	require.True(t, okBlob)

	blobClone.Blob[0] = 'x'

	sourceBlob, sourceBlobOK := source.Parts[0].Content[1].(EmbeddedResourcePart)
	require.True(t, sourceBlobOK)
	assert.Equal(t, byte('p'), sourceBlob.Blob[0])
}
