package generation_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/generation"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileAppliesFillOnlyDefaults(t *testing.T) {
	t.Parallel()

	maximum := 8192
	explicitMaximum := 2048
	defaultTemperature := 0.2
	explicitTemperature := 0.7
	level := config.ReasoningLevel("high")
	policy, err := generation.Compile(modelcatalog.ResolvedModel{
		Ref:            config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Protocol:       config.ProtocolOpenAIChatCompletions,
		ReasoningLevel: &level,
		Options: config.ModelOptions{
			MaxOutputTokens: &maximum,
			Temperature:     &defaultTemperature,
			ExtraBody:       map[string]any{"service_tier": "flex"},
		},
	})
	require.NoError(t, err)

	explicitProviderOptions := map[ai.Provider]any{
		ai.ProviderOpenAI: openai.RequestOptions{
			ExtraFields: map[string]any{"user": "caller"},
		},
	}
	request := ai.Request{
		MaxTokens:       &explicitMaximum,
		Temperature:     &explicitTemperature,
		ProviderOptions: explicitProviderOptions,
	}
	policy(&request)
	assert.InDelta(t, explicitTemperature, *request.Temperature, 1e-9)
	assert.Equal(t, explicitMaximum, *request.MaxTokens)
	assert.Equal(t, ai.ReasoningHigh, request.Reasoning.Effort)
	resolvedOptions, ok := request.ProviderOptions[ai.ProviderOpenAI].(openai.RequestOptions)
	require.True(t, ok)
	assert.Equal(t, "caller", resolvedOptions.ExtraFields["user"])
	assert.Equal(t, "flex", resolvedOptions.ExtraFields["service_tier"])

	originalOptions, ok := explicitProviderOptions[ai.ProviderOpenAI].(openai.RequestOptions)
	require.True(t, ok)
	assert.NotContains(t, originalOptions.ExtraFields, "service_tier")

	defaulted := ai.Request{}
	policy(&defaulted)
	assert.Equal(t, maximum, *defaulted.MaxTokens)
}

func TestCompileRejectsUnsupportedAndReservedOptions(t *testing.T) {
	t.Parallel()

	seed := int64(7)
	_, err := generation.Compile(modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Protocol: config.ProtocolOpenAIResponses,
		Options:  config.ModelOptions{Seed: &seed},
	})
	require.ErrorIs(t, err, generation.ErrInvalid)

	_, err = generation.Compile(modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Protocol: config.ProtocolOpenAIResponses,
		Options:  config.ModelOptions{ExtraBody: map[string]any{"model": "override"}},
	})
	require.ErrorIs(t, err, generation.ErrInvalid)
}

func TestCompileRejectsReasoningThatWouldBeDropped(t *testing.T) {
	t.Parallel()

	adaptive := config.ReasoningAdaptive
	disabled := config.ReasoningDisabled
	enabled := config.ReasoningEnabled
	budget := 4096
	include := true
	xhigh := config.ReasoningLevel(ai.ReasoningXHigh)
	minimal := config.ReasoningLevel(ai.ReasoningMinimal)
	high := config.ReasoningLevel(ai.ReasoningHigh)

	tests := []struct {
		name  string
		model modelcatalog.ResolvedModel
	}{
		{
			name: "enabled mode without a control",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Protocol: config.ProtocolOpenAIResponses, Options: config.ModelOptions{ReasoningMode: &enabled},
			},
		},
		{
			name: "responses adaptive mode",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Protocol: config.ProtocolOpenAIResponses, Options: config.ModelOptions{ReasoningMode: &adaptive},
			},
		},
		{
			name: "chat budget",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Protocol: config.ProtocolOpenAIChatCompletions, Options: config.ModelOptions{ReasoningBudget: &budget},
			},
		},
		{
			name: "chat include",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Protocol: config.ProtocolOpenAIChatCompletions, Options: config.ModelOptions{IncludeReasoning: &include},
			},
		},
		{
			name: "chat disabled mode",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Protocol: config.ProtocolOpenAIChatCompletions, Options: config.ModelOptions{ReasoningMode: &disabled},
			},
		},
		{
			name: "chat compatibility omits selected reasoning",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: "compatible", Model: "model"},
				Protocol: config.ProtocolOpenAIChatCompletions, ReasoningLevel: &high,
				Compatibility: openai.Compatibility{ChatReasoning: openai.ChatReasoningOmit},
			},
		},
		{
			name: "anthropic legacy mode cannot map xhigh",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"},
				Protocol: config.ProtocolAnthropicMessages, ReasoningLevel: &xhigh,
			},
		},
		{
			name: "anthropic adaptive mode rejects minimal",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderAnthropic, Model: "claude"},
				Protocol: config.ProtocolAnthropicMessages, ReasoningLevel: &minimal,
				Options: config.ModelOptions{ReasoningMode: &adaptive},
			},
		},
		{
			name: "gemini adaptive mode",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderGemini, Model: "gemini"},
				Protocol: config.ProtocolGeminiGenerateContent, Options: config.ModelOptions{ReasoningMode: &adaptive},
			},
		},
		{
			name: "gemini native level rejects xhigh",
			model: modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: ai.ProviderGemini, Model: "gemini"},
				Protocol: config.ProtocolGeminiGenerateContent, ReasoningLevel: &xhigh,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := generation.Compile(tt.model)
			require.ErrorIs(t, err, generation.ErrInvalid)
		})
	}
}

func TestCompileAllowsBudgetMappedGeminiReasoning(t *testing.T) {
	t.Parallel()

	xhigh := config.ReasoningLevel(ai.ReasoningXHigh)
	budget := 4096
	_, err := generation.Compile(modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderGemini, Model: "gemini"},
		Protocol: config.ProtocolGeminiGenerateContent, ReasoningLevel: &xhigh,
		Options: config.ModelOptions{ReasoningBudget: &budget},
	})
	require.NoError(t, err)
}

func TestCompileRejectsGeminiOutOfRangeControls(t *testing.T) {
	t.Parallel()

	seed := int64(1 << 31)
	_, err := generation.Compile(modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderGemini, Model: "gemini"},
		Protocol: config.ProtocolGeminiGenerateContent,
		Options:  config.ModelOptions{Seed: &seed},
	})
	require.ErrorIs(t, err, generation.ErrInvalid)
	require.ErrorContains(t, err, "provider gemini")
	require.ErrorContains(t, err, "model gemini")
	require.ErrorContains(t, err, "protocol gemini/generate_content")
	require.ErrorContains(t, err, "reasoning \"<provider-default>\"")
	require.ErrorContains(t, err, "request.seed")
}
