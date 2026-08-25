package ai

import (
	"encoding/json"
	"strings"
)

// ProviderParts rewrites tool-result content into the part kinds every
// provider transport carries natively: text, image, and file. Structured
// content becomes its JSON object text so the model still receives the
// payload; resource links become a deterministic JSON description and are
// never fetched; embedded resources project their text, or their blob as an
// image or file. The public Part taxonomy stays lossless; this view exists
// for provider I/O and consumers that have not learned the newer kinds.
func ProviderParts(parts []Part) []Part {
	if parts == nil {
		return nil
	}

	out := make([]Part, 0, len(parts))
	for _, part := range parts {
		switch part := part.(type) {
		case StructuredContentPart:
			out = append(out, Text(string(part.Data)))
		case ResourceLinkPart:
			out = append(out, Text(resourceLinkText(part)))
		case EmbeddedResourcePart:
			out = append(out, embeddedResourceParts(part)...)
		default:
			out = append(out, part)
		}
	}

	return out
}

func embeddedResourceParts(part EmbeddedResourcePart) []Part {
	if part.Blob == nil {
		return []Part{Text(part.Text)}
	}

	source := MediaSource{Data: part.Blob, MIMEType: part.MIMEType}
	if strings.HasPrefix(strings.ToLower(part.MIMEType), "image/") {
		return []Part{ImagePart{Source: source}}
	}

	return []Part{FilePart{Name: part.URI, Source: source}}
}

func resourceLinkText(link ResourceLinkPart) string {
	data, err := json.Marshal(struct {
		Kind        string `json:"kind"`
		URI         string `json:"uri"`
		Name        string `json:"name,omitempty"`
		Title       string `json:"title,omitempty"`
		Description string `json:"description,omitempty"`
		MIMEType    string `json:"mime_type,omitempty"`
	}{
		Kind:        "resource_link",
		URI:         link.URI,
		Name:        link.Name,
		Title:       link.Title,
		Description: link.Description,
		MIMEType:    link.MIMEType,
	})
	if err != nil {
		// Marshaling plain-string fields cannot fail; fall back to the URI
		// anyway rather than returning an empty description.
		return link.URI
	}

	return string(data)
}
