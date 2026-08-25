package agentmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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

		part, err := ai.StructuredContent(data)
		if err != nil {
			return nil, fmt.Errorf("structured content: %w", err)
		}

		parts = append(parts, part)
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
		if content == nil {
			return nil, errors.New("nil resource link")
		}

		return ai.ResourceLinkPart{
			URI:         content.URI,
			Name:        content.Name,
			Title:       content.Title,
			Description: content.Description,
			MIMEType:    content.MIMEType,
		}, nil
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
		return ai.EmbeddedResourcePart{
			URI:      resource.URI,
			MIMEType: resource.MIMEType,
			Text:     resource.Text,
		}, nil
	}

	return ai.EmbeddedResourcePart{
		URI:      resource.URI,
		MIMEType: resource.MIMEType,
		Blob:     bytes.Clone(resource.Blob),
	}, nil
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
