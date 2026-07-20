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
)

// ErrInvalid means the model configuration or credential store is invalid.
var ErrInvalid = errors.New("coding model: invalid configuration")

// New constructs one of the existing native provider models.
func New(
	ctx context.Context,
	modelConfig config.ModelConfig,
	store credential.Store,
) (ai.LanguageModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if store == nil {
		return nil, fmt.Errorf("%w: nil credential store", ErrInvalid)
	}

	probe := config.Config{
		Model:    modelConfig,
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	}
	if err := probe.ValidateRuntime(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	secret, err := store.Get(ctx, modelConfig.Provider)
	if err != nil {
		return nil, fmt.Errorf("coding model: credential for %s: %w", modelConfig.Provider, err)
	}

	switch modelConfig.Provider {
	case ai.ProviderOpenAI:
		return openai.New(
			modelConfig.ID,
			openai.WithAPIKey(secret.APIKey()),
			openai.WithAPI(modelConfig.API),
		), nil
	case ai.ProviderAnthropic:
		return anthropic.New(modelConfig.ID, anthropic.WithAPIKey(secret.APIKey())), nil
	case ai.ProviderGemini:
		return gemini.New(modelConfig.ID, gemini.WithAPIKey(secret.APIKey())), nil
	default:
		return nil, fmt.Errorf("%w: unsupported provider %q", ErrInvalid, modelConfig.Provider)
	}
}
