package agnes

import (
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/apierr"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// ImageOptions is the agnes entry for the ProviderOptions of
// [ai.ImageRequest] and [ai.ImageEditRequest]:
//
//	req.ProviderOptions = map[ai.Provider]any{
//	    ai.ProviderAgnes: agnes.ImageOptions{Ratio: "16:9"},
//	}
type ImageOptions struct {
	// Ratio selects the output aspect ratio, one of "1:1", "3:4", "4:3",
	// "16:9", "9:16", "2:3", "3:2", or "21:9". Empty omits the field and
	// Agnes uses "1:1". It combines with the portable Size tier.
	Ratio string
	// ResponseFormat selects "url" or "b64_json" and is sent inside
	// extra_body, never at the top level.
	ResponseFormat string
	// ReturnBase64 asks a text-to-image request to return inline bytes instead
	// of a URL. Agnes documents the field for generation only, so edits do not
	// send it.
	ReturnBase64 bool
	// ExtraFields is merged into the top level of the outgoing request body
	// using bounded, add-only semantics. It is the escape hatch for vendor
	// parameters this adapter does not model; keys the typed fields own
	// (model, prompt, size, ratio, image, extra_body, return_base64,
	// response_format), credential-shaped keys, and oversized values are
	// rejected.
	ExtraFields map[string]any
}

// imageOptions extracts this provider's options from a ProviderOptions map:
// the ai.ProviderAgnes entry first, then the ai.ProviderOpenAI entry, which is
// the same fallback openai's own options lookup performs. Both keys must hold
// an agnes.ImageOptions value; a value of any other type is ignored, so an
// openai.ImageOptions entry under either key has no effect.
func imageOptions(options map[ai.Provider]any) ImageOptions {
	if raw, ok := options[ai.ProviderAgnes]; ok {
		if opts, ok := raw.(ImageOptions); ok {
			return opts
		}
	}

	if raw, ok := options[ai.ProviderOpenAI]; ok {
		if opts, ok := raw.(ImageOptions); ok {
			return opts
		}
	}

	return ImageOptions{}
}

// imageReservedFields are the request keys this adapter's typed fields own.
// The list is per request family: it also reserves extra_body, whose members
// the typed fields own, so ExtraFields cannot smuggle a second image list or
// response_format past the documented encoding. The top-level response_format
// is reserved as well: the vendor names that position its most common
// integration error, and [ImageOptions.ResponseFormat] already reaches the
// documented extra_body member.
var imageReservedFields = []string{
	"model", "prompt", "size", "ratio", "image", "extra_body", "return_base64",
	"response_format",
}

// mergeImageExtraFields folds opts.ExtraFields into an already-encoded JSON
// request body. A no-op when there are no extras.
func mergeImageExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra, imageReservedFields...)
}

// decodeError builds the httpx.ErrorDecoder for this provider. The Agnes error
// envelope is OpenAI-shaped, so the shared decoder applies unchanged.
func decodeError(status int, retryAfter time.Duration, body []byte) error {
	return apierr.Decode(ai.ProviderAgnes, status, retryAfter, body)
}
