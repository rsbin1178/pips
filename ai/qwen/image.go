package qwen

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

// DashScope asynchronous image paths, relative to the native API base.
const (
	// imageSynthesisPath serves the prompt-based text-to-image family.
	imageSynthesisPath = "services/aigc/text2image/image-synthesis"
	// imageGenerationPath serves the wan2.6 messages-based family.
	imageGenerationPath = "services/aigc/image-generation/generation"
	// taskPathPrefix is the shared task-status endpoint.
	taskPathPrefix = "tasks/"
)

// asyncHeader turns a submission into a task.
const asyncHeader = "X-DashScope-Async"

// Task statuses DashScope reports.
const (
	taskSucceeded = "SUCCEEDED"
	taskFailed    = "FAILED"
	taskCanceled  = "CANCELED"
	taskUnknown   = "UNKNOWN"
)

// ImageModel is an ai.ImageModel backed by the DashScope asynchronous image
// generation API. Create one with [NewImageModel]; it is immutable and safe for
// concurrent use.
//
// DashScope returns hosted URLs that expire after 24 hours; the adapter does
// not download them.
type ImageModel struct {
	model        string
	client       *httpx.Client
	apiKey       string
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// Compile-time interface check.
var _ ai.ImageModel = (*ImageModel)(nil)

// ImageOptions carries DashScope image-generation extensions. Put it in
// [ai.ImageRequest.ProviderOptions] under [ai.ProviderQwen].
type ImageOptions struct {
	// NegativePrompt describes what to avoid.
	NegativePrompt string
	// PromptExtend enables DashScope prompt rewriting.
	PromptExtend *bool
	// Watermark adds the DashScope watermark when true.
	Watermark *bool
	// Seed makes generation reproducible.
	Seed *int64
}

// NewImageModel returns an image model bound to the given model ID (for
// example "qwen-image-plus", "wan2.5-t2i-preview", or "wan2.6-t2i"). It
// defaults to reading the DASHSCOPE_API_KEY environment variable and to
// [DefaultHTTPAPIURL].
func NewImageModel(model string, opts ...Option) *ImageModel {
	o := newOptions(opts)

	pollInterval := o.pollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}

	pollTimeout := o.pollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}

	return &ImageModel{
		model:        model,
		client:       httpx.New(o.ToHTTPXConfig(DefaultHTTPAPIURL, "DASHSCOPE_API_KEY"), DefaultHTTPAPIURL),
		apiKey:       o.ResolvedAPIKey("DASHSCOPE_API_KEY"),
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
	}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return ai.ProviderQwen }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model }

// Capabilities implements ai.ImageModel. It is a static hint, never a call
// gate.
func (m *ImageModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{ImageGeneration: true}
}

// GenerateImages implements ai.ImageModel. DashScope is task-based: the
// adapter submits the job, then polls until it reaches a terminal state.
func (m *ImageModel) GenerateImages(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("qwen: images: prompt is required: %w", ai.ErrInvalidRequest)
	}

	opts := imageOptions(req.ProviderOptions)
	headers := m.authHeaders()
	headers.Set(asyncHeader, "enable")

	taskID, err := m.submit(ctx, headers, req, opts)
	if err != nil {
		return nil, err
	}

	return m.wait(ctx, taskID, req)
}

// submit creates the asynchronous task and returns its ID.
func (m *ImageModel) submit(
	ctx context.Context,
	headers http.Header,
	req ai.ImageRequest,
	opts ImageOptions,
) (string, error) {
	parameters := imageParameters{
		Size:           normalizeSize(req.Size),
		N:              ai.Ptr(req.N),
		NegativePrompt: opts.NegativePrompt,
		PromptExtend:   opts.PromptExtend,
		Watermark:      opts.Watermark,
		Seed:           opts.Seed,
	}
	if req.N <= 0 {
		parameters.N = nil
	}

	path := imageSynthesisPath
	body := any(imageSynthesisRequest{
		Model:      m.model,
		Input:      imagePromptInput{Prompt: req.Prompt},
		Parameters: parameters,
	})

	if isMessagesImageModel(m.model) {
		path = imageGenerationPath
		body = imageGenerationRequest{
			Model: m.model,
			Input: imageMessagesInput{Messages: []imageMessage{{
				Role:    "user",
				Content: []imageContent{{Text: req.Prompt}},
			}}},
			Parameters: parameters,
		}
	}

	var parsed imageTaskSubmitResponse

	raw, err := m.client.PostJSON(ctx, path, headers, body, &parsed, decodeError())
	if err != nil {
		return "", fmt.Errorf("qwen: images: submit: %w", err)
	}

	if err := responseError(parsed.Code, parsed.Message, raw); err != nil {
		return "", fmt.Errorf("qwen: images: submit: %w", err)
	}

	if parsed.Output.TaskID == "" {
		return "", fmt.Errorf("qwen: images: submit: response carried no task_id: %w", ai.ErrUnsupported)
	}

	return parsed.Output.TaskID, nil
}

// wait polls the task until it succeeds, fails, or the deadline expires.
func (m *ImageModel) wait(ctx context.Context, taskID string, req ai.ImageRequest) (*ai.ImageResponse, error) {
	pollCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		pollCtx, cancel = context.WithTimeout(ctx, m.pollTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		var parsed imageTaskResponse

		raw, err := m.client.GetJSON(
			pollCtx, taskPathPrefix+taskID, m.authHeaders(), &parsed, decodeError(),
		)
		if err != nil {
			return nil, fmt.Errorf("qwen: images: poll task %s: %w", taskID, err)
		}

		switch parsed.Output.TaskStatus {
		case taskSucceeded:
			return imageResponseFrom(parsed, raw, req), nil
		case taskFailed, taskCanceled, taskUnknown:
			if err := responseError(parsed.Output.Code, parsed.Output.Message, raw); err != nil {
				return nil, fmt.Errorf("qwen: images: task %s failed: %w", taskID, err)
			}

			return nil, fmt.Errorf(
				"qwen: images: task %s reported %s: %w",
				taskID, parsed.Output.TaskStatus, ai.ErrInvalidRequest,
			)
		}

		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("qwen: images: task %s did not finish: %w", taskID, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// authHeaders returns the per-request authentication headers.
func (m *ImageModel) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}

// imageResponseFrom maps the terminal task body onto the portable type. Both
// DashScope result shapes are accepted: the wan2.5-and-earlier results array
// and the wan2.6 choices envelope.
func imageResponseFrom(parsed imageTaskResponse, raw []byte, req ai.ImageRequest) *ai.ImageResponse {
	out := &ai.ImageResponse{Raw: raw, Size: parsed.Usage.Size}
	if out.Size == "" {
		out.Size = normalizeSize(req.Size)
	}

	for _, result := range parsed.Output.Results {
		out.Images = append(out.Images, ai.GeneratedImage{
			URL:           result.URL,
			RevisedPrompt: result.ActualPrompt,
		})
	}

	for _, choice := range parsed.Output.Choices {
		for _, content := range choice.Message.Content {
			if content.Type != "" && content.Type != "image" {
				continue
			}

			if content.Image == "" {
				continue
			}

			out.Images = append(out.Images, ai.GeneratedImage{URL: content.Image})
		}
	}

	out.Usage = ai.ImageUsage{
		Usage: ai.Usage{
			InputTokens:  parsed.Usage.InputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
		},
		TotalTokens: parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
	}

	return out
}

// isMessagesImageModel reports whether the model uses the wan2.6 messages
// submission shape rather than the prompt-based synthesis endpoint.
func isMessagesImageModel(model string) bool {
	return strings.HasPrefix(model, "wan2.6")
}

// normalizeSize rewrites the OpenAI-style "1024x1024" spelling into the
// "1024*1024" DashScope accepts.
func normalizeSize(size string) string {
	if !strings.Contains(size, "x") {
		return size
	}

	left, right, ok := strings.Cut(size, "x")
	if !ok || left == "" || right == "" {
		return size
	}

	return left + "*" + right
}

func imageOptions(values map[ai.Provider]any) ImageOptions {
	if values == nil {
		return ImageOptions{}
	}

	opts, _ := values[ai.ProviderQwen].(ImageOptions)

	return opts
}

// Wire types.
type imageSynthesisRequest struct {
	Model      string           `json:"model"`
	Input      imagePromptInput `json:"input"`
	Parameters imageParameters  `json:"parameters,omitzero"`
}

type imageGenerationRequest struct {
	Model      string             `json:"model"`
	Input      imageMessagesInput `json:"input"`
	Parameters imageParameters    `json:"parameters,omitzero"`
}

type imagePromptInput struct {
	Prompt string `json:"prompt"`
}

type imageMessagesInput struct {
	Messages []imageMessage `json:"messages"`
}

type imageMessage struct {
	Role    string         `json:"role"`
	Content []imageContent `json:"content"`
}

type imageContent struct {
	Text string `json:"text"`
}

type imageParameters struct {
	Size           string `json:"size,omitempty"`
	N              *int   `json:"n,omitempty"`
	NegativePrompt string `json:"negative_prompt,omitempty"`
	PromptExtend   *bool  `json:"prompt_extend,omitempty"`
	Watermark      *bool  `json:"watermark,omitempty"`
	Seed           *int64 `json:"seed,omitempty"`
}

type imageTaskSubmitResponse struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"output"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type imageTaskResponse struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		Results    []struct {
			URL          string `json:"url"`
			OrigPrompt   string `json:"orig_prompt"`
			ActualPrompt string `json:"actual_prompt"`
		} `json:"results"`
		Choices []struct {
			Message struct {
				Content []struct {
					Image string `json:"image"`
					Type  string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"output"`
	Usage struct {
		ImageCount   int    `json:"image_count"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Size         string `json:"size"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}
