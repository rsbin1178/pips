package openai

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/rsbin1178/pips/ai"
)

const imagesPath = "images/generations"

// ImageModel is an ai.ImageModel backed by OpenAI's image generation endpoint
// (for example the "gpt-image-1" model). Create one with [NewImageModel].
type ImageModel struct {
	model *Model
}

var _ ai.ImageModel = (*ImageModel)(nil)

// NewImageModel returns an image-generation model bound to the given model ID
// (for example "gpt-image-1"). It accepts the same options as [New].
func NewImageModel(model string, opts ...Option) *ImageModel {
	return &ImageModel{model: New(model, opts...)}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model.model }

type imageRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	N       int    `json:"n,omitempty"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}

type imageResponse struct {
	Data  []imageDatum `json:"data"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type imageDatum struct {
	B64JSON string `json:"b64_json"`
}

// GenerateImages implements ai.ImageModel. OpenAI returns base64 PNG data.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	body, err := mergeExtraFields(imageRequest{
		Model:   m.model.model,
		Prompt:  req.Prompt,
		N:       req.N,
		Size:    req.Size,
		Quality: req.Quality,
	}, imageOptions(req, m.model.provider).ExtraFields)
	if err != nil {
		return nil, err
	}

	var parsed imageResponse

	raw, err := m.model.client.PostJSON(ctx, imagesPath, m.model.authHeaders(), body, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
	}

	out := &ai.ImageResponse{Raw: raw}

	for _, datum := range parsed.Data {
		data, err := base64.StdEncoding.DecodeString(datum.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("openai: decoding image data: %w", err)
		}

		out.Images = append(out.Images, ai.GeneratedImage{Data: data, MIMEType: "image/png"})
	}

	if parsed.Usage != nil {
		out.Usage = ai.Usage{InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens}
	}

	return out, nil
}

// ImageOptions is the openai entry for [ai.ImageRequest.ProviderOptions].
type ImageOptions struct {
	// ExtraFields is merged into the outgoing JSON request (background,
	// moderation, output_format, ...).
	ExtraFields map[string]any
}

func imageOptions(req ai.ImageRequest, provider ai.Provider) ImageOptions {
	if raw, ok := req.ProviderOptions[provider]; ok {
		if opts, ok := raw.(ImageOptions); ok {
			return opts
		}
	}

	if provider != ai.ProviderOpenAI {
		if raw, ok := req.ProviderOptions[ai.ProviderOpenAI]; ok {
			if opts, ok := raw.(ImageOptions); ok {
				return opts
			}
		}
	}

	return ImageOptions{}
}
