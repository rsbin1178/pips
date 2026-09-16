// Package imagewire maps the Images-shaped response items shared by the image
// adapters onto portable images and derives media types from wire format
// names. Adapters keep their own request and response structs; the data items
// of a response and the MIME inference rules live here once.
package imagewire

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// Image media types.
const (
	mediaPNG  = "image/png"
	mediaJPEG = "image/jpeg"
	mediaWebP = "image/webp"
)

// Datum is one response data item of an Images-shaped response body.
type Datum struct {
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt"`
}

// Image maps one response data item onto a portable image. mimeType is the
// media type inferred from response metadata or the request; empty means
// unknown. index names the item in errors. An item with neither payload field
// is an error: a zero-value image must never look like a successful result.
func Image(datum Datum, mimeType string, index int) (ai.GeneratedImage, error) {
	switch {
	case datum.B64JSON != "":
		data, err := base64.StdEncoding.DecodeString(datum.B64JSON)
		if err != nil {
			return ai.GeneratedImage{}, fmt.Errorf("decoding image %d: %w", index, err)
		}

		if mimeType == "" {
			mimeType = mediaPNG
		}

		return ai.GeneratedImage{Data: data, MIMEType: mimeType, RevisedPrompt: datum.RevisedPrompt}, nil
	case datum.URL != "":
		if mimeType == "" {
			mimeType = MIMEFromURL(datum.URL)
		}

		return ai.GeneratedImage{URL: datum.URL, MIMEType: mimeType, RevisedPrompt: datum.RevisedPrompt}, nil
	default:
		return ai.GeneratedImage{}, fmt.Errorf("data item %d carries neither b64_json nor url", index)
	}
}

// MIME maps a wire format name to a media type. It returns "" for an unknown
// or empty format rather than guessing.
func MIME(format string) string {
	switch strings.ToLower(format) {
	case "png":
		return mediaPNG
	case "jpeg", "jpg":
		return mediaJPEG
	case "webp":
		return mediaWebP
	default:
		return ""
	}
}

// MIMEFor picks the media type of produced image bytes: the format the
// provider reports first, then the format the caller requested.
func MIMEFor(reported, requested string) string {
	if mimeType := MIME(reported); mimeType != "" {
		return mimeType
	}

	return MIME(requested)
}

// MIMEFromURL infers a media type from a URL's file extension. It returns ""
// when the extension is unknown.
func MIMEFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	return MIME(strings.TrimPrefix(strings.ToLower(path.Ext(parsed.Path)), "."))
}
