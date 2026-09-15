package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

const imagesPath = "images/generations"

// Image media types, mapped from the wire output_format.
const (
	imageMediaPNG  = "image/png"
	imageMediaJPEG = "image/jpeg"
	imageMediaWebP = "image/webp"
)

// ImageModel is an ai.ImageModel backed by OpenAI's Images API: generation,
// edits, and variations (for example the "gpt-image-1" model). Create one with
// [NewImageModel].
type ImageModel struct {
	model *Model
}

// Compile-time interface checks.
var (
	_ ai.ImageModel    = (*ImageModel)(nil)
	_ ai.ImageEditor   = (*ImageModel)(nil)
	_ ai.ImageVariator = (*ImageModel)(nil)
	_ ai.ImageStreamer = (*ImageModel)(nil)
)

// NewImageModel returns an image model bound to the given model ID (for
// example "gpt-image-1"). It accepts the same options as [New].
func NewImageModel(model string, opts ...Option) *ImageModel {
	return &ImageModel{model: New(model, opts...)}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model.model }

// imageRequest is the wire body of POST /v1/images/generations. Unset optional
// fields are omitted so the API applies its own defaults.
type imageRequest struct {
	Model             string `json:"model"`
	Prompt            string `json:"prompt"`
	N                 int    `json:"n,omitempty"`
	Size              string `json:"size,omitempty"`
	Quality           string `json:"quality,omitempty"`
	ResponseFormat    string `json:"response_format,omitempty"`
	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression *int   `json:"output_compression,omitempty"`
	Background        string `json:"background,omitempty"`
	Moderation        string `json:"moderation,omitempty"`
	Style             string `json:"style,omitempty"`
	User              string `json:"user,omitempty"`
	Stream            *bool  `json:"stream,omitempty"`
	PartialImages     *int   `json:"partial_images,omitempty"`
}

// imageResponse is the wire body shared by all three images endpoints.
type imageResponse struct {
	Created      int64        `json:"created"`
	Data         []imageDatum `json:"data"`
	Background   string       `json:"background"`
	OutputFormat string       `json:"output_format"`
	Size         string       `json:"size"`
	Quality      string       `json:"quality"`
	Usage        *imageUsage  `json:"usage"`
}

type imageDatum struct {
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt"`
}

type imageUsageDetails struct {
	TextTokens  int `json:"text_tokens"`
	ImageTokens int `json:"image_tokens"`
}

type imageUsage struct {
	TotalTokens         int                `json:"total_tokens"`
	InputTokens         int                `json:"input_tokens"`
	OutputTokens        int                `json:"output_tokens"`
	InputTokensDetails  *imageUsageDetails `json:"input_tokens_details"`
	OutputTokensDetails *imageUsageDetails `json:"output_tokens_details"`
}

// ImageOptions is the openai entry for the ProviderOptions of
// [ai.ImageRequest], [ai.ImageEditRequest], and [ai.ImageVariationRequest].
// Options the target endpoint does not model at all are dropped; whether a
// model accepts a parameter its endpoint defines is left to the API.
type ImageOptions struct {
	// Background requests a "transparent", "opaque", or "auto" background.
	Background string
	// Moderation selects the GPT image moderation level, "low" or "auto".
	Moderation string
	// ResponseFormat selects "url" or "b64_json" on dall-e-2 and dall-e-3.
	// GPT image models always return base64.
	ResponseFormat string
	// Style selects the dall-e-3 style, "vivid" or "natural".
	Style string
	// User is the end-user safety identifier.
	User string
	// OutputCompression sets the 0-100 compression level for jpeg and webp
	// output. Nil omits the field; a pointer keeps an explicit zero
	// distinguishable from "unset".
	OutputCompression *int
	// PartialImages requests 0-3 partial images while streaming. It only takes
	// effect through [ai.ImageStreamer].
	PartialImages *int
	// InputFidelity selects the edit input fidelity, "high" or "low".
	InputFidelity string
	// EditEncoding selects how edit requests are encoded on the wire. The zero
	// value is [EditEncodingAuto].
	EditEncoding EditEncoding
	// ExtraFields is merged into the outgoing request; it never overrides a
	// typed field. Multipart requests accept scalar JSON values (string, bool,
	// number) only, because form fields are flat.
	ExtraFields map[string]any
}

// EditEncoding selects the wire encoding of an image edit request.
type EditEncoding string

// Edit encodings.
const (
	// EditEncodingAuto picks multipart when any source image carries inline
	// bytes, and JSON when every source image is a URL or file ID.
	EditEncodingAuto EditEncoding = "auto"
	// EditEncodingMultipart forces multipart/form-data, which requires inline
	// image bytes.
	EditEncodingMultipart EditEncoding = "multipart"
	// EditEncodingJSON forces application/json, whose image references carry a
	// URL, a file ID, or a data URL.
	EditEncodingJSON EditEncoding = "json"
)

// imageOptions extracts this provider's options from a ProviderOptions map.
func imageOptions(options map[ai.Provider]any, provider ai.Provider) ImageOptions {
	if raw, ok := options[provider]; ok {
		if opts, ok := raw.(ImageOptions); ok {
			return opts
		}
	}

	if provider != ai.ProviderOpenAI {
		if raw, ok := options[ai.ProviderOpenAI]; ok {
			if opts, ok := raw.(ImageOptions); ok {
				return opts
			}
		}
	}

	return ImageOptions{}
}

// GenerateImages implements ai.ImageModel.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	opts := imageOptions(req.ProviderOptions, m.model.provider)

	body, err := m.imageGenerationBody(req, opts, false)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.model.client.PostJSON(ctx, imagesPath, m.model.authHeaders(), body, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
	}

	return m.imageResponseFrom(parsed, req.OutputFormat, raw)
}

// imageGenerationBody builds the generations request body. stream is set by
// the streaming entry points only.
func (m *ImageModel) imageGenerationBody(req ai.ImageRequest, opts ImageOptions, stream bool) (any, error) {
	if err := validateImagePrompt(req.Prompt); err != nil {
		return nil, err
	}

	if err := validateImageOptions(opts); err != nil {
		return nil, err
	}

	body := imageRequest{
		Model:             m.model.model,
		Prompt:            req.Prompt,
		N:                 req.N,
		Size:              req.Size,
		Quality:           req.Quality,
		OutputFormat:      req.OutputFormat,
		ResponseFormat:    opts.ResponseFormat,
		OutputCompression: opts.OutputCompression,
		Background:        opts.Background,
		Moderation:        opts.Moderation,
		Style:             opts.Style,
		User:              opts.User,
		PartialImages:     opts.PartialImages,
	}

	if stream {
		body.Stream = ai.Ptr(true)
	}

	return mergeImageExtraFields(body, opts.ExtraFields)
}

// imageResponseFrom maps one decoded wire response onto the portable type.
// requestedFormat is the format asked for with the call; the response's own
// output_format wins over it.
func (m *ImageModel) imageResponseFrom(parsed imageResponse, requestedFormat string, raw []byte) (*ai.ImageResponse, error) {
	out := &ai.ImageResponse{
		Usage:        imageUsageFrom(parsed.Usage),
		OutputFormat: parsed.OutputFormat,
		Size:         parsed.Size,
		Quality:      parsed.Quality,
		Background:   parsed.Background,
		Raw:          raw,
	}

	if parsed.Created != 0 {
		out.CreatedAt = time.Unix(parsed.Created, 0).UTC()
	}

	mimeType := imageMIMEFor(parsed.OutputFormat, requestedFormat)

	for index, datum := range parsed.Data {
		image, err := imageFromDatum(datum, mimeType, index)
		if err != nil {
			return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
		}

		out.Images = append(out.Images, image)
	}

	return out, nil
}

// imageFromDatum maps one response data item. mimeType is the media type
// inferred from the response metadata or the request; empty means unknown.
func imageFromDatum(datum imageDatum, mimeType string, index int) (ai.GeneratedImage, error) {
	switch {
	case datum.B64JSON != "":
		data, err := base64.StdEncoding.DecodeString(datum.B64JSON)
		if err != nil {
			return ai.GeneratedImage{}, fmt.Errorf("decoding image %d: %w", index, err)
		}

		if mimeType == "" {
			mimeType = imageMediaPNG
		}

		return ai.GeneratedImage{Data: data, MIMEType: mimeType, RevisedPrompt: datum.RevisedPrompt}, nil
	case datum.URL != "":
		if mimeType == "" {
			mimeType = imageMIMEFromURL(datum.URL)
		}

		return ai.GeneratedImage{URL: datum.URL, MIMEType: mimeType, RevisedPrompt: datum.RevisedPrompt}, nil
	default:
		return ai.GeneratedImage{}, fmt.Errorf("data item %d carries neither b64_json nor url", index)
	}
}

// imageMIME maps a wire format name to a media type. It returns "" for an
// unknown or empty format rather than guessing.
func imageMIME(format string) string {
	switch strings.ToLower(format) {
	case "png":
		return imageMediaPNG
	case "jpeg", "jpg":
		return imageMediaJPEG
	case "webp":
		return imageMediaWebP
	default:
		return ""
	}
}

// imageMIMEFor picks the media type of produced image bytes: the format the
// provider reports first, then the format the caller requested.
func imageMIMEFor(outputFormat, requestedFormat string) string {
	if mimeType := imageMIME(outputFormat); mimeType != "" {
		return mimeType
	}

	return imageMIME(requestedFormat)
}

// imageMIMEFromURL infers a media type from a URL's file extension. It returns
// "" when the extension is unknown.
func imageMIMEFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	return imageMIME(strings.TrimPrefix(strings.ToLower(path.Ext(parsed.Path)), "."))
}

// imageUsageFrom maps the wire usage object onto the portable counters.
func imageUsageFrom(usage *imageUsage) ai.ImageUsage {
	if usage == nil {
		return ai.ImageUsage{}
	}

	out := ai.ImageUsage{
		Usage:       ai.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
		TotalTokens: usage.TotalTokens,
	}

	if usage.InputTokensDetails != nil {
		out.InputTextTokens = usage.InputTokensDetails.TextTokens
		out.InputImageTokens = usage.InputTokensDetails.ImageTokens
	}

	if usage.OutputTokensDetails != nil {
		out.OutputTextTokens = usage.OutputTokensDetails.TextTokens
		out.OutputImageTokens = usage.OutputTokensDetails.ImageTokens
	}

	return out
}

// imageSourceMIME returns a source's media type. Inline data without one is
// treated as PNG, OpenAI's default output format.
func imageSourceMIME(source ai.MediaSource) string {
	if source.MIMEType != "" {
		return source.MIMEType
	}

	return imageMediaPNG
}

// imageExtension returns the filename extension for a media type. Unknown
// types fall back to "bin".
func imageExtension(mimeType string) string {
	switch mimeType {
	case imageMediaPNG:
		return "png"
	case imageMediaJPEG:
		return "jpeg"
	case imageMediaWebP:
		return "webp"
	default:
		return "bin"
	}
}

// validateImagePrompt rejects an empty prompt locally; the API rejects it too,
// and failing before the request keeps that failure cheap and typed.
func validateImagePrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("openai: images: prompt is required: %w", ai.ErrInvalidRequest)
	}

	return nil
}

// validateImageOptions applies the documented value ranges of the shared
// image parameters.
func validateImageOptions(opts ImageOptions) error {
	if opts.PartialImages != nil && (*opts.PartialImages < 0 || *opts.PartialImages > maxPartialImages) {
		return fmt.Errorf("openai: images: partial_images must be within 0..%d: %w", maxPartialImages, ai.ErrInvalidRequest)
	}

	if opts.OutputCompression != nil && (*opts.OutputCompression < 0 || *opts.OutputCompression > maxOutputCompression) {
		return fmt.Errorf("openai: images: output_compression must be within 0..%d: %w", maxOutputCompression, ai.ErrInvalidRequest)
	}

	return nil
}

// maxPartialImages is the documented upper bound of partial_images.
const maxPartialImages = 3

// maxOutputCompression is the documented upper bound of output_compression.
const maxOutputCompression = 100

// imageSourceKind identifies which member of an edit image reference a source
// maps to.
type imageSourceKind int

const (
	imageSourceData imageSourceKind = iota
	imageSourceURL
	imageSourceID
)

// classifyImageSource enforces exactly one of file ID, URL, or inline bytes.
func classifyImageSource(source ai.MediaSource, label string) (imageSourceKind, error) {
	sources := 0
	if source.IsID() {
		sources++
	}

	if source.IsURL() {
		sources++
	}

	if len(source.Data) > 0 {
		sources++
	}

	if sources != 1 {
		return 0, fmt.Errorf(
			"openai: images: %s needs exactly one of inline data, url, or file ID: %w",
			label, ai.ErrInvalidRequest,
		)
	}

	switch {
	case source.IsID():
		return imageSourceID, nil
	case source.IsURL():
		return imageSourceURL, nil
	default:
		return imageSourceData, nil
	}
}

// imageRefFrom maps one image source onto a JSON edit reference.
func imageRefFrom(part ai.ImagePart, label string) (imageRefParam, error) {
	kind, err := classifyImageSource(part.Source, label)
	if err != nil {
		return imageRefParam{}, err
	}

	switch kind {
	case imageSourceID:
		return imageRefParam{FileID: part.Source.ID}, nil
	case imageSourceURL:
		return imageRefParam{ImageURL: part.Source.URL}, nil
	default:
		source := part.Source
		source.MIMEType = imageSourceMIME(source)

		return imageRefParam{ImageURL: dataURL(source)}, nil
	}
}

// formFields accumulates image multipart text fields. Empty and zero values
// are omitted so the API applies its own defaults.
type formFields []httpx.FormField

func (f *formFields) add(name, value string) {
	if value != "" {
		*f = append(*f, httpx.FormField{Name: name, Value: value})
	}
}

func (f *formFields) addInt(name string, value int) {
	if value != 0 {
		*f = append(*f, httpx.FormField{Name: name, Value: strconv.Itoa(value)})
	}
}

func (f *formFields) addIntPtr(name string, value *int) {
	if value != nil {
		*f = append(*f, httpx.FormField{Name: name, Value: strconv.Itoa(*value)})
	}
}

func (f *formFields) addBool(name string, value bool) {
	if value {
		*f = append(*f, httpx.FormField{Name: name, Value: "true"})
	}
}
