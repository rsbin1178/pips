package openai

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

const imageVariationsPath = "images/variations"

// imageVariationFormField is the multipart part name of the variation source
// image.
const imageVariationFormField = "image"

// CreateVariations implements ai.ImageVariator. The endpoint is
// multipart/form-data only.
func (m *ImageModel) CreateVariations(ctx context.Context, req ai.ImageVariationRequest) (*ai.ImageResponse, error) {
	opts := imageOptions(req.ProviderOptions, m.model.provider)

	if err := validateImageVariationRequest(req); err != nil {
		return nil, err
	}

	fields, err := variationFormFields(m.model.model, req, opts)
	if err != nil {
		return nil, err
	}

	mimeType := imageSourceMIME(req.Image.Source)
	files := []httpx.FormFile{{
		Field:       imageVariationFormField,
		Name:        "image." + imageExtension(mimeType),
		ContentType: mimeType,
		Data:        req.Image.Source.Data,
	}}

	var parsed imageResponse

	raw, err := m.model.client.PostMultipart(ctx, imageVariationsPath, m.model.authHeaders(), fields, files, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: images: %w", m.model.label(), err)
	}

	return m.imageResponseFrom(parsed, "", raw)
}

// variationFormFields renders the variation endpoint's multipart text fields.
func variationFormFields(model string, req ai.ImageVariationRequest, opts ImageOptions) ([]httpx.FormField, error) {
	fields := formFields{{Name: "model", Value: model}}

	fields.addInt("n", req.N)
	fields.add("size", req.Size)
	fields.add("response_format", opts.ResponseFormat)
	fields.add("user", opts.User)

	extra, err := mergeImageFormFields(opts.ExtraFields, fields)
	if err != nil {
		return nil, err
	}

	return append(fields, extra...), nil
}

// validateImageVariationRequest rejects a variation the endpoint cannot
// express. Variations require one inline source image, and OpenAI documents
// dall-e-2 as the only model.
func validateImageVariationRequest(req ai.ImageVariationRequest) error {
	if _, err := classifyImageSource(req.Image.Source, "variation image"); err != nil {
		return err
	}

	if len(req.Image.Source.Data) == 0 {
		return fmt.Errorf("openai: images: variations need inline image data: %w", ai.ErrInvalidRequest)
	}

	return nil
}
