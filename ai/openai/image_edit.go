package openai

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

const imageEditsPath = "images/edits"

// Image multipart part names. The official multi-image examples send repeated
// "image[]" parts; a single source image keeps the same name.
const (
	imageFormImageField = "image[]"
	imageFormMaskField  = "mask"
)

// maxEditImages is the documented upper bound of source images per edit.
const maxEditImages = 16

// imageEditRequest is the JSON variant of POST /v1/images/edits. Unset
// optional fields are omitted so the API applies its own defaults.
type imageEditRequest struct {
	Model             string          `json:"model"`
	Images            []imageRefParam `json:"images"`
	Mask              *imageRefParam  `json:"mask,omitempty"`
	Prompt            string          `json:"prompt"`
	N                 int             `json:"n,omitempty"`
	Size              string          `json:"size,omitempty"`
	Quality           string          `json:"quality,omitempty"`
	ResponseFormat    string          `json:"response_format,omitempty"`
	OutputFormat      string          `json:"output_format,omitempty"`
	OutputCompression *int            `json:"output_compression,omitempty"`
	Background        string          `json:"background,omitempty"`
	Moderation        string          `json:"moderation,omitempty"`
	InputFidelity     string          `json:"input_fidelity,omitempty"`
	User              string          `json:"user,omitempty"`
	Stream            *bool           `json:"stream,omitempty"`
	PartialImages     *int            `json:"partial_images,omitempty"`
}

// imageRefParam is one JSON image reference; exactly one member is set.
type imageRefParam struct {
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// EditImage implements ai.ImageEditor. The request is encoded as multipart or
// JSON according to [ImageOptions.EditEncoding].
func (m *ImageModel) EditImage(ctx context.Context, req ai.ImageEditRequest) (*ai.ImageResponse, error) {
	opts := imageOptions(req.ProviderOptions, m.model.provider)

	if err := validateImageEditRequest(req, opts); err != nil {
		return nil, err
	}

	if editEncoding(req, opts) == EditEncodingJSON {
		return m.editImageJSON(ctx, req, opts)
	}

	return m.editImageMultipart(ctx, req, opts)
}

// editImageJSON sends the application/json variant of the endpoint.
func (m *ImageModel) editImageJSON(ctx context.Context, req ai.ImageEditRequest, opts ImageOptions) (*ai.ImageResponse, error) {
	body, err := m.editJSONBody(req, opts, false)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.model.client.PostJSON(ctx, imageEditsPath, m.model.authHeaders(), body, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
	}

	return m.imageResponseFrom(parsed, req.OutputFormat, raw)
}

// editImageMultipart sends the multipart/form-data variant of the endpoint.
func (m *ImageModel) editImageMultipart(ctx context.Context, req ai.ImageEditRequest, opts ImageOptions) (*ai.ImageResponse, error) {
	fields, files, err := m.editMultipartParts(req, opts, false)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.model.client.PostMultipart(ctx, imageEditsPath, m.model.authHeaders(), fields, files, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
	}

	return m.imageResponseFrom(parsed, req.OutputFormat, raw)
}

// editEncoding applies the [EditEncodingAuto] rule or returns the explicit
// selection.
func editEncoding(req ai.ImageEditRequest, opts ImageOptions) EditEncoding {
	if opts.EditEncoding != "" && opts.EditEncoding != EditEncodingAuto {
		return opts.EditEncoding
	}

	// Auto: inline bytes are only expressible as multipart files, while URL and
	// file-ID sources are only expressible as JSON references.
	for _, image := range req.Images {
		if len(image.Source.Data) > 0 {
			return EditEncodingMultipart
		}
	}

	return EditEncodingJSON
}

// editJSONBody renders an edit request as the endpoint's JSON variant. stream
// is set by the streaming entry point only.
func (m *ImageModel) editJSONBody(req ai.ImageEditRequest, opts ImageOptions, stream bool) (any, error) {
	images := make([]imageRefParam, 0, len(req.Images))

	for index, part := range req.Images {
		ref, err := imageRefFrom(part, fmt.Sprintf("edit image %d", index))
		if err != nil {
			return nil, err
		}

		images = append(images, ref)
	}

	body := imageEditRequest{
		Model:             m.model.model,
		Images:            images,
		Prompt:            req.Prompt,
		N:                 req.N,
		Size:              req.Size,
		Quality:           req.Quality,
		OutputFormat:      req.OutputFormat,
		ResponseFormat:    opts.ResponseFormat,
		OutputCompression: opts.OutputCompression,
		Background:        opts.Background,
		Moderation:        opts.Moderation,
		InputFidelity:     opts.InputFidelity,
		User:              opts.User,
		PartialImages:     opts.PartialImages,
	}

	if req.Mask != nil {
		mask, err := imageRefFrom(*req.Mask, "edit mask")
		if err != nil {
			return nil, err
		}

		body.Mask = &mask
	}

	if stream {
		body.Stream = ai.Ptr(true)
	}

	return mergeImageExtraFields(body, opts.ExtraFields)
}

// editMultipartParts renders an edit request as multipart text fields and
// files. Multipart can only carry inline bytes, so URL and file-ID sources are
// rejected here.
func (m *ImageModel) editMultipartParts(req ai.ImageEditRequest, opts ImageOptions, stream bool) ([]httpx.FormField, []httpx.FormFile, error) {
	fields := formFields{{Name: "model", Value: m.model.model}}

	fields.add("prompt", req.Prompt)
	fields.addInt("n", req.N)
	fields.add("size", req.Size)
	fields.add("quality", req.Quality)
	fields.add("output_format", req.OutputFormat)
	fields.add("response_format", opts.ResponseFormat)
	fields.addIntPtr("output_compression", opts.OutputCompression)
	fields.add("background", opts.Background)
	fields.add("moderation", opts.Moderation)
	fields.add("input_fidelity", opts.InputFidelity)
	fields.add("user", opts.User)
	fields.addIntPtr("partial_images", opts.PartialImages)
	fields.addBool("stream", stream)

	extra, err := mergeImageFormFields(opts.ExtraFields, fields)
	if err != nil {
		return nil, nil, err
	}

	fields = append(fields, extra...)

	files := make([]httpx.FormFile, 0, len(req.Images)+1)

	for index, part := range req.Images {
		file, err := imageFormFile(part, imageFormImageField, fmt.Sprintf("image-%d", index))
		if err != nil {
			return nil, nil, err
		}

		files = append(files, file)
	}

	if req.Mask != nil {
		file, err := imageFormFile(*req.Mask, imageFormMaskField, "mask")
		if err != nil {
			return nil, nil, err
		}

		files = append(files, file)
	}

	return fields, files, nil
}

// imageFormFile renders one inline image source as a multipart file part.
// partName and namePrefix are adapter-generated, never caller input.
func imageFormFile(part ai.ImagePart, partName, namePrefix string) (httpx.FormFile, error) {
	if _, err := classifyImageSource(part.Source, partName); err != nil {
		return httpx.FormFile{}, err
	}

	if len(part.Source.Data) == 0 {
		return httpx.FormFile{}, fmt.Errorf(
			"openai: images: %s has no inline data; multipart cannot reference a URL or file ID: %w",
			partName, ai.ErrInvalidRequest,
		)
	}

	mimeType := imageSourceMIME(part.Source)

	return httpx.FormFile{
		Field:       partName,
		Name:        namePrefix + "." + imageExtension(mimeType),
		ContentType: mimeType,
		Data:        part.Source.Data,
	}, nil
}

// validateImageEditRequest rejects structurally invalid edits before any
// request is sent.
func validateImageEditRequest(req ai.ImageEditRequest, opts ImageOptions) error {
	if err := validateImagePrompt(req.Prompt); err != nil {
		return err
	}

	if len(req.Images) == 0 {
		return fmt.Errorf("openai: images: edits need at least one source image: %w", ai.ErrInvalidRequest)
	}

	if len(req.Images) > maxEditImages {
		return fmt.Errorf("openai: images: edits accept at most %d source images: %w", maxEditImages, ai.ErrInvalidRequest)
	}

	if err := validateImageOptions(opts); err != nil {
		return err
	}

	return validateEditEncoding(opts.EditEncoding)
}

// validateEditEncoding rejects an unknown [EditEncoding] value instead of
// silently falling back to one of the encodings.
func validateEditEncoding(encoding EditEncoding) error {
	switch encoding {
	case "", EditEncodingAuto, EditEncodingMultipart, EditEncodingJSON:
		return nil
	default:
		return fmt.Errorf("openai: images: unknown edit encoding %q: %w", encoding, ai.ErrInvalidRequest)
	}
}
