package agentmcp

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertResultUsesStructuredFallback(t *testing.T) {
	t.Parallel()

	parts, err := convertResult(&mcp.CallToolResult{
		StructuredContent: map[string]any{"status": "ok"},
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.StructuredContentPart{Data: json.RawMessage(`{"status":"ok"}`)}}, parts)

	parts, err = convertResult(&mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "preferred"}},
		StructuredContent: map[string]any{"status": "duplicate"},
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("preferred")}, parts)
}

func TestConvertResourceLinkKeepsIdentity(t *testing.T) {
	t.Parallel()

	parts, err := convertResult(&mcp.CallToolResult{
		Content: []mcp.Content{&mcp.ResourceLink{
			URI:         "file:///reports/q3.pdf",
			Name:        "q3.pdf",
			Title:       "Q3 Report",
			Description: "Quarterly report",
			MIMEType:    "application/pdf",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.ResourceLinkPart{
		URI:         "file:///reports/q3.pdf",
		Name:        "q3.pdf",
		Title:       "Q3 Report",
		Description: "Quarterly report",
		MIMEType:    "application/pdf",
	}}, parts)
}

func TestConvertEmbeddedTextResourceKeepsURI(t *testing.T) {
	t.Parallel()

	part, err := convertEmbeddedResource(&mcp.EmbeddedResource{
		Resource: &mcp.ResourceContents{
			URI:      "file:///notes/todo.md",
			MIMEType: "text/markdown",
			Text:     "# Todo",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.EmbeddedResourcePart{
		URI:      "file:///notes/todo.md",
		MIMEType: "text/markdown",
		Text:     "# Todo",
	}, part)
}

func TestConvertEmbeddedBlobResourceKeepsURI(t *testing.T) {
	t.Parallel()

	part, err := convertEmbeddedResource(&mcp.EmbeddedResource{
		Resource: &mcp.ResourceContents{
			URI:      "test://image",
			MIMEType: "IMAGE/PNG",
			Blob:     []byte("image"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.EmbeddedResourcePart{
		URI:      "test://image",
		MIMEType: "IMAGE/PNG",
		Blob:     []byte("image"),
	}, part)
}

func TestProviderPartsProjectsConvertedResults(t *testing.T) {
	t.Parallel()

	structured, err := ai.StructuredContent(json.RawMessage(`{"status":"ok"}`))
	require.NoError(t, err)

	projected := ai.ProviderParts([]ai.Part{
		structured,
		ai.ResourceLinkPart{URI: "file:///reports/q3.pdf", Name: "q3.pdf"},
		ai.EmbeddedResourcePart{URI: "file:///notes/todo.md", MIMEType: "text/markdown", Text: "# Todo"},
		ai.EmbeddedResourcePart{URI: "test://image", MIMEType: "image/png", Blob: []byte("image")},
		ai.EmbeddedResourcePart{URI: "test://audio", MIMEType: "audio/wav", Blob: []byte("audio")},
	})

	require.Len(t, projected, 5)
	assert.Equal(t, ai.Text(`{"status":"ok"}`), projected[0])

	linkText, ok := projected[1].(ai.TextPart)
	require.True(t, ok)
	assert.JSONEq(t, `{"kind":"resource_link","uri":"file:///reports/q3.pdf","name":"q3.pdf"}`, linkText.Text)

	assert.Equal(t, ai.Text("# Todo"), projected[2])
	assert.Equal(t, ai.ImagePart{Source: ai.MediaSource{Data: []byte("image"), MIMEType: "image/png"}}, projected[3])
	assert.Equal(t, ai.FilePart{Name: "test://audio", Source: ai.MediaSource{Data: []byte("audio"), MIMEType: "audio/wav"}}, projected[4])
}

func TestConvertResultRejectsInvalidContent(t *testing.T) {
	t.Parallel()

	var nilText *mcp.TextContent

	_, err := convertResult(&mcp.CallToolResult{Content: []mcp.Content{nilText}})
	require.ErrorContains(t, err, "nil text content")

	var nilResource *mcp.EmbeddedResource

	_, err = convertResult(&mcp.CallToolResult{Content: []mcp.Content{nilResource}})
	require.ErrorContains(t, err, "nil embedded resource")

	_, err = convertEmbeddedResource(&mcp.EmbeddedResource{})
	require.ErrorContains(t, err, "nil embedded resource")

	var nilLink *mcp.ResourceLink

	_, err = convertResult(&mcp.CallToolResult{Content: []mcp.Content{nilLink}})
	require.ErrorContains(t, err, "nil resource link")
}

func TestConvertResultReportsUnmarshalableStructuredContent(t *testing.T) {
	t.Parallel()

	_, err := convertResult(&mcp.CallToolResult{
		StructuredContent: map[string]any{"bad": func() {}},
	})

	var typeErr *json.UnsupportedTypeError
	require.ErrorAs(t, err, &typeErr)

	_, err = convertResult(&mcp.CallToolResult{StructuredContent: []string{"not", "an", "object"}})
	require.ErrorContains(t, err, "structured content must be a JSON object")
}
