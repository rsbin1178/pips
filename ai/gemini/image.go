package gemini

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/rsbin1178/pips/ai"
)

// ImageModel is an ai.ImageModel backed by a Gemini image-capable model (for
// example "gemini-3.8-flash-image"). Image output arrives as inlineData parts
// from generateContent. Create one with [NewImageModel].
type ImageModel struct {
	model *Model
}

var _ ai.ImageModel = (*ImageModel)(nil)

// NewImageModel returns an image-generation model bound to the given model ID.
// It accepts the same options as [New].
func NewImageModel(model string, opts ...Option) *ImageModel {
	return &ImageModel{model: New(model, opts...)}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model.model }

// GenerateImages implements ai.ImageModel by asking generateContent for image
// output. Gemini generates one image per call; N is ignored.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	body := generateRequest{
		Contents: []wireContent{{Role: roleUser, Parts: []wirePart{{Text: req.Prompt}}}},
		GenerationConfig: &generationConfig{
			ResponseModalities: []string{"IMAGE"},
		},
	}

	merged, err := mergeExtraFields(body, imageOptions(req).ExtraFields)
	if err != nil {
		return nil, err
	}

	var parsed generateResponse

	raw, err := m.model.client.PostJSON(
		ctx, m.model.methodPath("generateContent"), m.model.authHeaders(), merged, &parsed,
		decodeError(m.model.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: image generateContent: %w", err)
	}

	out := &ai.ImageResponse{Usage: ai.ImageUsage{Usage: usageFrom(parsed.UsageMetadata)}, Raw: raw}
	if len(parsed.Candidates) == 0 {
		return out, nil
	}

	for _, part := range parsed.Candidates[0].Content.Parts {
		if part.InlineData == nil {
			continue
		}

		data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			return nil, fmt.Errorf("gemini: decoding image data: %w", err)
		}

		out.Images = append(out.Images, ai.GeneratedImage{Data: data, MIMEType: part.InlineData.MIMEType})
	}

	return out, nil
}

// ImageOptions is the gemini entry for [ai.ImageRequest.ProviderOptions].
type ImageOptions struct {
	ExtraFields map[string]any
}

func imageOptions(req ai.ImageRequest) ImageOptions {
	if raw, ok := req.ProviderOptions[ai.ProviderGemini]; ok {
		if opts, ok := raw.(ImageOptions); ok {
			return opts
		}
	}

	return ImageOptions{}
}
