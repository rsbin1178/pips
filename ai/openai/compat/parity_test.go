package compat_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cerebras"
	"github.com/rsbin1178/pips/ai/deepseek"
	"github.com/rsbin1178/pips/ai/groq"
	"github.com/rsbin1178/pips/ai/kimi"
	"github.com/rsbin1178/pips/ai/minimax"
	"github.com/rsbin1178/pips/ai/mistral"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openai/compat"
	"github.com/rsbin1178/pips/ai/openrouter"
	"github.com/rsbin1178/pips/ai/qwen"
	"github.com/rsbin1178/pips/ai/siliconflow"
	"github.com/rsbin1178/pips/ai/together"
	"github.com/rsbin1178/pips/ai/xai"
	"github.com/rsbin1178/pips/ai/zhipu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReviewedProfileFacadeParity guards the structural invariant behind the
// vendor facades: a dedicated ai/<vendor> package and the reviewed compat
// profile for the same provider must agree on identity and static
// capabilities. The coding agent resolves built-in providers through
// compat.Lookup, so a divergence here would silently change what it reports
// for those models.
func TestReviewedProfileFacadeParity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider ai.Provider
		models   []string
		facade   func(string) *openai.Model
	}{
		{ai.ProviderDeepSeek, []string{"deepseek-flash", "deepseek-v4-pro"}, func(m string) *openai.Model { return deepseek.New(m) }},
		{ai.ProviderGroq, []string{"llama-3.3-70b-versatile", "qwen/qwen3.6-27b", "llama-3.2-11b-vision-preview", "openai/gpt-oss-120b"}, func(m string) *openai.Model { return groq.New(m) }},
		{ai.ProviderXAI, []string{"grok-2-1212", "grok-vision-beta"}, func(m string) *openai.Model { return xai.New(m) }},
		{ai.ProviderOpenRouter, []string{"openai/gpt-5.2", "anthropic/claude-sonnet-4.6"}, func(m string) *openai.Model { return openrouter.New(m) }},
		{ai.ProviderCerebras, []string{"llama3.1-8b", "gpt-oss-120b", "qwen-3-32b", "gemma-3-27b"}, func(m string) *openai.Model { return cerebras.New(m) }},
		{ai.ProviderTogether, []string{"meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo"}, func(m string) *openai.Model { return together.New(m) }},
		{ai.ProviderMistral, []string{"mistral-large-latest", "pixtral-12b-2409", "magistral-small-2506", "open-mistral-nemo", "codestral-2508"}, func(m string) *openai.Model { return mistral.New(m) }},
		{ai.ProviderZhipu, []string{"glm-4-plus", "glm-4v-plus", "glm-zero-preview"}, func(m string) *openai.Model { return zhipu.New(m) }},
		{ai.ProviderSiliconFlow, []string{"deepseek-ai/DeepSeek-V3.2"}, func(m string) *openai.Model { return siliconflow.New(m) }},
		{ai.ProviderKimi, []string{"kimi-k3", "kimi-k2.6", "moonshot-v1-8k", "moonshot-v1-8k-vision-preview"}, func(m string) *openai.Model { return kimi.New(m) }},
		{ai.ProviderQwen, []string{"qwen3.7-max", "qwen-plus", "qwen3-vl-plus", "qwen-vl-max", "qwq-32b"}, func(m string) *openai.Model { return qwen.New(m) }},
		{ai.ProviderMiniMax, []string{"MiniMax-M3", "MiniMax-M2.1", "abab6.5s-chat"}, func(m string) *openai.Model { return minimax.New(m) }},
	}

	for _, test := range tests {
		profile, ok := compat.Lookup(test.provider)
		require.True(t, ok, "provider %q must have a reviewed profile", test.provider)

		for _, model := range test.models {
			t.Run(string(test.provider)+"/"+model, func(t *testing.T) {
				t.Parallel()

				reviewed := compat.New(profile, model)
				facade := test.facade(model)

				assert.Equal(t, test.provider, reviewed.Provider())
				assert.Equal(t, reviewed.Provider(), facade.Provider())
				assert.Equal(t, reviewed.ModelID(), facade.ModelID())
				assert.Equal(t, reviewed.Capabilities(), facade.Capabilities())
				// Wire differences are part of the reviewed contract: a
				// divergence would silently change the fields the coding agent
				// sends for a built-in provider.
				assert.Equal(t, reviewed.Compatibility(), facade.Compatibility())
			})
		}
	}
}

// TestReviewedChatProfilesForwardReasoning pins the reasoning knob each
// Chat Completions profile encodes. Ability guesses must not turn into a
// withheld field: ChatReasoningOmit is reserved for a protocol that has no
// such field at all, and every reviewed chat profile has one
// (.trellis/spec/backend/provider-compatibility-policy.md R1).
func TestReviewedChatProfilesForwardReasoning(t *testing.T) {
	t.Parallel()

	// The knob each provider family actually reads (policy R4).
	want := map[ai.Provider]openai.ChatReasoningFormat{
		ai.ProviderDeepSeek:   openai.ChatReasoningDeepSeek,
		ai.ProviderOpenRouter: openai.ChatReasoningObject,
		ai.ProviderKimi:       openai.ChatReasoningEffort,
		ai.ProviderQwen:       openai.ChatReasoningEffort,
		ai.ProviderMiniMax:    openai.ChatReasoningEffort,
		ai.ProviderZhipu:      openai.ChatReasoningEffort,
	}

	for provider, format := range want {
		profile, ok := compat.Lookup(provider)
		require.True(t, ok, "provider %q must have a reviewed profile", provider)
		assert.Equal(t, format, profile.Compatibility.ChatReasoning, "%s must read its own knob", provider)
	}

	for _, provider := range compat.Providers() {
		profile, ok := compat.Lookup(provider)
		require.True(t, ok, "provider %q must have a reviewed profile", provider)

		if profile.API != openai.APIChatCompletions {
			continue
		}

		assert.NotEqual(
			t,
			openai.ChatReasoningOmit,
			profile.Compatibility.ChatReasoning,
			"%s is a Chat Completions profile; withholding a configured level is not its call (policy R1)",
			provider,
		)
	}
}
