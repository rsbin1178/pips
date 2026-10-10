//nolint:wsl_v5 // Protocol option assembly stays linear and auditable.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

// ErrInvalid means the resolved model or credential store is invalid.
var ErrInvalid = errors.New("coding model: invalid configuration")

// New constructs a native or compatible provider adapter from one complete
// immutable resolved snapshot.
func New(
	ctx context.Context,
	resolved modelcatalog.ResolvedModel,
	store credential.Store,
) (ai.LanguageModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("%w: nil credential store", ErrInvalid)
	}
	if _, err := config.ParseModelRef(resolved.Ref.String()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	secret, err := store.Get(ctx, resolved.Ref.Provider)
	if err != nil {
		return nil, fmt.Errorf("coding model: credential for %s: %w", resolved.Ref.Provider, err)
	}

	switch resolved.Protocol {
	case config.ProtocolOpenAIResponses, config.ProtocolOpenAIChatCompletions:
		options := adapterOptions(resolved, []openai.Option{
			openai.WithAPIKey(secret.APIKey()),
			openai.WithProvider(resolved.Ref.Provider),
			openai.WithBaseURL(resolved.Endpoint.BaseURL),
			openai.WithAPI(openAIAPI(resolved.Protocol)),
			openai.WithCompatibility(resolved.Compatibility),
		}, openai.WithAllowHTTP, openai.WithAllowPrivateIPs, openai.WithStreamIdleTimeout)

		return withCodingModel(openai.New(resolved.Ref.Model, options...), resolved), nil
	case config.ProtocolAnthropicMessages:
		options := adapterOptions(resolved, []anthropic.Option{
			anthropic.WithAPIKey(secret.APIKey()),
			anthropic.WithProvider(resolved.Ref.Provider),
			anthropic.WithBaseURL(resolved.Endpoint.BaseURL),
		}, anthropic.WithAllowHTTP, anthropic.WithAllowPrivateIPs, anthropic.WithStreamIdleTimeout)

		return withCodingModel(anthropic.New(resolved.Ref.Model, options...), resolved), nil
	case config.ProtocolGeminiGenerateContent:
		options := adapterOptions(resolved, []gemini.Option{
			gemini.WithAPIKey(secret.APIKey()),
			gemini.WithProvider(resolved.Ref.Provider),
			gemini.WithBaseURL(resolved.Endpoint.BaseURL),
		}, gemini.WithAllowHTTP, gemini.WithAllowPrivateIPs, gemini.WithStreamIdleTimeout)

		return withCodingModel(gemini.New(resolved.Ref.Model, options...), resolved), nil
	default:
		return nil, fmt.Errorf("%w: unsupported protocol %q", ErrInvalid, resolved.Protocol)
	}
}

// adapterOptions completes one adapter's option list with the endpoint and
// transport settings every provider package shares. Each package has its own
// option type, so the three setters are passed in rather than the list being
// rebuilt per branch — which keeps a new endpoint or transport knob from being
// added to two protocols and forgotten in the third.
func adapterOptions[T any](
	resolved modelcatalog.ResolvedModel,
	base []T,
	allowHTTP func() T,
	allowPrivateIPs func() T,
	streamIdleTimeout func(time.Duration) T,
) []T {
	options := slices.Clone(base)
	if resolved.Endpoint.AllowHTTP {
		options = append(options, allowHTTP())
	}
	if resolved.Endpoint.AllowPrivateIPs {
		options = append(options, allowPrivateIPs())
	}
	if resolved.StreamIdleTimeout > 0 {
		options = append(options, streamIdleTimeout(resolved.StreamIdleTimeout))
	}

	return options
}

func openAIAPI(protocol config.Protocol) openai.API {
	if protocol == config.ProtocolOpenAIResponses {
		return openai.APIResponses
	}

	return openai.APIChatCompletions
}
