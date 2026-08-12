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
	assert.Equal(t, []ai.Part{ai.Text(`{"status":"ok"}`)}, parts)

	parts, err = convertResult(&mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "preferred"}},
		StructuredContent: map[string]any{"status": "duplicate"},
	})
	require.NoError(t, err)
	assert.Equal(t, []ai.Part{ai.Text("preferred")}, parts)
}

func TestConvertEmbeddedImageResource(t *testing.T) {
	t.Parallel()

	part, err := convertEmbeddedResource(&mcp.EmbeddedResource{
		Resource: &mcp.ResourceContents{
			URI:      "test://image",
			MIMEType: "IMAGE/PNG",
			Blob:     []byte("image"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ImagePart{Source: ai.MediaSource{
		Data:     []byte("image"),
		MIMEType: "IMAGE/PNG",
	}}, part)
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
