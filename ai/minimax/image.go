package minimax

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// imagePath is the MiniMax image generation endpoint, relative to the /v1 base.
const imagePath = "image_generation"

// miniMaxMaxImages caps the documented batch size for one request.
const miniMaxMaxImages = 9

// miniMaxAspectRatios are the aspect ratios the image API documents.
var miniMaxAspectRatios = map[string]struct{}{
	"1:1": {}, "16:9": {}, "4:3": {}, "3:2": {},
	"2:3": {}, "3:4": {}, "9:16": {}, "21:9": {},
}

// ImageModel is an ai.ImageModel backed by the MiniMax image generation API.
// Create one with [NewImageModel]; it is immutable and safe for concurrent use.
//
// MiniMax returns hosted URLs valid for 24 hours, or inline base64 JPEG bytes
// when requested through [ImageOptions.ResponseFormat]. The adapter never
// downloads a URL.
type ImageModel struct {
	model  string
	client *httpx.Client
	apiKey string
}

// Compile-time interface check.
var _ ai.ImageModel = (*ImageModel)(nil)

// ImageOptions carries MiniMax image-generation extensions. Put it in
// [ai.ImageRequest.ProviderOptions] under [ai.ProviderMiniMax].
type ImageOptions struct {
	// ResponseFormat is "url" (the default) or "base64".
	ResponseFormat string
	// PromptOptimizer enables automatic prompt optimization.
	PromptOptimizer *bool
	// Watermark adds the AIGC watermark.
	Watermark *bool
	// Seed makes generation reproducible.
	Seed *int64
	// SubjectReference supplies one portrait reference for image-to-image.
	SubjectReference []SubjectReference
}

// SubjectReference is a MiniMax portrait reference. Type is currently only
// "character"; ImageFile is a public URL or a base64 data URI.
type SubjectReference struct {
	Type      string
	ImageFile string
}

// NewImageModel returns an image model bound to the given model ID (for
// example "image-01"). It defaults to reading the MINIMAX_API_KEY environment
// variable and to [DefaultBaseURL].
func NewImageModel(model string, opts ...Option) *ImageModel {
	o := newOptions(opts)

	return &ImageModel{
		model:  model,
		client: httpx.New(o.ToHTTPXConfig(DefaultBaseURL, "MINIMAX_API_KEY"), DefaultBaseURL),
		apiKey: o.ResolvedAPIKey("MINIMAX_API_KEY"),
	}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return ai.ProviderMiniMax }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model }

// Capabilities implements ai.ImageModel. It is a static hint, never a call
// gate.
func (m *ImageModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{ImageGeneration: true}
}

// GenerateImages implements ai.ImageModel.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("minimax: images: prompt is required: %w", ai.ErrInvalidRequest)
	}

	if req.N > miniMaxMaxImages {
		return nil, fmt.Errorf(
			"minimax: images: n must not exceed %d: %w", miniMaxMaxImages, ai.ErrInvalidRequest,
		)
	}

	opts := imageOptions(req.ProviderOptions)

	body, err := imageBody(m.model, req, opts)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.client.PostJSON(ctx, imagePath, m.authHeaders(), body, &parsed, decodeError())
	if err != nil {
		return nil, fmt.Errorf("minimax: images: %w", err)
	}

	return imageResponseFrom(parsed, raw)
}

// authHeaders returns the per-request authentication headers.
func (m *ImageModel) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}

// imageBody builds the wire request.
func imageBody(model string, req ai.ImageRequest, opts ImageOptions) (imageRequest, error) {
	aspectRatio, width, height, err := sizeFields(req.Size)
	if err != nil {
		return imageRequest{}, err
	}

	body := imageRequest{
		Model:            model,
		Prompt:           req.Prompt,
		AspectRatio:      aspectRatio,
		Width:            width,
		Height:           height,
		ResponseFormat:   opts.ResponseFormat,
		PromptOptimizer:  opts.PromptOptimizer,
		AIGCWatermark:    opts.Watermark,
		Seed:             opts.Seed,
		SubjectReference: subjectReferences(opts.SubjectReference),
	}
	if req.N > 0 {
		body.N = ai.Ptr(req.N)
	}

	return body, nil
}

func subjectReferences(values []SubjectReference) []imageSubjectReference {
	if len(values) == 0 {
		return nil
	}

	out := make([]imageSubjectReference, 0, len(values))
	for _, value := range values {
		referenceType := value.Type
		if referenceType == "" {
			referenceType = "character"
		}

		out = append(out, imageSubjectReference{Type: referenceType, ImageFile: value.ImageFile})
	}

	return out
}

// imageResponseFrom maps the response onto the portable type.
func imageResponseFrom(parsed imageResponse, raw []byte) (*ai.ImageResponse, error) {
	if parsed.BaseResp.StatusCode != 0 {
		apiErr := ai.NewError(ai.ProviderMiniMax, 0, parsed.BaseResp.StatusMsg)
		apiErr.Code = strconv.Itoa(parsed.BaseResp.StatusCode)
		apiErr.Raw = raw

		return nil, fmt.Errorf(
			"minimax: images: %w", apiErr.WithSentinel(miniMaxSentinel(parsed.BaseResp.StatusCode)),
		)
	}

	out := &ai.ImageResponse{Raw: raw}

	for _, url := range parsed.Data.ImageURLs {
		out.Images = append(out.Images, ai.GeneratedImage{URL: url})
	}

	for _, encoded := range parsed.Data.ImageBase64 {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("minimax: images: decode base64 image: %w", err)
		}

		// MiniMax returns JPEG bytes; the official examples decode to .jpeg.
		out.Images = append(out.Images, ai.GeneratedImage{Data: data, MIMEType: "image/jpeg"})
	}

	if len(out.Images) == 0 {
		return nil, fmt.Errorf(
			"minimax: images: no image returned (content safety or empty batch): %w", ai.ErrInvalidRequest,
		)
	}

	return out, nil
}

// sizeFields maps the portable size onto MiniMax's aspect_ratio or
// width/height pair. An empty size leaves both unset so the vendor default
// (1:1) applies.
func sizeFields(size string) (string, *int, *int, error) {
	size = strings.TrimSpace(size)
	if size == "" {
		return "", nil, nil, nil
	}

	if strings.Contains(size, ":") {
		if _, ok := miniMaxAspectRatios[size]; !ok {
			return "", nil, nil, fmt.Errorf(
				"minimax: images: aspect ratio %q is not documented: %w", size, ai.ErrInvalidRequest,
			)
		}

		return size, nil, nil, nil
	}

	left, right, ok := strings.Cut(size, "x")
	if !ok {
		return "", nil, nil, fmt.Errorf(
			`minimax: images: size %q must be "WxH" or a documented aspect ratio: %w`,
			size, ai.ErrInvalidRequest,
		)
	}

	width, widthErr := strconv.Atoi(strings.TrimSpace(left))

	height, heightErr := strconv.Atoi(strings.TrimSpace(right))
	if widthErr != nil || heightErr != nil {
		return "", nil, nil, fmt.Errorf(
			"minimax: images: size %q is not numeric: %w", size, ai.ErrInvalidRequest,
		)
	}

	if width < 512 || width > 2048 || height < 512 || height > 2048 || width%8 != 0 || height%8 != 0 {
		return "", nil, nil, fmt.Errorf(
			"minimax: images: size %q must be 512..2048 per side and divisible by 8: %w",
			size, ai.ErrInvalidRequest,
		)
	}

	return "", &width, &height, nil
}

func imageOptions(values map[ai.Provider]any) ImageOptions {
	if values == nil {
		return ImageOptions{}
	}

	opts, _ := values[ai.ProviderMiniMax].(ImageOptions)

	return opts
}

// decodeError maps an HTTP-level MiniMax error envelope onto an ai.Error.
func decodeError() httpx.ErrorDecoder {
	return func(status int, retryAfter time.Duration, body []byte) error {
		apiErr := ai.NewError(ai.ProviderMiniMax, status, string(body))
		apiErr.RetryAfter = retryAfter
		apiErr.Raw = body

		var envelope struct {
			Message  string `json:"message"`
			BaseResp *struct {
				StatusCode int    `json:"status_code"`
				StatusMsg  string `json:"status_msg"`
			} `json:"base_resp"`
		}
		if err := jsonx.Unmarshal(body, &envelope); err == nil {
			if envelope.Message != "" {
				apiErr.Message = envelope.Message
			}

			if envelope.BaseResp != nil {
				apiErr.Code = strconv.Itoa(envelope.BaseResp.StatusCode)
				if envelope.BaseResp.StatusMsg != "" {
					apiErr.Message = envelope.BaseResp.StatusMsg
				}
			}
		}

		return apiErr
	}
}

// miniMaxSentinel maps a MiniMax base_resp status code to the closest ai
// sentinel. The vendor reports these codes with HTTP 200, so the status alone
// cannot classify them.
func miniMaxSentinel(code int) error {
	switch code {
	case 1002:
		return ai.ErrRateLimited
	case 1004, 2049:
		return ai.ErrAuth
	case 1008, 1026, 2013:
		return ai.ErrInvalidRequest
	default:
		return ai.ErrInvalidRequest
	}
}

// Wire types. metadata is intentionally absent: MiniMax documents its counters
// as both integers and strings, and nothing consumes them.
type imageRequest struct {
	Model            string                  `json:"model"`
	Prompt           string                  `json:"prompt"`
	AspectRatio      string                  `json:"aspect_ratio,omitempty"`
	Width            *int                    `json:"width,omitempty"`
	Height           *int                    `json:"height,omitempty"`
	ResponseFormat   string                  `json:"response_format,omitempty"`
	Seed             *int64                  `json:"seed,omitempty"`
	N                *int                    `json:"n,omitempty"`
	PromptOptimizer  *bool                   `json:"prompt_optimizer,omitempty"`
	AIGCWatermark    *bool                   `json:"aigc_watermark,omitempty"`
	SubjectReference []imageSubjectReference `json:"subject_reference,omitempty"`
}

type imageSubjectReference struct {
	Type      string `json:"type"`
	ImageFile string `json:"image_file"`
}

type imageResponse struct {
	ID   string `json:"id"`
	Data struct {
		ImageURLs   []string `json:"image_urls"`
		ImageBase64 []string `json:"image_base64"`
	} `json:"data"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}
