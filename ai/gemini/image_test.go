package gemini_test

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageModel_InterfaceConformance(t *testing.T) {
	t.Parallel()

	model := gemini.NewImageModel("gemini-2.5-flash-image")

	var (
		_ ai.ImageModel  = model
		_ ai.ImageEditor = model
	)

	assert.Equal(t, ai.ProviderGemini, model.Provider())
	assert.Equal(t, "gemini-2.5-flash-image", model.ModelID())
}

func TestImageGeneration_FullOptions(t *testing.T) {
	t.Parallel()

	pixel := base64.StdEncoding.EncodeToString([]byte{10, 20, 30, 40})

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/gemini-2.5-flash-image:generateContent", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "reasoning draft", "thought": true},
						{"inlineData": {"mimeType": "image/png", "data": "` + pixel + `"}}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 10,
				"candidatesTokenCount": 1200,
				"thoughtsTokenCount": 50,
				"totalTokenCount": 1260
			}
		}`))
	})

	model := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	opts := gemini.ImageOptions{
		AspectRatio:     "16:9",
		ImageSize:       "2K",
		ThinkingLevel:   "high",
		SearchGrounding: true,
		SafetySettings: []gemini.SafetySetting{
			{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_LOW_AND_ABOVE"},
		},
		ExtraFields: map[string]any{
			"customField": "extraVal",
		},
	}

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt:       "a futuristic city",
		OutputFormat: "png",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderGemini: opts,
		},
	})
	require.NoError(t, err)

	// Verify captured payload
	contents := as[[]any](t, captured["contents"])
	require.Len(t, contents, 1)

	firstContent := as[map[string]any](t, contents[0])
	parts := as[[]any](t, firstContent["parts"])
	require.Len(t, parts, 1)

	firstPart := as[map[string]any](t, parts[0])
	assert.Equal(t, "a futuristic city", firstPart["text"])

	gc := as[map[string]any](t, captured["generationConfig"])
	imgCfg := as[map[string]any](t, gc["imageConfig"])
	assert.Equal(t, "16:9", imgCfg["aspectRatio"])
	assert.Equal(t, "2K", imgCfg["imageSize"])

	thCfg := as[map[string]any](t, gc["thinkingConfig"])
	assert.Equal(t, "high", thCfg["thinkingLevel"])

	tools := as[[]any](t, captured["tools"])
	require.Len(t, tools, 1)

	firstTool := as[map[string]any](t, tools[0])
	assert.Contains(t, firstTool, "google_search")

	safety := as[[]any](t, captured["safetySettings"])
	require.Len(t, safety, 1)

	firstSafety := as[map[string]any](t, safety[0])
	assert.Equal(t, "HARM_CATEGORY_HATE_SPEECH", firstSafety["category"])

	assert.Equal(t, "extraVal", captured["customField"])

	// Verify response
	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte{10, 20, 30, 40}, resp.Images[0].Data)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
	assert.Equal(t, "png", resp.OutputFormat)
	assert.Equal(t, 1260, resp.Usage.TotalTokens)
	assert.Equal(t, 10, resp.Usage.InputTokens)
}

func TestImageGeneration_DimensionResolution(t *testing.T) {
	t.Parallel()

	pixel := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})

	tests := []struct {
		name         string
		reqSize      string
		opts         gemini.ImageOptions
		expectedAR   string
		expectedSize string
	}{
		{
			name:         "Ratio direct in Size",
			reqSize:      "9:16",
			opts:         gemini.ImageOptions{},
			expectedAR:   "9:16",
			expectedSize: "",
		},
		{
			name:         "Resolution in Size",
			reqSize:      "4k",
			opts:         gemini.ImageOptions{},
			expectedAR:   "",
			expectedSize: "4K",
		},
		{
			name:         "Pixels resolution in Size",
			reqSize:      "1920x1080",
			opts:         gemini.ImageOptions{},
			expectedAR:   "16:9",
			expectedSize: "",
		},
		{
			name:    "Explicit options override Size",
			reqSize: "1024x1024",
			opts: gemini.ImageOptions{
				AspectRatio: "3:4",
				ImageSize:   "1K",
			},
			expectedAR:   "3:4",
			expectedSize: "1K",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any

			base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, decodeBody(r, &captured))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}}]},"finishReason":"STOP"}]}`))
			})

			model := gemini.NewImageModel("gemini-2.5-flash-image",
				gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
				gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

			_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
				Prompt: "test",
				Size:   tt.reqSize,
				ProviderOptions: map[ai.Provider]any{
					ai.ProviderGemini: tt.opts,
				},
			})
			require.NoError(t, err)

			gc := as[map[string]any](t, captured["generationConfig"])
			if tt.expectedAR != "" || tt.expectedSize != "" {
				imgCfg := as[map[string]any](t, gc["imageConfig"])
				if tt.expectedAR != "" {
					assert.Equal(t, tt.expectedAR, imgCfg["aspectRatio"])
				}

				if tt.expectedSize != "" {
					assert.Equal(t, tt.expectedSize, imgCfg["imageSize"])
				}
			}
		})
	}
}

func TestImageGeneration_ThoughtImagesFiltered(t *testing.T) {
	t.Parallel()

	thoughtPixel := base64.StdEncoding.EncodeToString([]byte{9, 9, 9})
	finalPixel := base64.StdEncoding.EncodeToString([]byte{42, 42, 42})

	base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"inlineData": {"mimeType": "image/png", "data": "` + thoughtPixel + `"}, "thought": true},
						{"text": "generating final output...", "thought": true},
						{"inlineData": {"mimeType": "image/png", "data": "` + finalPixel + `"}}
					]
				},
				"finishReason": "STOP"
			}]
		}`))
	})

	model := gemini.NewImageModel("gemini-3.1-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.NoError(t, err)

	// Only 1 final image, the thought draft image must be filtered out
	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte{42, 42, 42}, resp.Images[0].Data)
}

func TestImageGeneration_ValidationAndSafetyErrors(t *testing.T) {
	t.Parallel()

	model := gemini.NewImageModel("gemini-2.5-flash-image", gemini.WithAPIKey("gm-test"))

	// Empty prompt
	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "   "})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Negative N
	_, err = model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "cat", N: -1})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Gemini N > 1
	_, err = model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "cat", N: 2})
	require.ErrorIs(t, err, ai.ErrUnsupported)

	// Safety block in candidate finishReason
	baseSafety := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`))
	})
	modelSafety := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(baseSafety),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	_, err = modelSafety.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "harmful prompt"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "SAFETY")

	// Safety block in promptFeedback
	basePromptBlock := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	})
	modelPromptBlock := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(basePromptBlock),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	_, err = modelPromptBlock.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "blocked prompt"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "blockReason: SAFETY")

	// No images returned at all
	baseEmpty := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"I cannot draw that"}]},"finishReason":"STOP"}]}`))
	})
	modelEmpty := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(baseEmpty),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	_, err = modelEmpty.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "draw text only"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "no image returned")
}

func TestImageGeneration_ImagenPredictRouting(t *testing.T) {
	t.Parallel()

	pixel1 := base64.StdEncoding.EncodeToString([]byte{1, 1, 1})
	pixel2 := base64.StdEncoding.EncodeToString([]byte{2, 2, 2})

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/imagen-3.0-generate-002:predict", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"predictions": [
				{"bytesBase64Encoded": "` + pixel1 + `", "mimeType": "image/jpeg"},
				{"bytesBase64Encoded": "` + pixel2 + `", "mimeType": "image/jpeg"}
			]
		}`))
	})

	model := gemini.NewImageModel("imagen-3.0-generate-002",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	opts := gemini.ImageOptions{
		PersonGeneration: "allow_adult",
		ExtraFields: map[string]any{
			"customPredictField": 123,
		},
	}

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "two dogs playing",
		N:      2,
		Size:   "16:9",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderGemini: opts,
		},
	})
	require.NoError(t, err)

	// Verify instances and parameters
	instances := as[[]any](t, captured["instances"])
	require.Len(t, instances, 1)

	firstInst := as[map[string]any](t, instances[0])
	assert.Equal(t, "two dogs playing", firstInst["prompt"])

	params := as[map[string]any](t, captured["parameters"])
	assert.InDelta(t, 2.0, params["sampleCount"], 0.001)
	assert.Equal(t, "16:9", params["aspectRatio"])
	assert.Equal(t, "allow_adult", params["personGeneration"])
	assert.InDelta(t, 123.0, captured["customPredictField"], 0.001)

	require.Len(t, resp.Images, 2)
	assert.Equal(t, []byte{1, 1, 1}, resp.Images[0].Data)
	assert.Equal(t, []byte{2, 2, 2}, resp.Images[1].Data)
	assert.Equal(t, "image/jpeg", resp.Images[0].MIMEType)
	assert.Equal(t, "jpeg", resp.OutputFormat)

	// Imagen validation: N > 4 rejected
	_, err = model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "test", N: 5})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Imagen validation: EditImage not supported
	_, err = model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "edit this",
		Images: []ai.ImagePart{{Source: ai.MediaSource{Data: []byte{1}}}},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)
}

func TestImageEdit_MultiImageEditing(t *testing.T) {
	t.Parallel()

	pixelOut := base64.StdEncoding.EncodeToString([]byte{99, 99, 99})

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/gemini-2.5-flash-image:generateContent", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [
						{"inlineData": {"mimeType": "image/png", "data": "` + pixelOut + `"}}
					]
				},
				"finishReason": "STOP"
			}]
		}`))
	})

	model := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	img1Data := []byte{1, 2}
	img2URI := "gs://my-bucket/image2.png"

	resp, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "blend these two pictures",
		Images: []ai.ImagePart{
			{Source: ai.MediaSource{Data: img1Data, MIMEType: "image/png"}},
			{Source: ai.MediaSource{URL: img2URI, MIMEType: "image/png"}},
		},
		Size: "1:1",
	})
	require.NoError(t, err)

	// Verify captured parts in order: image 1 (inline), image 2 (fileURI), then text prompt
	contents := as[[]any](t, captured["contents"])
	require.Len(t, contents, 1)

	firstContent := as[map[string]any](t, contents[0])
	parts := as[[]any](t, firstContent["parts"])
	require.Len(t, parts, 3)

	part0 := as[map[string]any](t, parts[0])
	inline0 := as[map[string]any](t, part0["inlineData"])
	assert.Equal(t, base64.StdEncoding.EncodeToString(img1Data), inline0["data"])
	assert.Equal(t, "image/png", inline0["mimeType"])

	part1 := as[map[string]any](t, parts[1])
	file1 := as[map[string]any](t, part1["fileData"])
	assert.Equal(t, img2URI, file1["fileUri"])

	part2 := as[map[string]any](t, parts[2])
	assert.Equal(t, "blend these two pictures", part2["text"])

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte{99, 99, 99}, resp.Images[0].Data)
}

func TestImageEdit_ValidationErrors(t *testing.T) {
	t.Parallel()

	model := gemini.NewImageModel("gemini-2.5-flash-image", gemini.WithAPIKey("gm-test"))

	// Empty prompt
	_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "",
		Images: []ai.ImagePart{{Source: ai.MediaSource{Data: []byte{1}}}},
	})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Zero images
	_, err = model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "edit",
		Images: nil,
	})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Mask not supported
	_, err = model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "edit",
		Images: []ai.ImagePart{{Source: ai.MediaSource{Data: []byte{1}}}},
		Mask:   &ai.ImagePart{Source: ai.MediaSource{Data: []byte{2}}},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)

	// More than 14 images rejected
	manyImages := make([]ai.ImagePart, 15)
	for i := range manyImages {
		manyImages[i] = ai.ImagePart{Source: ai.MediaSource{Data: []byte{byte(i)}}}
	}

	_, err = model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "too many",
		Images: manyImages,
	})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Provider ID rejected
	_, err = model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "edit",
		Images: []ai.ImagePart{{Source: ai.MediaSource{ID: "provider-file-id"}}},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)
}
