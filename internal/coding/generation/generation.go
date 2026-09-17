// Package generation compiles resolved model defaults into a fill-only Agent
// request policy. Explicit per-call request fields always win.
//
//nolint:wsl_v5 // Fill-only request assembly keeps related assignments adjacent.
package generation

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

// ErrInvalid means resolved defaults cannot be represented by the target protocol.
var ErrInvalid = errors.New("coding generation: invalid request defaults")

// Policy applies immutable fill-only defaults to a request.
type Policy func(*ai.Request)

// Compile validates and compiles one resolved snapshot. It performs no I/O.
func Compile(model modelcatalog.ResolvedModel) (Policy, error) {
	options := model.Options.Clone()
	if err := validate(model, options); err != nil {
		return nil, fmt.Errorf(
			"%w: provider %s model %s protocol %s variant %q reasoning %q: %w",
			ErrInvalid,
			model.Ref.Provider,
			model.Ref.Model,
			model.Protocol,
			model.Variant,
			reasoningLabel(model.ReasoningLevel),
			err,
		)
	}

	return func(request *ai.Request) {
		if request == nil {
			return
		}
		applyPortable(request, model, options)
		applyProviderOptions(request, model, options)
	}, nil
}

//nolint:gocyclo // The protocol matrix is kept in one preflight boundary.
func validate(model modelcatalog.ResolvedModel, options config.ModelOptions) error {
	if err := validateReasoningShape(model, options); err != nil {
		return err
	}
	if options.TopLogProbs != nil && options.LogProbs == nil {
		return errors.New("request.top_logprobs requires request.logprobs")
	}
	if options.TopLogProbs != nil && (*options.TopLogProbs < 0 || *options.TopLogProbs > 20) {
		return errors.New("request.top_logprobs is outside 0..20")
	}
	if options.LogProbs != nil && !*options.LogProbs && options.TopLogProbs != nil {
		return errors.New("request.top_logprobs requires request.logprobs=true")
	}

	switch model.Protocol {
	case config.ProtocolOpenAIResponses:
		if path := firstPresent([]optionPresence{
			{"request.top_k", options.TopK != nil},
			{"request.seed", options.Seed != nil},
			{"request.frequency_penalty", options.FrequencyPenalty != nil},
			{"request.presence_penalty", options.PresencePenalty != nil},
			{"request.min_p", options.MinP != nil},
			{"request.repetition_penalty", options.RepetitionPenalty != nil},
			{"request.stop", options.Stop != nil},
			{"request.reasoning_budget", options.ReasoningBudget != nil},
		}); path != "" {
			return fmt.Errorf("%s is unsupported by Responses", path)
		}
		if options.ReasoningMode != nil && *options.ReasoningMode == config.ReasoningAdaptive {
			return errors.New("request.reasoning_mode=adaptive is unsupported by Responses")
		}
		if options.LogProbs != nil && !*options.LogProbs {
			return errors.New("request.logprobs=false is unsupported by Responses")
		}
	case config.ProtocolOpenAIChatCompletions:
		if model.Ref.Provider == ai.ProviderOpenAI && options.TopK != nil {
			return errors.New("request.top_k is unsupported by OpenAI Chat Completions")
		}
		if options.ReasoningBudget != nil {
			return errors.New("request.reasoning_budget is unsupported by Chat Completions")
		}
		if options.IncludeReasoning != nil {
			return errors.New("request.include_reasoning is unsupported by Chat Completions")
		}
		if options.ReasoningMode != nil &&
			(*options.ReasoningMode == config.ReasoningAdaptive ||
				*options.ReasoningMode == config.ReasoningDisabled) {
			return errors.New("request.reasoning_mode is unsupported by Chat Completions")
		}
		// A reasoning selection is never rejected here merely because a
		// provider profile declines to encode it. Ability differences are
		// forwarded and left to the provider; see
		// .trellis/spec/backend/provider-compatibility-policy.md (R1).
	case config.ProtocolAnthropicMessages:
		if path := firstPresent([]optionPresence{
			{"request.seed", options.Seed != nil},
			{"request.frequency_penalty", options.FrequencyPenalty != nil},
			{"request.presence_penalty", options.PresencePenalty != nil},
			{"request.min_p", options.MinP != nil},
			{"request.repetition_penalty", options.RepetitionPenalty != nil},
			{"request.logprobs", options.LogProbs != nil},
		}); path != "" {
			return fmt.Errorf("%s is unsupported by Anthropic Messages", path)
		}
		if options.ReasoningMode != nil && *options.ReasoningMode == config.ReasoningAdaptive &&
			options.ReasoningBudget != nil {
			return errors.New("request.reasoning_mode=adaptive conflicts with request.reasoning_budget")
		}
		if options.ReasoningMode != nil && *options.ReasoningMode == config.ReasoningAdaptive &&
			!anthropicAdaptiveEffort(model.ReasoningLevel) {
			return fmt.Errorf(
				"reasoning %q is unsupported by Anthropic adaptive thinking",
				reasoningLabel(model.ReasoningLevel),
			)
		}
		if legacyAnthropicEffortUnsupported(model, options) {
			return errors.New("reasoning selection requires request.reasoning_mode=adaptive or request.reasoning_budget")
		}
	case config.ProtocolGeminiGenerateContent:
		if options.MinP != nil {
			return errors.New("request.min_p is unsupported by Gemini GenerateContent")
		}
		if options.RepetitionPenalty != nil {
			return errors.New("request.repetition_penalty is unsupported by Gemini GenerateContent")
		}
		if options.ReasoningMode != nil && *options.ReasoningMode == config.ReasoningAdaptive {
			return errors.New("request.reasoning_mode=adaptive is unsupported by Gemini GenerateContent")
		}
		if !geminiThinkingLevel(
			model.ReasoningLevel,
			options.ReasoningMode,
			options.ReasoningBudget,
		) {
			return fmt.Errorf(
				"reasoning %q is unsupported by Gemini thinkingLevel",
				reasoningLabel(model.ReasoningLevel),
			)
		}
		if err := validateGeminiRanges(options); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown protocol %q", model.Protocol)
	}

	if err := validateExtra(model.Protocol, options.ExtraBody); err != nil {
		return fmt.Errorf("request.extra_body: %w", err)
	}

	return nil
}

func anthropicAdaptiveEffort(level *config.ReasoningLevel) bool {
	if level == nil {
		return true
	}

	switch ai.ReasoningEffort(*level) {
	case ai.ReasoningNone, ai.ReasoningLow, ai.ReasoningMedium,
		ai.ReasoningHigh, ai.ReasoningXHigh, ai.ReasoningMax:
		return true
	default:
		return false
	}
}

func geminiThinkingLevel(
	level *config.ReasoningLevel,
	mode *config.ReasoningMode,
	budget *int,
) bool {
	if level == nil || budget != nil || mode != nil && *mode == config.ReasoningDisabled {
		return true
	}

	switch ai.ReasoningEffort(*level) {
	case ai.ReasoningNone, ai.ReasoningMinimal, ai.ReasoningLow,
		ai.ReasoningMedium, ai.ReasoningHigh:
		return true
	default:
		return false
	}
}

func validateReasoningShape(
	model modelcatalog.ResolvedModel,
	options config.ModelOptions,
) error {
	if options.ReasoningMode == nil {
		return nil
	}
	if *options.ReasoningMode == config.ReasoningDisabled {
		if options.ReasoningBudget != nil {
			return errors.New("request.reasoning_mode=disabled conflicts with request.reasoning_budget")
		}
		if options.IncludeReasoning != nil && *options.IncludeReasoning {
			return errors.New("request.reasoning_mode=disabled conflicts with request.include_reasoning=true")
		}

		return nil
	}
	if *options.ReasoningMode != config.ReasoningEnabled ||
		model.ReasoningLevel != nil || options.ReasoningBudget != nil {
		return nil
	}
	if model.Protocol == config.ProtocolOpenAIChatCompletions &&
		model.Compatibility.ChatReasoning == openai.ChatReasoningDeepSeek {
		return nil
	}

	return errors.New("request.reasoning_mode=enabled requires a reasoning selection or budget")
}

type optionPresence struct {
	path    string
	present bool
}

func firstPresent(values []optionPresence) string {
	for _, value := range values {
		if value.present {
			return value.path
		}
	}

	return ""
}

func reasoningLabel(value *config.ReasoningLevel) string {
	if value == nil {
		return "<provider-default>"
	}

	return string(*value)
}

func validateGeminiRanges(options config.ModelOptions) error {
	rules := []struct {
		invalid bool
		message string
	}{
		{outside(options.Temperature, 0, 2), "request.temperature is outside the Gemini 0..2 range"},
		{outside(options.TopP, 0, 1), "request.top_p is outside the Gemini 0..1 range"},
		{outsidePositiveInt32(options.TopK), "request.top_k is outside the Gemini 1..int32 range"},
		{outsideSignedInt32(options.Seed), "request.seed exceeds the Gemini int32 range"},
		{outside(options.FrequencyPenalty, -2, 2), "request.frequency_penalty is outside the Gemini -2..2 range"},
		{outside(options.PresencePenalty, -2, 2), "request.presence_penalty is outside the Gemini -2..2 range"},
		{outsidePositiveInt32(options.MaxOutputTokens), "request.max_output_tokens is outside the Gemini 1..int32 range"},
		{outsidePositiveInt32(options.ReasoningBudget), "request.reasoning_budget is outside the Gemini 1..int32 range"},
	}
	for _, rule := range rules {
		if rule.invalid {
			return errors.New(rule.message)
		}
	}

	return nil
}

func outsidePositiveInt32(value *int) bool {
	return value != nil && (*value <= 0 || int64(*value) > math.MaxInt32)
}

func outsideSignedInt32(value *int64) bool {
	return value != nil && (*value < math.MinInt32 || *value > math.MaxInt32)
}

func outside(value *float64, minimum, maximum float64) bool {
	return value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) ||
		*value < minimum || *value > maximum)
}

func legacyAnthropicEffortUnsupported(
	model modelcatalog.ResolvedModel,
	options config.ModelOptions,
) bool {
	if model.ReasoningLevel == nil || options.ReasoningBudget != nil ||
		(options.ReasoningMode != nil &&
			(*options.ReasoningMode == config.ReasoningAdaptive ||
				*options.ReasoningMode == config.ReasoningDisabled)) {
		return false
	}

	switch ai.ReasoningEffort(*model.ReasoningLevel) {
	case ai.ReasoningNone, ai.ReasoningLow, ai.ReasoningMedium, ai.ReasoningHigh:
		return false
	default:
		return true
	}
}

func validateExtra(protocol config.Protocol, extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	reserved := []string{
		"model", "input", "messages", "instructions", "system", "contents",
		"systemInstruction", "tools", "tool_choice", "toolChoice", "toolConfig",
		"stream", "stream_options", "response_format", "text", "temperature", "top_p",
		"top_k", "max_tokens", "max_completion_tokens", "max_output_tokens", "stop",
		"reasoning", "thinking", "seed", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "store", "background", "conversation",
		"previous_response_id", "cachedContent",
	}
	switch protocol {
	case config.ProtocolOpenAIResponses:
		reserved = append(reserved, "include")
	case config.ProtocolOpenAIChatCompletions:
		reserved = append(reserved, "reasoning_effort")
	case config.ProtocolAnthropicMessages:
		reserved = append(reserved, "output_config", "cache_control")
	case config.ProtocolGeminiGenerateContent:
		reserved = append(reserved,
			"generationConfig.temperature", "generationConfig.topP", "generationConfig.topK",
			"generationConfig.maxOutputTokens", "generationConfig.stopSequences",
			"generationConfig.responseMimeType", "generationConfig.responseSchema",
			"generationConfig.seed", "generationConfig.frequencyPenalty",
			"generationConfig.presencePenalty", "generationConfig.responseLogprobs",
			"generationConfig.logprobs", "generationConfig.responseModalities",
			"generationConfig.thinkingConfig",
		)
	case config.ProtocolOpenAIAuto:
		return errors.New("unresolved openai/auto protocol")
	}

	return ai.ValidateRequestBodyExtension(extra, reserved...)
}

func applyPortable(request *ai.Request, model modelcatalog.ResolvedModel, options config.ModelOptions) {
	fillPointer(&request.MaxTokens, options.MaxOutputTokens)
	fillPointer(&request.Temperature, options.Temperature)
	fillPointer(&request.TopP, options.TopP)
	fillPointer(&request.TopK, options.TopK)
	fillPointer(&request.Seed, options.Seed)
	fillPointer(&request.FrequencyPenalty, options.FrequencyPenalty)
	fillPointer(&request.PresencePenalty, options.PresencePenalty)
	if len(request.Stop) == 0 && options.Stop != nil {
		request.Stop = slices.Clone(*options.Stop)
	}
	if request.LogProbs == nil && options.LogProbs != nil {
		request.LogProbs = &ai.LogProbsConfig{Enabled: *options.LogProbs}
		if options.TopLogProbs != nil {
			request.LogProbs.Top = *options.TopLogProbs
		}
	}
	if request.Reasoning == nil && hasReasoning(model, options) {
		request.Reasoning = &ai.ReasoningConfig{}
		if model.ReasoningLevel != nil {
			request.Reasoning.Effort = ai.ReasoningEffort(*model.ReasoningLevel)
		}
		if options.ReasoningMode != nil {
			request.Reasoning.Mode = ai.ReasoningMode(*options.ReasoningMode)
		}
		if options.ReasoningBudget != nil {
			request.Reasoning.BudgetTokens = *options.ReasoningBudget
		}
		if options.IncludeReasoning != nil {
			request.Reasoning.IncludeSummary = *options.IncludeReasoning
		}
	}
}

func applyProviderOptions(
	request *ai.Request,
	model modelcatalog.ResolvedModel,
	options config.ModelOptions,
) {
	request.ProviderOptions = maps.Clone(request.ProviderOptions)
	if request.ProviderOptions == nil {
		request.ProviderOptions = map[ai.Provider]any{}
	}
	switch model.Protocol {
	case config.ProtocolOpenAIResponses, config.ProtocolOpenAIChatCompletions:
		current, _ := request.ProviderOptions[model.Ref.Provider].(openai.RequestOptions)
		fillPointer(&current.MinP, options.MinP)
		fillPointer(&current.RepetitionPenalty, options.RepetitionPenalty)
		current.ExtraFields = fillExtra(current.ExtraFields, options.ExtraBody)
		request.ProviderOptions[model.Ref.Provider] = current
	case config.ProtocolAnthropicMessages:
		current, _ := request.ProviderOptions[model.Ref.Provider].(anthropic.RequestOptions)
		current.ExtraFields = fillExtra(current.ExtraFields, options.ExtraBody)
		request.ProviderOptions[model.Ref.Provider] = current
	case config.ProtocolGeminiGenerateContent:
		current, _ := request.ProviderOptions[model.Ref.Provider].(gemini.RequestOptions)
		current.ExtraFields = fillExtra(current.ExtraFields, options.ExtraBody)
		request.ProviderOptions[model.Ref.Provider] = current
	case config.ProtocolOpenAIAuto:
		// Compile rejects unresolved protocols before constructing this policy.
	}
}

func fillExtra(explicit, defaults map[string]any) map[string]any {
	if len(explicit) == 0 && len(defaults) == 0 {
		return nil
	}
	result := make(map[string]any, len(defaults)+len(explicit))
	maps.Copy(result, defaults)
	maps.Copy(result, explicit)

	return result
}

func hasReasoning(model modelcatalog.ResolvedModel, options config.ModelOptions) bool {
	return model.ReasoningLevel != nil || options.ReasoningMode != nil ||
		options.ReasoningBudget != nil || options.IncludeReasoning != nil
}

func fillPointer[T any](target **T, value *T) {
	if *target == nil && value != nil {
		*target = new(*value)
	}
}
