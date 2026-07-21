//nolint:wsl_v5 // Protocol option assembly stays linear and auditable.
package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
	"github.com/rsbin/pips/ai/gemini"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
)

// ErrInvalid means the resolved model or credential store is invalid.
var ErrInvalid = errors.New("coding model: invalid configuration")

// New constructs a native or compatible provider adapter from one complete
// immutable resolved snapshot.
//
//nolint:gocyclo // Each protocol owns a distinct typed option family.
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

	switch resolved.API {
	case config.APIResponses, config.APIChatCompletions:
		options := []openai.Option{
			openai.WithAPIKey(secret.APIKey()),
			openai.WithProvider(resolved.Ref.Provider),
			openai.WithBaseURL(resolved.Endpoint.BaseURL),
			openai.WithAPI(openAIAPI(resolved.API)),
			openai.WithCompatibility(resolved.Compatibility),
		}
		if resolved.Endpoint.AllowHTTP {
			options = append(options, openai.WithAllowHTTP())
		}
		if resolved.Endpoint.AllowPrivateIPs {
			options = append(options, openai.WithAllowPrivateIPs())
		}

		return openai.New(resolved.Ref.Model, options...), nil
	case config.APIAnthropicMessages:
		options := []anthropic.Option{
			anthropic.WithAPIKey(secret.APIKey()),
			anthropic.WithProvider(resolved.Ref.Provider),
			anthropic.WithBaseURL(resolved.Endpoint.BaseURL),
		}
		if resolved.Endpoint.AllowHTTP {
			options = append(options, anthropic.WithAllowHTTP())
		}
		if resolved.Endpoint.AllowPrivateIPs {
			options = append(options, anthropic.WithAllowPrivateIPs())
		}

		return anthropic.New(resolved.Ref.Model, options...), nil
	case config.APIGenerateContent:
		options := []gemini.Option{
			gemini.WithAPIKey(secret.APIKey()),
			gemini.WithProvider(resolved.Ref.Provider),
			gemini.WithBaseURL(resolved.Endpoint.BaseURL),
		}
		if resolved.Endpoint.AllowHTTP {
			options = append(options, gemini.WithAllowHTTP())
		}
		if resolved.Endpoint.AllowPrivateIPs {
			options = append(options, gemini.WithAllowPrivateIPs())
		}

		return gemini.New(resolved.Ref.Model, options...), nil
	default:
		return nil, fmt.Errorf("%w: unsupported api %q", ErrInvalid, resolved.API)
	}
}

func openAIAPI(api config.API) openai.API {
	if api == config.APIResponses {
		return openai.APIResponses
	}

	return openai.APIChatCompletions
}
