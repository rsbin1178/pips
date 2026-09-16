package agnes

import "github.com/rsbin1178/pips/ai/internal/imagewire"

// imagesPath is the only Agnes image endpoint. Text-to-image and image-to-image
// requests both POST here; there is no separate edits endpoint.
const imagesPath = "images/generations"

// imageRequest is the wire body of POST /v1/images/generations. Unset optional
// fields are omitted so the vendor's defaults apply.
type imageRequest struct {
	Model        string          `json:"model"`
	Prompt       string          `json:"prompt"`
	Size         string          `json:"size,omitempty"`
	Ratio        string          `json:"ratio,omitempty"`
	ReturnBase64 *bool           `json:"return_base64,omitempty"`
	ExtraBody    *imageExtraBody `json:"extra_body,omitempty"`
}

// imageExtraBody carries the advanced parameters. The vendor's parameter table
// lists a top-level image member, but its examples, troubleshooting notes, and
// integration checklist all place the source images in extra_body.image; this
// adapter follows the latter. response_format belongs here too and must never
// be sent at the top level, which the vendor calls the most common integration
// error.
type imageExtraBody struct {
	Image          []string `json:"image,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"`
}

// imageResponse is the wire body of the image endpoint. The vendor documents
// no usage, output-format, or size echo fields, so they are not modeled; the
// untouched body stays available through ai.ImageResponse.Raw.
type imageResponse struct {
	Created int64             `json:"created"`
	Data    []imagewire.Datum `json:"data"`
}
