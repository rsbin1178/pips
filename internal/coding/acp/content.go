package acp

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/url"
	"strconv"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/ai"
)

func convertPrompt(blocks []acpsdk.ContentBlock, limits Limits) (ai.Message, error) {
	if len(blocks) == 0 || len(blocks) > limits.MaxPromptBlocks {
		return ai.Message{}, fmt.Errorf("%w: prompt block count", ErrInvalid)
	}

	parts := make([]ai.Part, 0, len(blocks))
	textBytes := 0
	binaryBytes := 0

	for index, block := range blocks {
		part, textSize, binarySize, err := convertPromptBlock(
			block,
			limits.MaxPromptBytes,
			limits.MaxBinaryBytes,
		)
		if err != nil {
			return ai.Message{}, fmt.Errorf("%w: prompt block %d", err, index)
		}

		textBytes += textSize

		binaryBytes += binarySize
		if textBytes > limits.MaxPromptBytes || binaryBytes > limits.MaxBinaryBytes {
			return ai.Message{}, fmt.Errorf("%w: prompt byte limit", ErrInvalid)
		}

		parts = append(parts, part)
	}

	return ai.User(parts...), nil
}

func convertPromptBlock(
	block acpsdk.ContentBlock,
	maxText int,
	maxBinary int,
) (ai.Part, int, int, error) {
	if err := block.Validate(); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: invalid content union", ErrInvalid)
	}

	switch {
	case block.Text != nil:
		if !validProtocolText(block.Text.Text, maxText, true) {
			return nil, 0, 0, fmt.Errorf("%w: invalid text", ErrInvalid)
		}

		return ai.Text(block.Text.Text), len(block.Text.Text), 0, nil
	case block.ResourceLink != nil:
		text, err := renderResourceLink(*block.ResourceLink)
		return ai.Text(text), len(text), 0, err
	case block.Image != nil:
		part, size, err := convertImage(*block.Image, maxBinary)
		if block.Image.Uri != nil && *block.Image.Uri != "" {
			return part, size, 0, err
		}

		return part, 0, size, err
	case block.Resource != nil:
		return convertEmbeddedResource(block.Resource.Resource, maxText, maxBinary)
	case block.Audio != nil:
		return nil, 0, 0, fmt.Errorf("%w: audio prompts are unsupported", ErrInvalid)
	default:
		return nil, 0, 0, fmt.Errorf("%w: unknown content block", ErrInvalid)
	}
}

func renderResourceLink(link acpsdk.ContentBlockResourceLink) (string, error) {
	if !validProtocolText(link.Name, 4<<10, false) || !validURI(link.Uri) {
		return "", fmt.Errorf("%w: invalid resource link", ErrInvalid)
	}

	var rendered strings.Builder
	rendered.WriteString("Resource: ")
	rendered.WriteString(link.Name)
	rendered.WriteString("\nURI: ")
	rendered.WriteString(link.Uri)

	if link.Title != nil && *link.Title != "" {
		rendered.WriteString("\nTitle: ")
		rendered.WriteString(*link.Title)
	}

	if link.Description != nil && *link.Description != "" {
		rendered.WriteString("\nDescription: ")
		rendered.WriteString(*link.Description)
	}

	if link.MimeType != nil && *link.MimeType != "" {
		rendered.WriteString("\nMIME type: ")
		rendered.WriteString(*link.MimeType)
	}

	if link.Size != nil {
		rendered.WriteString("\nSize: ")
		rendered.WriteString(strconv.Itoa(*link.Size))
		rendered.WriteString(" bytes")
	}

	if !validProtocolText(rendered.String(), 32<<10, false) {
		return "", fmt.Errorf("%w: resource link too large", ErrInvalid)
	}

	return rendered.String(), nil
}

func convertImage(image acpsdk.ContentBlockImage, maximum int) (ai.Part, int, error) {
	if !validMIMEType(image.MimeType, "image") {
		return nil, 0, fmt.Errorf("%w: invalid image MIME type", ErrInvalid)
	}

	if image.Uri != nil && *image.Uri != "" {
		if image.Data != "" || !validURI(*image.Uri) {
			return nil, 0, fmt.Errorf("%w: ambiguous image source", ErrInvalid)
		}

		return ai.ImageURL(*image.Uri), len(*image.Uri), nil
	}

	data, err := decodeBase64(image.Data, maximum)
	if err != nil {
		return nil, 0, err
	}

	return ai.ImageData(image.MimeType, data), len(data), nil
}

func convertEmbeddedResource(
	resource acpsdk.EmbeddedResourceResource,
	maxText int,
	maxBinary int,
) (ai.Part, int, int, error) {
	switch {
	case resource.TextResourceContents != nil && resource.BlobResourceContents == nil:
		return convertEmbeddedText(*resource.TextResourceContents, maxText)
	case resource.BlobResourceContents != nil && resource.TextResourceContents == nil:
		return convertEmbeddedBlob(*resource.BlobResourceContents, maxBinary)
	default:
		return nil, 0, 0, fmt.Errorf("%w: ambiguous embedded resource", ErrInvalid)
	}
}

func convertEmbeddedText(
	value acpsdk.TextResourceContents,
	maximum int,
) (ai.Part, int, int, error) {
	if !validURI(value.Uri) || !validProtocolText(value.Text, maximum, true) {
		return nil, 0, 0, fmt.Errorf("%w: invalid text resource", ErrInvalid)
	}

	mimeType := "text/plain"
	if value.MimeType != nil && *value.MimeType != "" {
		mimeType = *value.MimeType
	}

	if !validMIMEType(mimeType, "") {
		return nil, 0, 0, fmt.Errorf("%w: invalid text resource MIME type", ErrInvalid)
	}

	return ai.FileData(resourceName(value.Uri), mimeType, []byte(value.Text)), len(value.Text), 0, nil
}

func convertEmbeddedBlob(
	value acpsdk.BlobResourceContents,
	maximum int,
) (ai.Part, int, int, error) {
	if !validURI(value.Uri) || value.MimeType == nil || !validMIMEType(*value.MimeType, "") {
		return nil, 0, 0, fmt.Errorf("%w: invalid blob resource", ErrInvalid)
	}

	data, err := decodeBase64(value.Blob, maximum)
	if err != nil {
		return nil, 0, 0, err
	}

	return ai.FileData(resourceName(value.Uri), *value.MimeType, data), 0, len(data), nil
}

func decodeBase64(value string, maximum int) ([]byte, error) {
	if value == "" || base64.StdEncoding.DecodedLen(len(value)) > maximum {
		return nil, fmt.Errorf("%w: binary byte limit", ErrInvalid)
	}

	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(data) > maximum {
		return nil, fmt.Errorf("%w: invalid base64 data", ErrInvalid)
	}

	return data, nil
}

func validURI(value string) bool {
	if !validProtocolText(value, 32<<10, false) {
		return false
	}

	parsed, err := url.Parse(value)

	return err == nil && parsed.Scheme != ""
}

func validMIMEType(value, requiredTopLevel string) bool {
	if !validProtocolText(value, 256, false) || strings.TrimSpace(value) != value {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}

	if requiredTopLevel == "" {
		return true
	}

	slash := strings.IndexByte(mediaType, '/')

	return slash > 0 && strings.EqualFold(mediaType[:slash], requiredTopLevel)
}

func resourceName(uri string) string {
	parsed, err := url.Parse(uri)
	if err == nil {
		path := strings.TrimRight(parsed.Path, "/")
		if index := strings.LastIndexByte(path, '/'); index >= 0 && index+1 < len(path) {
			return path[index+1:]
		}
	}

	return "resource"
}
