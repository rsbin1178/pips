package gemini

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// ImageModel is an ai.ImageModel and ai.ImageEditor backed by Gemini and Imagen
// models (for example "gemini-2.5-flash-image", "gemini-3.1-flash-image", or
// "imagen-3.0-generate-002"). Create one with [NewImageModel].
type ImageModel struct {
	model *Model
}

var (
	_ ai.ImageModel  = (*ImageModel)(nil)
	_ ai.ImageEditor = (*ImageModel)(nil)
)

// NewImageModel returns an image-generation model bound to the given model ID.
// It accepts the same options as [New].
func NewImageModel(model string, opts ...Option) *ImageModel {
	return &ImageModel{model: New(model, opts...)}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model.model }

// SafetySetting configures safety category thresholds for Gemini requests.
type SafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// ImageOptions is the gemini entry for [ai.ImageRequest.ProviderOptions]
// and [ai.ImageEditRequest.ProviderOptions].
type ImageOptions struct {
	// AspectRatio is the target aspect ratio (for example "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9", "4:5", "5:4").
	AspectRatio string
	// ImageSize is the resolution tier (for example "1K", "2K", "4K").
	ImageSize string
	// ResponseModalities overrides the requested output modalities. Defaults to ["IMAGE"].
	ResponseModalities []string
	// ThinkingLevel configures reasoning depth ("low" or "high").
	ThinkingLevel string
	// SearchGrounding attaches Google Search Grounding to the request.
	SearchGrounding bool
	// PersonGeneration configures person generation for Imagen models ("dont_allow", "allow_adult", "allow_all").
	PersonGeneration string
	// SafetySettings configures safety category thresholds.
	SafetySettings []SafetySetting
	// ExtraFields folds arbitrary vendor fields into the request body.
	ExtraFields map[string]any
}

func imageOptions(opts map[ai.Provider]any) ImageOptions {
	if raw, ok := opts[ai.ProviderGemini]; ok {
		switch o := raw.(type) {
		case ImageOptions:
			return o
		case *ImageOptions:
			if o != nil {
				return *o
			}
		}
	}

	return ImageOptions{}
}

// isImagenModel reports whether model refers to an Imagen dedicated image generation model.
func isImagenModel(model string) bool {
	base := strings.ToLower(model)
	base = strings.TrimPrefix(base, "models/")

	return strings.HasPrefix(base, "imagen-")
}

// GenerateImages implements ai.ImageModel by delegating to generateContent (for Gemini)
// or predict (for Imagen).
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if err := validatePrompt(req.Prompt); err != nil {
		return nil, err
	}

	opts := imageOptions(req.ProviderOptions)

	if isImagenModel(m.model.model) {
		return m.generateImagen(ctx, req, opts)
	}

	return m.generateGemini(ctx, req, opts)
}

// generateImagen generates images via the Imagen :predict endpoint.
func (m *ImageModel) generateImagen(ctx context.Context, req ai.ImageRequest, opts ImageOptions) (*ai.ImageResponse, error) {
	if err := validateImagenN(req.N); err != nil {
		return nil, err
	}

	sampleCount := req.N
	if sampleCount == 0 {
		sampleCount = 1
	}

	aspectRatio, _ := resolveDimensions(req.Size, opts)

	params := &predictParameters{
		SampleCount:      sampleCount,
		AspectRatio:      aspectRatio,
		PersonGeneration: opts.PersonGeneration,
	}

	body := predictRequest{
		Instances:  []predictInstance{{Prompt: req.Prompt}},
		Parameters: params,
	}

	merged, err := mergePredictExtraFields(body, opts.ExtraFields)
	if err != nil {
		return nil, err
	}

	var parsed predictResponse

	raw, err := m.model.client.PostJSON(
		ctx, m.model.methodPath("predict"), m.model.authHeaders(), merged, &parsed,
		decodeError(m.model.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: imagen predict: %w", err)
	}

	out, err := parsePredictImageResponse(parsed, raw)
	if err != nil {
		return nil, err
	}

	finalizeImageResponse(out, req.OutputFormat, req.Size)

	return out, nil
}

// generateGemini generates images via the Gemini :generateContent endpoint.
func (m *ImageModel) generateGemini(ctx context.Context, req ai.ImageRequest, opts ImageOptions) (*ai.ImageResponse, error) {
	if err := validateGeminiN(req.N); err != nil {
		return nil, err
	}

	body, err := buildGenerateBody([]wirePart{{Text: req.Prompt}}, req.Size, opts)
	if err != nil {
		return nil, err
	}

	var parsed generateResponse

	raw, err := m.model.client.PostJSON(
		ctx, m.model.methodPath("generateContent"), m.model.authHeaders(), body, &parsed,
		decodeError(m.model.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: image generateContent: %w", err)
	}

	out, err := parseGenerateImageResponse(parsed, raw)
	if err != nil {
		return nil, err
	}

	finalizeImageResponse(out, req.OutputFormat, req.Size)

	return out, nil
}

// EditImage implements ai.ImageEditor by sending multi-part input to generateContent.
func (m *ImageModel) EditImage(ctx context.Context, req ai.ImageEditRequest) (*ai.ImageResponse, error) {
	if err := validateEditRequest(m.model.model, req); err != nil {
		return nil, err
	}

	parts := make([]wirePart, 0, len(req.Images)+1)

	for i, imgPart := range req.Images {
		p, err := editPartFrom(imgPart.Source, i)
		if err != nil {
			return nil, err
		}

		parts = append(parts, p)
	}

	parts = append(parts, wirePart{Text: req.Prompt})

	opts := imageOptions(req.ProviderOptions)

	body, err := buildGenerateBody(parts, req.Size, opts)
	if err != nil {
		return nil, err
	}

	var parsed generateResponse

	raw, err := m.model.client.PostJSON(
		ctx, m.model.methodPath("generateContent"), m.model.authHeaders(), body, &parsed,
		decodeError(m.model.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: image generateContent: %w", err)
	}

	out, err := parseGenerateImageResponse(parsed, raw)
	if err != nil {
		return nil, err
	}

	finalizeImageResponse(out, req.OutputFormat, req.Size)

	return out, nil
}

func validateEditRequest(model string, req ai.ImageEditRequest) error {
	if isImagenModel(model) {
		return fmt.Errorf("gemini: images: model %q does not support image editing: %w", model, ai.ErrUnsupported)
	}

	if err := validatePrompt(req.Prompt); err != nil {
		return err
	}

	if len(req.Images) == 0 {
		return fmt.Errorf("gemini: images: edits need at least one source image: %w", ai.ErrInvalidRequest)
	}

	if len(req.Images) > 14 {
		return fmt.Errorf("gemini: images: at most 14 source images supported, got %d: %w", len(req.Images), ai.ErrInvalidRequest)
	}

	if req.Mask != nil {
		return fmt.Errorf("gemini: images: edits do not support a mask: %w", ai.ErrUnsupported)
	}

	return validateGeminiN(req.N)
}

func buildGenerateBody(parts []wirePart, size string, opts ImageOptions) (any, error) {
	aspectRatio, imageSize := resolveDimensions(size, opts)

	var ic *wireImageConfig
	if aspectRatio != "" || imageSize != "" {
		ic = &wireImageConfig{AspectRatio: aspectRatio, ImageSize: imageSize}
	}

	modalities := opts.ResponseModalities
	if len(modalities) == 0 {
		modalities = []string{"IMAGE"}
	}

	var tc *thinkingConfig
	if opts.ThinkingLevel != "" {
		tc = &thinkingConfig{ThinkingLevel: opts.ThinkingLevel}
	}

	var tools []wireTool
	if opts.SearchGrounding {
		tools = append(tools, wireTool{GoogleSearch: &wireGoogleSearch{}})
	}

	var safety []wireSafetySetting
	for _, s := range opts.SafetySettings {
		safety = append(safety, wireSafetySetting(s))
	}

	body := generateRequest{
		Contents: []wireContent{{Role: roleUser, Parts: parts}},
		Tools:    tools,
		GenerationConfig: &generationConfig{
			ResponseModalities: modalities,
			ThinkingConfig:     tc,
			ImageConfig:        ic,
		},
		SafetySettings: safety,
	}

	return mergeImageExtraFields(body, opts.ExtraFields)
}

func editPartFrom(src ai.MediaSource, index int) (wirePart, error) {
	if src.IsID() {
		return wirePart{}, fmt.Errorf("gemini: images: edit image %d references a file ID, which is not supported: %w", index, ai.ErrUnsupported)
	}

	if src.IsURL() {
		return wirePart{FileData: &wireFileData{FileURI: src.URL, MIMEType: src.MIMEType}}, nil
	}

	if len(src.Data) > 0 {
		mime := src.MIMEType
		if mime == "" {
			mime = "image/png"
		}

		return wirePart{InlineData: &wireBlob{
			MIMEType: mime,
			Data:     base64.StdEncoding.EncodeToString(src.Data),
		}}, nil
	}

	return wirePart{}, fmt.Errorf("gemini: images: edit image %d has no data or url: %w", index, ai.ErrInvalidRequest)
}

func parseGenerateImageResponse(parsed generateResponse, raw []byte) (*ai.ImageResponse, error) {
	out := &ai.ImageResponse{
		Usage: imageUsageFrom(parsed.UsageMetadata),
		Raw:   raw,
	}

	if len(parsed.Candidates) > 0 {
		for _, part := range parsed.Candidates[0].Content.Parts {
			if part.Thought {
				continue
			}

			if part.InlineData == nil {
				continue
			}

			data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini: decoding image data: %w", err)
			}

			out.Images = append(out.Images, ai.GeneratedImage{
				Data:     data,
				MIMEType: part.InlineData.MIMEType,
			})
		}
	}

	if len(out.Images) == 0 {
		if parsed.PromptFeedback != nil && parsed.PromptFeedback.BlockReason != "" {
			return nil, fmt.Errorf("gemini: image generation blocked by safety filters (blockReason: %s): %w", parsed.PromptFeedback.BlockReason, ai.ErrInvalidRequest)
		}

		if len(parsed.Candidates) > 0 {
			reason := parsed.Candidates[0].FinishReason
			if reason != "" && reason != "STOP" {
				return nil, fmt.Errorf("gemini: image generation blocked by safety filters (finishReason: %s): %w", reason, ai.ErrInvalidRequest)
			}
		}

		return nil, fmt.Errorf("gemini: no image returned in response: %w", ai.ErrInvalidRequest)
	}

	return out, nil
}

func parsePredictImageResponse(parsed predictResponse, raw []byte) (*ai.ImageResponse, error) {
	out := &ai.ImageResponse{
		Raw: raw,
	}

	for _, pred := range parsed.Predictions {
		if pred.BytesBase64Encoded == "" {
			continue
		}

		data, err := base64.StdEncoding.DecodeString(pred.BytesBase64Encoded)
		if err != nil {
			return nil, fmt.Errorf("gemini: decoding imagen prediction: %w", err)
		}

		mime := pred.MIMEType
		if mime == "" {
			mime = "image/jpeg"
		}

		out.Images = append(out.Images, ai.GeneratedImage{
			Data:     data,
			MIMEType: mime,
		})
	}

	if len(out.Images) == 0 {
		return nil, fmt.Errorf("gemini: no image returned in imagen prediction: %w", ai.ErrInvalidRequest)
	}

	return out, nil
}

func finalizeImageResponse(out *ai.ImageResponse, reqOutputFormat, reqSize string) {
	if reqOutputFormat != "" {
		out.OutputFormat = reqOutputFormat
	} else if len(out.Images) > 0 {
		out.OutputFormat = mimeToFormat(out.Images[0].MIMEType)
	}

	out.Size = reqSize
}

func mimeToFormat(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

func imageUsageFrom(u *wireUsage) ai.ImageUsage {
	if u == nil {
		return ai.ImageUsage{}
	}

	base := usageFrom(u)
	total := u.TotalTokenCount

	if total == 0 {
		total = base.InputTokens + base.OutputTokens
	}

	return ai.ImageUsage{
		Usage:       base,
		TotalTokens: total,
	}
}

func mergeImageExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra, "contents")
}

func mergePredictExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra, "instances")
}

func validatePrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("gemini: images: prompt is required: %w", ai.ErrInvalidRequest)
	}

	return nil
}

func validateGeminiN(n int) error {
	switch {
	case n > 1:
		return fmt.Errorf("gemini: images: generating more than one image is not supported: %w", ai.ErrUnsupported)
	case n < 0:
		return fmt.Errorf("gemini: images: n must not be negative: %w", ai.ErrInvalidRequest)
	default:
		return nil
	}
}

func validateImagenN(n int) error {
	switch {
	case n > 4:
		return fmt.Errorf("gemini: images: imagen sampleCount cannot exceed 4: %w", ai.ErrInvalidRequest)
	case n < 0:
		return fmt.Errorf("gemini: images: n must not be negative: %w", ai.ErrInvalidRequest)
	default:
		return nil
	}
}

func resolveDimensions(size string, opts ImageOptions) (string, string) {
	aspectRatio := opts.AspectRatio
	imageSize := opts.ImageSize

	s := strings.TrimSpace(size)
	if s == "" {
		return aspectRatio, imageSize
	}

	upper := strings.ToUpper(s)
	if imageSize == "" && (upper == "1K" || upper == "2K" || upper == "4K") {
		imageSize = upper

		return aspectRatio, imageSize
	}

	if aspectRatio == "" {
		switch s {
		case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9", "4:5", "5:4":
			aspectRatio = s

			return aspectRatio, imageSize
		}

		var w, h int
		if n, _ := fmt.Sscanf(s, "%dx%d", &w, &h); n == 2 && w > 0 && h > 0 {
			aspectRatio = closestAspectRatio(float64(w) / float64(h))

			return aspectRatio, imageSize
		}
	}

	return aspectRatio, imageSize
}

func closestAspectRatio(ratio float64) string {
	type candidate struct {
		name string
		val  float64
	}

	candidates := []candidate{
		{"1:1", 1.0},
		{"16:9", 16.0 / 9.0},
		{"9:16", 9.0 / 16.0},
		{"4:3", 4.0 / 3.0},
		{"3:4", 3.0 / 4.0},
		{"3:2", 3.0 / 2.0},
		{"2:3", 2.0 / 3.0},
		{"21:9", 21.0 / 9.0},
		{"4:5", 4.0 / 5.0},
		{"5:4", 5.0 / 4.0},
	}

	best := "1:1"
	minDiff := 1e9

	for _, c := range candidates {
		diff := math.Abs(ratio - c.val)
		if diff < minDiff {
			minDiff = diff
			best = c.name
		}
	}

	return best
}
