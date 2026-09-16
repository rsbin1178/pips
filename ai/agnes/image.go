package agnes

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/imagewire"
)

// defaultSourceMIME is the media type of inline edit sources that do not carry
// one. PNG is the format in the vendor's documented responses, so it is the
// least surprising assumption for a source image.
const defaultSourceMIME = "image/png"

// GenerateImages implements ai.ImageModel.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	opts := imageOptions(req.ProviderOptions)

	body, err := generationBody(m.model, req, opts)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.client.PostJSON(ctx, imagesPath, m.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("agnes: images: %w", err)
	}

	return responseFrom(parsed, req.OutputFormat, raw)
}

// EditImage implements ai.ImageEditor. Agnes has no separate edits endpoint:
// the sources travel in extra_body.image of the same generations request.
func (m *ImageModel) EditImage(ctx context.Context, req ai.ImageEditRequest) (*ai.ImageResponse, error) {
	opts := imageOptions(req.ProviderOptions)

	body, err := editBody(m.model, req, opts)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.client.PostJSON(ctx, imagesPath, m.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("agnes: images: %w", err)
	}

	return responseFrom(parsed, req.OutputFormat, raw)
}

// generationBody builds the text-to-image request body.
func generationBody(model string, req ai.ImageRequest, opts ImageOptions) (any, error) {
	if err := validateGeneration(req, opts); err != nil {
		return nil, err
	}

	body := imageRequest{
		Model:  model,
		Prompt: req.Prompt,
		Size:   req.Size,
		Ratio:  opts.Ratio,
	}

	if opts.ReturnBase64 {
		body.ReturnBase64 = ai.Ptr(true)
	}

	if opts.ResponseFormat != "" {
		body.ExtraBody = &imageExtraBody{ResponseFormat: opts.ResponseFormat}
	}

	return mergeImageExtraFields(body, opts.ExtraFields)
}

// editBody builds the image-to-image request body.
func editBody(model string, req ai.ImageEditRequest, opts ImageOptions) (any, error) {
	if err := validateEdit(req, opts); err != nil {
		return nil, err
	}

	images := make([]string, 0, len(req.Images))

	for index, part := range req.Images {
		source, err := imageSource(part.Source, index)
		if err != nil {
			return nil, err
		}

		images = append(images, source)
	}

	body := imageRequest{
		Model:  model,
		Prompt: req.Prompt,
		Size:   req.Size,
		Ratio:  opts.Ratio,
		ExtraBody: &imageExtraBody{
			Image:          images,
			ResponseFormat: opts.ResponseFormat,
		},
	}

	return mergeImageExtraFields(body, opts.ExtraFields)
}

// imageSource renders one edit source as an extra_body.image entry. A public
// URL passes through; inline bytes become a data URI. A provider file ID has
// no Agnes wire representation at all, while a source that is empty or carries
// both members cannot be encoded unambiguously.
func imageSource(source ai.MediaSource, index int) (string, error) {
	if source.IsID() {
		return "", fmt.Errorf(
			"agnes: images: edit image %d references a file ID, which the Agnes API does not accept: %w",
			index, ai.ErrUnsupported,
		)
	}

	switch {
	case source.URL != "" && len(source.Data) == 0:
		return source.URL, nil
	case source.URL == "" && len(source.Data) > 0:
		return dataURI(source), nil
	default:
		return "", fmt.Errorf(
			"agnes: images: edit image %d needs exactly one of url or inline data: %w",
			index, ai.ErrInvalidRequest,
		)
	}
}

// dataURI encodes inline source bytes as the data URI the vendor accepts.
func dataURI(source ai.MediaSource) string {
	mimeType := source.MIMEType
	if mimeType == "" {
		mimeType = defaultSourceMIME
	}

	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(source.Data)
}

// responseFrom maps one decoded response onto the portable type.
// requestedFormat is the format the caller asked for; Agnes reports no output
// format of its own, so the URL extension and the PNG default for inline bytes
// are the remaining MIME sources.
func responseFrom(parsed imageResponse, requestedFormat string, raw []byte) (*ai.ImageResponse, error) {
	out := &ai.ImageResponse{Raw: raw}

	if parsed.Created != 0 {
		out.CreatedAt = time.Unix(parsed.Created, 0).UTC()
	}

	mimeType := imagewire.MIME(requestedFormat)

	for index, datum := range parsed.Data {
		image, err := imagewire.Image(datum, mimeType, index)
		if err != nil {
			return nil, fmt.Errorf("agnes: images: %w", err)
		}

		out.Images = append(out.Images, image)
	}

	return out, nil
}

// validateGeneration rejects a text-to-image request Agnes cannot express.
func validateGeneration(req ai.ImageRequest, opts ImageOptions) error {
	if err := validatePrompt(req.Prompt); err != nil {
		return err
	}

	if strings.TrimSpace(req.Size) == "" {
		return fmt.Errorf(`agnes: images: size is required ("1K", "2K", "3K", or "4K"): %w`, ai.ErrInvalidRequest)
	}

	if err := validateOptions(opts); err != nil {
		return err
	}

	return validateN(req.N)
}

// validateEdit rejects an edit request Agnes cannot express.
func validateEdit(req ai.ImageEditRequest, opts ImageOptions) error {
	if err := validatePrompt(req.Prompt); err != nil {
		return err
	}

	if len(req.Images) == 0 {
		return fmt.Errorf("agnes: images: edits need at least one source image: %w", ai.ErrInvalidRequest)
	}

	if req.Mask != nil {
		return fmt.Errorf("agnes: images: edits do not support a mask: %w", ai.ErrUnsupported)
	}

	if err := validateOptions(opts); err != nil {
		return err
	}

	return validateN(req.N)
}

// validatePrompt rejects an empty prompt locally; Agnes rejects it too, and
// failing before the request keeps that failure cheap and typed.
func validatePrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("agnes: images: prompt is required: %w", ai.ErrInvalidRequest)
	}

	return nil
}

// validateOptions checks the documented Agnes-specific option values.
func validateOptions(opts ImageOptions) error {
	if opts.Ratio != "" && !validRatio(opts.Ratio) {
		return fmt.Errorf("agnes: images: ratio %q is not a documented value: %w", opts.Ratio, ai.ErrInvalidRequest)
	}

	return nil
}

// validRatio reports whether ratio is one of the values the Agnes Image API
// documents. Unknown values fail locally because the vendor's default would
// otherwise silently change the requested framing.
func validRatio(ratio string) bool {
	switch ratio {
	case "1:1", "3:4", "4:3", "16:9", "9:16", "2:3", "3:2", "21:9":
		return true
	default:
		return false
	}
}

// validateN maps the portable count onto Agnes's single-image surface. Zero
// and one are the portable spellings of "one image"; anything above is a
// capability gap rather than a malformed request, and a negative count is
// malformed.
func validateN(n int) error {
	switch {
	case n > 1:
		return fmt.Errorf("agnes: images: generating more than one image is not supported: %w", ai.ErrUnsupported)
	case n < 0:
		return fmt.Errorf("agnes: images: n must not be negative: %w", ai.ErrInvalidRequest)
	default:
		return nil
	}
}
