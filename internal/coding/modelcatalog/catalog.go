// Package modelcatalog resolves immutable provider/model snapshots from the
// local coding configuration. It never performs discovery or network I/O.
//
//nolint:wsl_v5 // Resolution and validation stages stay explicit and adjacent.
package modelcatalog

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openai/compat"
	"github.com/rsbin1178/pips/internal/coding/config"
)

const originBuiltIn = "built-in"

// ErrInvalid means a registry entry or selection cannot be resolved safely.
var ErrInvalid = errors.New("coding model catalog: invalid configuration")

// Selection is one process-local model, variant, and reasoning choice.
type Selection struct {
	Ref               config.ModelRef
	Variant           string
	ReasoningOverride *config.ReasoningLevel
}

// Endpoint is the resolved provider connection boundary.
type Endpoint struct {
	BaseURL         string
	AllowHTTP       bool
	AllowPrivateIPs bool
	Origin          string
}

// Limits are local context metadata and are never sent as request options.
type Limits struct {
	ContextWindow int
}

// ResolvedModel is an immutable-by-convention runtime snapshot.
type ResolvedModel struct {
	Ref              config.ModelRef
	Protocol         config.Protocol
	Endpoint         Endpoint
	Limits           Limits
	Compatibility    openai.Compatibility
	Options          config.ModelOptions
	Variant          string
	ReasoningLevels  []config.ReasoningLevel
	ReasoningLevel   *config.ReasoningLevel
	ReasoningBudgets map[config.ReasoningLevel]int
}

// Clone returns a fully detached runtime snapshot.
func (m ResolvedModel) Clone() ResolvedModel {
	cloned := m
	cloned.Options = m.Options.Clone()
	cloned.ReasoningLevels = slices.Clone(m.ReasoningLevels)
	cloned.ReasoningLevel = clonePointer(m.ReasoningLevel)
	cloned.ReasoningBudgets = maps.Clone(m.ReasoningBudgets)

	return cloned
}

// Equal reports value equality without relying on pointer or map identity.
func (m ResolvedModel) Equal(other ResolvedModel) bool {
	return reflect.DeepEqual(m, other)
}

// Entry is one selectable model and its declared presets.
type Entry struct {
	Ref              config.ModelRef
	ContextWindow    int
	ReasoningLevels  []config.ReasoningLevel
	DefaultReasoning *config.ReasoningLevel
	Variants         []string
	DefaultVariant   string
}

// Catalog is the read-only model lookup boundary used by CLI/TUI/runtime.
// Future extensions can supply the same interface without changing callers.
type Catalog interface {
	Resolve(Selection) (ResolvedModel, error)
	List() []Entry
}

type registry struct {
	providers map[ai.Provider]providerDefinition
	models    map[string]config.ModelConfig
	entries   []Entry
}

type providerDefinition struct {
	endpoint      Endpoint
	protocol      config.Protocol
	compatibility openai.Compatibility
}

// New validates the complete local registry and returns a network-free
// catalog. The current selection is included even when its metadata is absent.
func New(value config.Config) (Catalog, error) {
	if err := value.ValidateRuntime(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	providers, err := resolveProviders(value.Providers)
	if err != nil {
		return nil, err
	}
	models := make(map[string]config.ModelConfig, len(value.Models)+1)
	for _, model := range value.Models {
		models[model.Ref.String()] = model.Clone()
	}
	if _, exists := models[value.Model.String()]; !exists {
		models[value.Model.String()] = config.ModelConfig{
			Ref:      value.Model,
			Variants: map[string]config.VariantConfig{},
		}
	}

	result := &registry{providers: providers, models: models}
	refs := make([]string, 0, len(models))
	for ref := range models {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		model := models[ref]
		if _, err := result.resolve(Selection{Ref: model.Ref}); err != nil {
			return nil, err
		}
		result.entries = append(result.entries, entryFrom(model))
	}

	return result, nil
}

// SelectionFromConfig returns the initial process-local selection.
func SelectionFromConfig(value config.Config) Selection {
	return Selection{
		Ref:               value.Model,
		Variant:           value.Variant,
		ReasoningOverride: clonePointer(value.Reasoning),
	}
}

func (r *registry) Resolve(selection Selection) (ResolvedModel, error) {
	resolved, err := r.resolve(selection)
	if err != nil {
		return ResolvedModel{}, err
	}

	return resolved.Clone(), nil
}

//nolint:gocyclo // Resolution order is the catalog's core invariant and stays linear.
func (r *registry) resolve(selection Selection) (ResolvedModel, error) {
	if r == nil {
		return ResolvedModel{}, fmt.Errorf("%w: nil catalog", ErrInvalid)
	}
	ref, err := config.ParseModelRef(selection.Ref.String())
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	model, exists := r.models[ref.String()]
	if !exists {
		return ResolvedModel{}, fmt.Errorf("%w: model %q is not in the local catalog", ErrInvalid, ref)
	}
	provider, exists := r.providers[ref.Provider]
	if !exists {
		return ResolvedModel{}, fmt.Errorf("%w: provider %q is not configured", ErrInvalid, ref.Provider)
	}

	protocol := provider.protocol
	if model.Protocol != "" {
		protocol = model.Protocol
	}
	if protocol == "" {
		return ResolvedModel{}, fmt.Errorf("%w: model %q has no protocol", ErrInvalid, ref)
	}
	protocol = resolveProtocol(ref.Model, protocol)
	openAIShaped := config.IsOpenAIProtocol(protocol)
	if !openAIShaped && !reflect.DeepEqual(model.Compatibility, config.CompatibilityConfig{}) {
		return ResolvedModel{}, fmt.Errorf("%w: model %q compatibility requires an OpenAI protocol", ErrInvalid, ref)
	}
	compatibility := openai.Compatibility{}
	if openAIShaped {
		compatibility = model.Compatibility.Resolve(provider.compatibility)
	}

	variantName := selection.Variant
	if variantName == "" {
		variantName = model.DefaultVariant
	}
	options := model.Options.Clone()
	reasoning := clonePointer(model.DefaultReasoningLevel)
	if variantName != "" {
		variant, ok := model.Variants[variantName]
		if !ok {
			return ResolvedModel{}, fmt.Errorf("%w: model %q has no variant %q", ErrInvalid, ref, variantName)
		}
		options, err = options.Overlay(variant.Options)
		if err != nil {
			return ResolvedModel{}, fmt.Errorf("%w: model %q variant %q: %w", ErrInvalid, ref, variantName, err)
		}
		if variant.ReasoningLevel != nil {
			reasoning = clonePointer(variant.ReasoningLevel)
		}
	}
	if selection.ReasoningOverride != nil {
		if !slices.Contains(model.ReasoningLevels, *selection.ReasoningOverride) {
			return ResolvedModel{}, fmt.Errorf(
				"%w: model %q does not support reasoning level %q",
				ErrInvalid,
				ref,
				*selection.ReasoningOverride,
			)
		}
		reasoning = clonePointer(selection.ReasoningOverride)
	}
	useMappedBudget := options.ReasoningMode == nil ||
		(*options.ReasoningMode != config.ReasoningAdaptive &&
			*options.ReasoningMode != config.ReasoningDisabled)
	if reasoning != nil && options.ReasoningBudget == nil && useMappedBudget {
		if budget, ok := model.ReasoningBudgets[*reasoning]; ok {
			options.ReasoningBudget = new(budget)
		}
	}

	return ResolvedModel{
		Ref:              ref,
		Protocol:         protocol,
		Endpoint:         provider.endpoint,
		Limits:           Limits{ContextWindow: model.ContextWindow},
		Compatibility:    compatibility,
		Options:          options,
		Variant:          variantName,
		ReasoningLevels:  slices.Clone(model.ReasoningLevels),
		ReasoningLevel:   reasoning,
		ReasoningBudgets: maps.Clone(model.ReasoningBudgets),
	}, nil
}

func (r *registry) List() []Entry {
	if r == nil {
		return nil
	}
	entries := make([]Entry, len(r.entries))
	for index, entry := range r.entries {
		entry.ReasoningLevels = slices.Clone(entry.ReasoningLevels)
		entry.DefaultReasoning = clonePointer(entry.DefaultReasoning)
		entry.Variants = slices.Clone(entry.Variants)
		entries[index] = entry
	}

	return entries
}

//nolint:gocyclo // Reviewed defaults and user overrides are resolved in one boundary.
func resolveProviders(overrides map[ai.Provider]config.ProviderConfig) (map[ai.Provider]providerDefinition, error) {
	providers := map[ai.Provider]providerDefinition{
		ai.ProviderOpenAI: {
			endpoint: Endpoint{BaseURL: openai.DefaultBaseURL(), Origin: originBuiltIn},
			protocol: config.ProtocolOpenAIResponses,
		},
		ai.ProviderAnthropic: {
			endpoint: Endpoint{BaseURL: anthropic.DefaultBaseURL(), Origin: originBuiltIn},
			protocol: config.ProtocolAnthropicMessages,
		},
		ai.ProviderGemini: {
			endpoint: Endpoint{BaseURL: gemini.DefaultBaseURL(), Origin: originBuiltIn},
			protocol: config.ProtocolGeminiGenerateContent,
		},
	}
	for _, provider := range []ai.Provider{
		ai.ProviderDeepSeek,
		ai.ProviderGroq,
		ai.ProviderXAI,
		ai.ProviderOpenRouter,
		ai.ProviderCerebras,
		ai.ProviderTogether,
		ai.ProviderMistral,
	} {
		profile, ok := compat.Lookup(provider)
		if !ok {
			return nil, fmt.Errorf("%w: missing reviewed profile %q", ErrInvalid, provider)
		}
		providers[provider] = providerDefinition{
			endpoint: Endpoint{BaseURL: profile.BaseURL, Origin: originBuiltIn},
			protocol: protocolFromOpenAI(profile.API), compatibility: profile.Compatibility,
		}
	}

	for provider, override := range overrides {
		definition, reviewed := providers[provider]
		if !reviewed && (override.BaseURL == "" || override.Protocol == "") {
			return nil, fmt.Errorf("%w: custom provider %q requires base_url and protocol", ErrInvalid, provider)
		}
		if override.BaseURL != "" {
			definition.endpoint.BaseURL = override.BaseURL
			definition.endpoint.Origin = "config"
		}
		if override.Protocol != "" {
			definition.protocol = override.Protocol
		}
		definition.endpoint.AllowHTTP = override.AllowHTTP
		definition.endpoint.AllowPrivateIPs = override.AllowPrivateIPs
		definition.compatibility = override.Compatibility.Resolve(definition.compatibility)
		openAIShaped := config.IsOpenAIProtocol(definition.protocol)
		if !openAIShaped && !reflect.DeepEqual(override.Compatibility, config.CompatibilityConfig{}) {
			return nil, fmt.Errorf("%w: provider %q compatibility requires an OpenAI protocol", ErrInvalid, provider)
		}
		if err := validateEndpoint(provider, definition.endpoint); err != nil {
			return nil, err
		}
		providers[provider] = definition
	}

	for provider, definition := range providers {
		if err := validateEndpoint(provider, definition.endpoint); err != nil {
			return nil, err
		}
	}

	return providers, nil
}

//nolint:gocyclo // URL and static SSRF checks form one fail-closed validator.
func validateEndpoint(provider ai.Provider, endpoint Endpoint) error {
	if strings.ContainsFunc(endpoint.BaseURL, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%w: provider %q base_url contains control characters", ErrInvalid, provider)
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: provider %q has an invalid base_url", ErrInvalid, provider)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !endpoint.AllowHTTP {
			return fmt.Errorf("%w: provider %q http base_url requires allow_http", ErrInvalid, provider)
		}
	default:
		return fmt.Errorf("%w: provider %q base_url must use https or explicitly allowed http", ErrInvalid, provider)
	}

	host := parsed.Hostname()
	ip := net.ParseIP(host)
	private := strings.EqualFold(host, "localhost") || ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast())
	if private && !endpoint.AllowPrivateIPs {
		return fmt.Errorf("%w: provider %q private base_url requires allow_private_ips", ErrInvalid, provider)
	}

	return nil
}

func entryFrom(model config.ModelConfig) Entry {
	variants := make([]string, 0, len(model.Variants))
	for name := range model.Variants {
		variants = append(variants, name)
	}
	slices.Sort(variants)

	return Entry{
		Ref: model.Ref, ContextWindow: model.ContextWindow,
		ReasoningLevels:  slices.Clone(model.ReasoningLevels),
		DefaultReasoning: clonePointer(model.DefaultReasoningLevel),
		Variants:         variants, DefaultVariant: model.DefaultVariant,
	}
}

func protocolFromOpenAI(api openai.API) config.Protocol {
	if api == openai.APIResponses {
		return config.ProtocolOpenAIResponses
	}

	return config.ProtocolOpenAIChatCompletions
}

func resolveProtocol(modelID string, protocol config.Protocol) config.Protocol {
	if protocol != config.ProtocolOpenAIAuto {
		return protocol
	}

	resolved := openai.ResolveAPI(modelID, openai.APIAuto)

	return protocolFromOpenAI(resolved)
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}

	return new(*value)
}
