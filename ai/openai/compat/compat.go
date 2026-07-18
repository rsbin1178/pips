// Package compat provides service profiles for providers that expose an
// OpenAI-shaped Chat Completions or Responses API.
//
// Profiles reuse the protocol implementation in package openai while keeping
// provider identity, credentials, endpoints, capabilities, and documented wire
// differences explicit. Native protocols such as Bedrock Converse and Vertex
// AI are not compatibility profiles.
package compat

import (
	"os"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

// Profile describes one OpenAI-shaped service. Named constructors in this
// package provide reviewed profiles; New also accepts custom profiles for
// proxies and self-hosted endpoints.
type Profile struct {
	Provider      ai.Provider
	BaseURL       string
	APIKeyEnv     []string
	API           openai.API
	Compatibility openai.Compatibility
	Capabilities  ai.Capabilities

	capabilitiesFor func(string) ai.Capabilities
}

// New returns an OpenAI-protocol model configured from profile. Profile
// defaults are applied before opts, so callers may override any transport or
// protocol setting. An empty provider key is intentional and never falls back
// to OPENAI_API_KEY.
func New(profile Profile, model string, opts ...openai.Option) *openai.Model {
	api := profile.API
	if api == "" {
		api = openai.APIChatCompletions
	}

	capabilities := profile.Capabilities
	if profile.capabilitiesFor != nil {
		capabilities = profile.capabilitiesFor(model)
	}

	if capabilities == (ai.Capabilities{}) {
		capabilities = ai.Capabilities{Text: true, Tools: true}
	}

	defaults := []openai.Option{
		openai.WithProvider(profile.Provider),
		openai.WithBaseURL(profile.BaseURL),
		openai.WithAPIKey(firstEnv(profile.APIKeyEnv...)),
		openai.WithAPI(api),
		openai.WithCompatibility(profile.Compatibility),
		openai.WithCapabilities(capabilities),
	}

	return openai.New(model, append(defaults, opts...)...)
}

// DeepSeek returns a model using DeepSeek's Chat Completions endpoint.
func DeepSeek(model string, opts ...openai.Option) *openai.Model {
	return New(deepSeekProfile, model, opts...)
}

// Groq returns a model using Groq's OpenAI-compatible endpoint.
func Groq(model string, opts ...openai.Option) *openai.Model {
	return New(groqProfile, model, opts...)
}

// XAI returns a model using xAI's Responses endpoint, the service's
// recommended API for reasoning and agentic use cases.
func XAI(model string, opts ...openai.Option) *openai.Model {
	return New(xaiProfile, model, opts...)
}

// OpenRouter returns a model routed through OpenRouter's Chat Completions
// endpoint. Capabilities depend on the selected upstream model and are
// therefore reported conservatively.
func OpenRouter(model string, opts ...openai.Option) *openai.Model {
	return New(openRouterProfile, model, opts...)
}

// Cerebras returns a model using the Cerebras Inference Chat Completions
// endpoint.
func Cerebras(model string, opts ...openai.Option) *openai.Model {
	return New(cerebrasProfile, model, opts...)
}

// Together returns a model using Together AI's Chat Completions endpoint.
func Together(model string, opts ...openai.Option) *openai.Model {
	return New(togetherProfile, model, opts...)
}

// Mistral returns a model using Mistral's Chat Completions endpoint. Mistral's
// stateful Conversations API is a separate native protocol.
func Mistral(model string, opts ...openai.Option) *openai.Model {
	return New(mistralProfile, model, opts...)
}

var deepSeekProfile = Profile{
	Provider:  ai.ProviderDeepSeek,
	BaseURL:   "https://api.deepseek.com",
	APIKeyEnv: []string{"DEEPSEEK_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		StructuredOutput: openai.StructuredOutputJSONObject,
		ChatReasoning:    openai.ChatReasoningDeepSeek,
		ReasoningHistory: openai.ReasoningHistoryContent,
	},
	Capabilities: ai.Capabilities{
		Text:             true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
	},
}

var groqProfile = Profile{
	Provider:        ai.ProviderGroq,
	BaseURL:         "https://api.groq.com/openai/v1",
	APIKeyEnv:       []string{"GROQ_API_KEY"},
	API:             openai.APIChatCompletions,
	capabilitiesFor: groqCapabilities,
}

var xaiProfile = Profile{
	Provider:  ai.ProviderXAI,
	BaseURL:   "https://api.x.ai/v1",
	APIKeyEnv: []string{"XAI_API_KEY"},
	API:       openai.APIResponses,
	Compatibility: openai.Compatibility{
		IncludeEncryptedReasoning: true,
	},
	Capabilities: ai.Capabilities{
		Text:             true,
		Vision:           true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
	},
}

var openRouterProfile = Profile{
	Provider:  ai.ProviderOpenRouter,
	BaseURL:   "https://openrouter.ai/api/v1",
	APIKeyEnv: []string{"OPENROUTER_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ChatReasoning:    openai.ChatReasoningObject,
		ReasoningHistory: openai.ReasoningHistoryReasoning,
	},
	Capabilities: ai.Capabilities{Text: true, Tools: true},
}

var cerebrasProfile = Profile{
	Provider:        ai.ProviderCerebras,
	BaseURL:         "https://api.cerebras.ai/v1",
	APIKeyEnv:       []string{"CEREBRAS_API_KEY"},
	API:             openai.APIChatCompletions,
	capabilitiesFor: cerebrasCapabilities,
}

var togetherProfile = Profile{
	Provider:  ai.ProviderTogether,
	BaseURL:   "https://api.together.xyz/v1",
	APIKeyEnv: []string{"TOGETHER_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ChatReasoning:    openai.ChatReasoningOmit,
		ReasoningHistory: openai.ReasoningHistoryReasoning,
	},
	Capabilities: ai.Capabilities{Text: true, Tools: true},
}

var mistralProfile = Profile{
	Provider:  ai.ProviderMistral,
	BaseURL:   "https://api.mistral.ai/v1",
	APIKeyEnv: []string{"MISTRAL_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ReasoningHistory: openai.ReasoningHistoryReasoning,
	},
	capabilitiesFor: mistralCapabilities,
}

func groqCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if strings.Contains(model, "vision") || strings.Contains(model, "scout") {
		caps.Vision = true
	}

	if strings.Contains(model, "qwen") || strings.Contains(model, "deepseek") || strings.Contains(model, "gpt-oss") {
		caps.Reasoning = true
	}

	return caps
}

func cerebrasCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if strings.Contains(model, "gpt-oss") || strings.Contains(model, "qwen") {
		caps.Reasoning = true
	}

	if strings.Contains(model, "gemma") {
		caps.Vision = true
	}

	return caps
}

func mistralCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}

	if strings.Contains(model, "pixtral") || strings.Contains(model, "vision") {
		caps.Vision = true
	}

	if strings.Contains(model, "magistral") || strings.Contains(model, "reasoning") {
		caps.Reasoning = true
	}

	return caps
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return ""
}
