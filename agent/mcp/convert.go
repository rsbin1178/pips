package agentmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/ai"
)

func convertResult(result *mcp.CallToolResult) ([]ai.Part, error) {
	parts := make([]ai.Part, 0, len(result.Content))

	for index, content := range result.Content {
		part, err := convertContent(content)
		if err != nil {
			return nil, fmt.Errorf("content %d: %w", index, err)
		}

		parts = append(parts, part)
	}

	if len(parts) == 0 && result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("structured content: %w", err)
		}

		if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, errors.New("structured content must be a JSON object")
		}

		parts = append(parts, ai.Text(string(data)))
	}

	return parts, nil
}

func convertContent(content mcp.Content) (ai.Part, error) {
	switch content := content.(type) {
	case *mcp.TextContent:
		if content == nil {
			return nil, errors.New("nil text content")
		}

		return ai.Text(content.Text), nil
	case *mcp.ImageContent:
		if content == nil {
			return nil, errors.New("nil image content")
		}

		return ai.ImagePart{Source: ai.MediaSource{
			Data:     bytes.Clone(content.Data),
			MIMEType: content.MIMEType,
		}}, nil
	case *mcp.AudioContent:
		if content == nil {
			return nil, errors.New("nil audio content")
		}

		return ai.FilePart{Source: ai.MediaSource{
			Data:     bytes.Clone(content.Data),
			MIMEType: content.MIMEType,
		}}, nil
	case *mcp.EmbeddedResource:
		return convertEmbeddedResource(content)
	case *mcp.ResourceLink:
		return contentJSON(content)
	default:
		return contentJSON(content)
	}
}

func convertEmbeddedResource(content *mcp.EmbeddedResource) (ai.Part, error) {
	if content == nil || content.Resource == nil {
		return nil, errors.New("nil embedded resource")
	}

	resource := content.Resource
	if resource.Blob == nil {
		return ai.Text(resource.Text), nil
	}

	source := ai.MediaSource{
		Data:     bytes.Clone(resource.Blob),
		MIMEType: resource.MIMEType,
	}
	if strings.HasPrefix(strings.ToLower(resource.MIMEType), "image/") {
		return ai.ImagePart{Source: source}, nil
	}

	return ai.FilePart{Source: source, Name: resource.URI}, nil
}

func contentJSON(content mcp.Content) (ai.Part, error) {
	if content == nil {
		return nil, errors.New("nil content")
	}

	data, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal %T: %w", content, err)
	}

	return ai.Text(string(data)), nil
}
