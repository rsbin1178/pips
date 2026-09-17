// Package compat provides service profiles for providers that expose an
// OpenAI-shaped Chat Completions or Responses API.
//
// Profiles reuse the protocol implementation in package openai while keeping
// provider identity, credentials, endpoints, capabilities, and documented wire
// differences explicit. Native protocols such as Bedrock Converse and Vertex
// AI are not compatibility profiles.
//
// This package keeps two roles. [Lookup] and [Providers] are the reviewed
// registry that applications use to build a built-in provider list without
// duplicating endpoint or wire knowledge. The named constructors
// ([DeepSeek], [Groq], [XAI], [Cerebras], [Together], [Mistral], [Zhipu],
// [SiliconFlow], [Kimi], [Qwen], [MiniMax] and their embedding variants) are
// backward-compatible
// convenience wrappers; new code should prefer the dedicated ai/<vendor>
// packages, which are the canonical vendor facades.
//
//nolint:wsl_v5 // Profile lookup and defensive copying form one operation.
package compat

import (
	"os"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// reviewedProfiles is the single reviewed provider registry. Lookup and
// Providers both read it, so an application that ranges over Providers cannot
// drift from the profiles Lookup resolves.
var reviewedProfiles = map[ai.Provider]Profile{
	ai.ProviderDeepSeek:    deepSeekProfile,
	ai.ProviderGroq:        groqProfile,
	ai.ProviderXAI:         xaiProfile,
	ai.ProviderOpenRouter:  openRouterProfile,
	ai.ProviderCerebras:    cerebrasProfile,
	ai.ProviderTogether:    togetherProfile,
	ai.ProviderMistral:     mistralProfile,
	ai.ProviderZhipu:       zhipuProfile,
	ai.ProviderSiliconFlow: siliconFlowProfile,
	ai.ProviderKimi:        kimiProfile,
	ai.ProviderQwen:        qwenProfile,
	ai.ProviderMiniMax:     miniMaxProfile,
}

// Providers returns the reviewed OpenAI-compatible provider identifiers in
// sorted order. It lets applications build a built-in provider registry by
// derivation instead of duplicating and maintaining the reviewed set.
func Providers() []ai.Provider {
	providers := make([]ai.Provider, 0, len(reviewedProfiles))
	for provider := range reviewedProfiles {
		providers = append(providers, provider)
	}
	slices.Sort(providers)

	return providers
}

// Lookup returns a defensive copy of a reviewed OpenAI-compatible provider
// profile. It lets applications build registries without duplicating endpoint
// and wire-compatibility knowledge.
func Lookup(provider ai.Provider) (Profile, bool) {
	profile, ok := reviewedProfiles[provider]
	if !ok {
		return Profile{}, false
	}
	profile.APIKeyEnv = slices.Clone(profile.APIKeyEnv)

	return profile, true
}

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
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/deepseek.New] instead.
func DeepSeek(model string, opts ...openai.Option) *openai.Model {
	return New(deepSeekProfile, model, opts...)
}

// Groq returns a model using Groq's OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/groq.New] instead.
func Groq(model string, opts ...openai.Option) *openai.Model {
	return New(groqProfile, model, opts...)
}

// XAI returns a model using xAI's Responses endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/xai.New] instead.
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
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/cerebras.New] instead.
func Cerebras(model string, opts ...openai.Option) *openai.Model {
	return New(cerebrasProfile, model, opts...)
}

// Together returns a model using Together AI's Chat Completions endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/together.New] instead.
func Together(model string, opts ...openai.Option) *openai.Model {
	return New(togetherProfile, model, opts...)
}

// Mistral returns a model using Mistral's Chat Completions endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/mistral.New] instead.
func Mistral(model string, opts ...openai.Option) *openai.Model {
	return New(mistralProfile, model, opts...)
}

// Zhipu returns a model using Zhipu AI's Chat Completions endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/zhipu.New] instead.
func Zhipu(model string, opts ...openai.Option) *openai.Model {
	return New(zhipuProfile, model, opts...)
}

// SiliconFlow returns a model using SiliconFlow's OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/siliconflow.New] instead.
func SiliconFlow(model string, opts ...openai.Option) *openai.Model {
	return New(siliconFlowProfile, model, opts...)
}

// Kimi returns a model using Moonshot AI's OpenAI-compatible Chat Completions
// endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/kimi.New] instead.
func Kimi(model string, opts ...openai.Option) *openai.Model {
	return New(kimiProfile, model, opts...)
}

// Qwen returns a model using Alibaba Cloud DashScope's OpenAI-compatible Chat
// Completions endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/qwen.New] instead.
func Qwen(model string, opts ...openai.Option) *openai.Model {
	return New(qwenProfile, model, opts...)
}

// MiniMax returns a model using MiniMax's OpenAI-compatible Chat Completions
// endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/minimax.New] instead.
func MiniMax(model string, opts ...openai.Option) *openai.Model {
	return New(miniMaxProfile, model, opts...)
}

// Embedding returns an OpenAI-protocol embedding model configured from profile.
// Profile defaults (provider identity, base URL, and credentials) are applied
// before opts, so callers may override any setting. An empty provider key is
// intentional and never falls back to OPENAI_API_KEY.
func Embedding(profile Profile, model string, opts ...openai.Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(profile.Provider),
		openai.WithBaseURL(profile.BaseURL),
		openai.WithAPIKey(firstEnv(profile.APIKeyEnv...)),
	}

	return openai.NewEmbeddingModel(model, append(defaults, opts...)...)
}

// TogetherEmbedding returns an embedding model using Together AI's
// OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/together.NewEmbeddingModel] instead.
func TogetherEmbedding(model string, opts ...openai.Option) *openai.EmbeddingModel {
	return Embedding(togetherProfile, model, opts...)
}

// MistralEmbedding returns an embedding model using Mistral AI's
// OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/mistral.NewEmbeddingModel] instead.
func MistralEmbedding(model string, opts ...openai.Option) *openai.EmbeddingModel {
	return Embedding(mistralProfile, model, opts...)
}

// ZhipuEmbedding returns an embedding model using Zhipu AI's
// OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/zhipu.NewEmbeddingModel] instead.
func ZhipuEmbedding(model string, opts ...openai.Option) *openai.EmbeddingModel {
	return Embedding(zhipuProfile, model, opts...)
}

// SiliconFlowEmbedding returns an embedding model using SiliconFlow's
// OpenAI-compatible endpoint.
//
// Deprecated: prefer dedicated [github.com/rsbin1178/pips/ai/siliconflow.NewEmbeddingModel] instead.
func SiliconFlowEmbedding(model string, opts ...openai.Option) *openai.EmbeddingModel {
	return Embedding(siliconFlowProfile, model, opts...)
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
		BuiltinTools:     openai.BuiltinToolsStrip,
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
	Provider:  ai.ProviderGroq,
	BaseURL:   "https://api.groq.com/openai/v1",
	APIKeyEnv: []string{"GROQ_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		ReasoningHistory: openai.ReasoningHistoryReasoning,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: groqCapabilities,
}

var xaiProfile = Profile{
	Provider:  ai.ProviderXAI,
	BaseURL:   "https://api.x.ai/v1",
	APIKeyEnv: []string{"XAI_API_KEY"},
	API:       openai.APIResponses,
	Compatibility: openai.Compatibility{
		MaxTokensField:            openai.MaxTokensFieldLegacy,
		IncludeEncryptedReasoning: true,
	},
	Capabilities: ai.Capabilities{
		Text:             true,
		Vision:           true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
		WebSearch:        true,
		CodeExecution:    true,
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
		ReasoningHistory: openai.ReasoningHistoryDetails,
	},
	Capabilities: ai.Capabilities{Text: true, Tools: true},
}

var cerebrasProfile = Profile{
	Provider:  ai.ProviderCerebras,
	BaseURL:   "https://api.cerebras.ai/v1",
	APIKeyEnv: []string{"CEREBRAS_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		ReasoningHistory: openai.ReasoningHistoryReasoning,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: cerebrasCapabilities,
}

var togetherProfile = Profile{
	Provider:  ai.ProviderTogether,
	BaseURL:   "https://api.together.ai/v1",
	APIKeyEnv: []string{"TOGETHER_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ReasoningHistory: openai.ReasoningHistoryReasoning,
		BuiltinTools:     openai.BuiltinToolsStrip,
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
		ReasoningHistory: openai.ReasoningHistoryContentChunks,
		BuiltinTools:     openai.BuiltinToolsStrip,
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

var zhipuProfile = Profile{
	Provider:  ai.ProviderZhipu,
	BaseURL:   "https://open.bigmodel.cn/api/paas/v4",
	APIKeyEnv: []string{"ZHIPU_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		StructuredOutput: openai.StructuredOutputJSONObject,
		ReasoningHistory: openai.ReasoningHistoryContent,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: zhipuCapabilities,
}

func zhipuCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{
		Text:             true,
		Tools:            true,
		StructuredOutput: true,
	}

	if strings.Contains(model, "vision") || strings.Contains(model, "v") {
		caps.Vision = true
	}

	if strings.Contains(model, "zero") || strings.Contains(model, "reasoning") {
		caps.Reasoning = true
	}

	return caps
}

var siliconFlowProfile = Profile{
	Provider:  ai.ProviderSiliconFlow,
	BaseURL:   "https://api.siliconflow.cn/v1",
	APIKeyEnv: []string{"SILICONFLOW_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		// SiliconFlow documents max_tokens only; max_completion_tokens is not
		// part of its Chat Completions schema.
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ReasoningHistory: openai.ReasoningHistoryContent,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	Capabilities: ai.Capabilities{Text: true, Tools: true},
}

var kimiProfile = Profile{
	Provider:  ai.ProviderKimi,
	BaseURL:   "https://api.moonshot.ai/v1",
	APIKeyEnv: []string{"MOONSHOT_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		// The reviewed profile cannot vary by model, so it declines reasoning
		// controls outright rather than sending reasoning_effort to the k2.x
		// families that do not accept it. Kimi thinking is configured through
		// request.extra_body instead.
		ChatReasoning:    openai.ChatReasoningOmit,
		ReasoningHistory: openai.ReasoningHistoryContent,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: kimiCapabilities,
}

// kimiCapabilities mirrors ai/kimi.capabilitiesFor; the parity test keeps the
// reviewed profile and the vendor facade in sync.
func kimiCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true, StructuredOutput: true, PromptCaching: true}

	if containsAny(model, "vision", "kimi-k3", "kimi-k2.6") {
		caps.Vision = true
	}

	if containsAny(model, "kimi-k3", "kimi-k2.7", "kimi-k2.6", "kimi-k2.5", "thinking", "reasoning") {
		caps.Reasoning = true
	}

	return caps
}

var qwenProfile = Profile{
	Provider:  ai.ProviderQwen,
	BaseURL:   "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
	APIKeyEnv: []string{"DASHSCOPE_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		MaxTokensField:   openai.MaxTokensFieldLegacy,
		ChatReasoning:    openai.ChatReasoningOmit,
		ReasoningHistory: openai.ReasoningHistoryContent,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: qwenCapabilities,
}

// qwenCapabilities mirrors ai/qwen.capabilitiesFor; the parity test keeps the
// reviewed profile and the vendor facade in sync.
func qwenCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if containsAny(model, "vl", "omni", "qvq") {
		caps.Vision = true
	}

	if containsAny(model, "qwq", "thinking", "qwen3") {
		caps.Reasoning = true
	}

	return caps
}

var miniMaxProfile = Profile{
	Provider:  ai.ProviderMiniMax,
	BaseURL:   "https://api.minimax.io/v1",
	APIKeyEnv: []string{"MINIMAX_API_KEY"},
	API:       openai.APIChatCompletions,
	Compatibility: openai.Compatibility{
		ChatReasoning:    openai.ChatReasoningOmit,
		ReasoningHistory: openai.ReasoningHistoryContent,
		BuiltinTools:     openai.BuiltinToolsStrip,
	},
	capabilitiesFor: miniMaxCapabilities,
}

// miniMaxCapabilities mirrors ai/minimax.capabilitiesFor; the parity test keeps
// the reviewed profile and the vendor facade in sync.
func miniMaxCapabilities(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if containsAnyFold(model, "m3", "m2", "vision", "vl") {
		caps.Vision = true
	}

	if containsAnyFold(model, "m3") {
		caps.VideoInput = true
	}

	if containsAnyFold(model, "m3", "m2") {
		caps.Reasoning = true
	}

	return caps
}

func containsAny(model string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(model, needle) {
			return true
		}
	}

	return false
}

func containsAnyFold(model string, needles ...string) bool {
	lower := strings.ToLower(model)
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}

	return false
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return ""
}
