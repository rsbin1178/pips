//nolint:wsl_v5 // Presence-aware clones, validation, and overlays stay explicit.
package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

// ErrInvalid means a configuration value or combination is invalid.
var ErrInvalid = errors.New("coding config: invalid configuration")

// Field identifies one selectable application setting for provenance queries.
type Field string

// Configuration fields.
const (
	FieldModel      Field = "model"
	FieldVariant    Field = "variant"
	FieldReasoning  Field = "reasoning"
	FieldToolSearch Field = "tool_search"
	FieldSandbox    Field = "sandbox"
	FieldApproval   Field = "approval"
)

var fields = []Field{
	FieldModel,
	FieldVariant,
	FieldReasoning,
	FieldToolSearch,
	FieldSandbox,
	FieldApproval,
}

// Fields returns all selectable fields in display order.
func Fields() []Field { return slices.Clone(fields) }

// SourceKind identifies a configuration layer.
type SourceKind string

// Configuration source kinds, from lowest to highest precedence.
const (
	SourceDefault     SourceKind = "default"
	SourceConfigFile  SourceKind = "config_file"
	SourceEnvironment SourceKind = "environment"
	SourceFlag        SourceKind = "flag"
)

// Source records the winning layer and its non-secret origin.
type Source struct {
	Kind   SourceKind
	Detail string
}

// SandboxMode selects the application sandbox boundary.
type SandboxMode string

// Supported sandbox modes.
const (
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxFullAccess     SandboxMode = "full-access"
)

// ApprovalMode selects when operations require explicit approval.
type ApprovalMode string

// Supported approval modes.
const (
	ApprovalOnRequest ApprovalMode = "on-request"
	ApprovalNever     ApprovalMode = "never"
)

// API identifies the provider wire protocol used by a model.
type API string

// Supported wire protocols.
const (
	APIResponses         API = "responses"
	APIChatCompletions   API = "chat_completions"
	APIAnthropicMessages API = "anthropic_messages"
	APIGenerateContent   API = "generate_content"
)

// ReasoningLevel is a model-defined, ordered reasoning capability value.
type ReasoningLevel string

// ReasoningMode controls provider reasoning independently from its level.
type ReasoningMode string

// Portable reasoning modes.
const (
	ReasoningAuto     ReasoningMode = "auto"
	ReasoningEnabled  ReasoningMode = "enabled"
	ReasoningAdaptive ReasoningMode = "adaptive"
	ReasoningDisabled ReasoningMode = "disabled"
)

// ModelRef is the canonical provider/model identity. Model may itself contain
// slashes; only the first slash is structural.
type ModelRef struct {
	Provider ai.Provider
	Model    string
}

// String returns the canonical provider/model identity.
func (r ModelRef) String() string {
	if r.Provider == "" || r.Model == "" {
		return ""
	}

	return string(r.Provider) + "/" + r.Model
}

// ParseModelRef parses a canonical provider/model identity.
func ParseModelRef(value string) (ModelRef, error) {
	trimmed := strings.TrimSpace(value)
	providerValue, modelValue, ok := strings.Cut(trimmed, "/")
	if !ok {
		return ModelRef{}, fmt.Errorf("%w: model %q must use provider/model", ErrInvalid, value)
	}

	provider, err := ParseProvider(providerValue)
	if err != nil {
		return ModelRef{}, err
	}

	model, err := parseIdentifier("model id", modelValue, 512, true)
	if err != nil {
		return ModelRef{}, err
	}

	return ModelRef{Provider: provider, Model: model}, nil
}

// CompatibilityConfig contains presence-aware OpenAI-compatible wire
// overrides. Nil means inherit the reviewed provider profile.
type CompatibilityConfig struct {
	MaxTokensField            *openai.MaxTokensField
	StreamUsage               *openai.StreamUsageMode
	StructuredOutput          *openai.StructuredOutputMode
	ChatReasoning             *openai.ChatReasoningFormat
	ReasoningHistory          *openai.ReasoningHistoryField
	IncludeEncryptedReasoning *bool
}

// Clone returns a fully detached compatibility override.
func (c CompatibilityConfig) Clone() CompatibilityConfig {
	cloned := c
	cloned.MaxTokensField = clonePointer(c.MaxTokensField)
	cloned.StreamUsage = clonePointer(c.StreamUsage)
	cloned.StructuredOutput = clonePointer(c.StructuredOutput)
	cloned.ChatReasoning = clonePointer(c.ChatReasoning)
	cloned.ReasoningHistory = clonePointer(c.ReasoningHistory)
	cloned.IncludeEncryptedReasoning = clonePointer(c.IncludeEncryptedReasoning)

	return cloned
}

// Equal reports value equality without relying on pointer identity.
func (c CompatibilityConfig) Equal(other CompatibilityConfig) bool {
	return reflect.DeepEqual(c, other)
}

// Resolve applies this override to a base compatibility profile.
func (c CompatibilityConfig) Resolve(base openai.Compatibility) openai.Compatibility {
	if c.MaxTokensField != nil {
		base.MaxTokensField = *c.MaxTokensField
	}
	if c.StreamUsage != nil {
		base.StreamUsage = *c.StreamUsage
	}
	if c.StructuredOutput != nil {
		base.StructuredOutput = *c.StructuredOutput
	}
	if c.ChatReasoning != nil {
		base.ChatReasoning = *c.ChatReasoning
	}
	if c.ReasoningHistory != nil {
		base.ReasoningHistory = *c.ReasoningHistory
	}
	if c.IncludeEncryptedReasoning != nil {
		base.IncludeEncryptedReasoning = *c.IncludeEncryptedReasoning
	}

	return base
}

// ProviderConfig describes one reusable provider connection. Credentials are
// intentionally absent and are acquired from credential.Store at runtime.
type ProviderConfig struct {
	BaseURL         string
	API             API
	AllowHTTP       bool
	AllowPrivateIPs bool
	Compatibility   CompatibilityConfig
}

// Clone returns a fully detached provider definition.
func (p ProviderConfig) Clone() ProviderConfig {
	cloned := p
	cloned.Compatibility = p.Compatibility.Clone()

	return cloned
}

// Equal reports value equality without relying on pointer identity.
func (p ProviderConfig) Equal(other ProviderConfig) bool {
	return reflect.DeepEqual(p, other)
}

// ModelOptions are presence-aware per-request defaults. Metadata limits live
// on ModelConfig instead and are never sent to a provider.
type ModelOptions struct {
	MaxOutputTokens   *int
	Temperature       *float64
	TopP              *float64
	TopK              *int
	MinP              *float64
	Seed              *int64
	FrequencyPenalty  *float64
	PresencePenalty   *float64
	RepetitionPenalty *float64
	Stop              *[]string
	LogProbs          *bool
	TopLogProbs       *int
	ReasoningMode     *ReasoningMode
	ReasoningBudget   *int
	IncludeReasoning  *bool
	ExtraBody         map[string]any
}

// Clone returns a fully detached copy.
func (o ModelOptions) Clone() ModelOptions {
	cloned := o
	cloned.MaxOutputTokens = clonePointer(o.MaxOutputTokens)
	cloned.Temperature = clonePointer(o.Temperature)
	cloned.TopP = clonePointer(o.TopP)
	cloned.TopK = clonePointer(o.TopK)
	cloned.MinP = clonePointer(o.MinP)
	cloned.Seed = clonePointer(o.Seed)
	cloned.FrequencyPenalty = clonePointer(o.FrequencyPenalty)
	cloned.PresencePenalty = clonePointer(o.PresencePenalty)
	cloned.RepetitionPenalty = clonePointer(o.RepetitionPenalty)
	cloned.LogProbs = clonePointer(o.LogProbs)
	cloned.TopLogProbs = clonePointer(o.TopLogProbs)
	cloned.ReasoningMode = clonePointer(o.ReasoningMode)
	cloned.ReasoningBudget = clonePointer(o.ReasoningBudget)
	cloned.IncludeReasoning = clonePointer(o.IncludeReasoning)
	if o.Stop != nil {
		value := slices.Clone(*o.Stop)
		cloned.Stop = &value
	}
	cloned.ExtraBody = cloneRawMap(o.ExtraBody)

	return cloned
}

// Overlay returns o with every explicitly present typed value from override.
// Raw extension objects use recursive add-only semantics and reject collisions.
func (o ModelOptions) Overlay(override ModelOptions) (ModelOptions, error) {
	merged := o.Clone()
	if err := applyOptionOverlay(&merged, override); err != nil {
		return ModelOptions{}, err
	}

	return merged, nil
}

// Equal reports value equality without relying on pointer identity.
func (o ModelOptions) Equal(other ModelOptions) bool {
	return reflect.DeepEqual(o, other)
}

// VariantConfig is a named request-option overlay.
type VariantConfig struct {
	ReasoningLevel *ReasoningLevel
	Options        ModelOptions
}

// ModelConfig contains local metadata and request defaults for one model.
type ModelConfig struct {
	Ref                   ModelRef
	API                   API
	ContextWindow         int
	MaxOutputTokens       int
	ReasoningLevels       []ReasoningLevel
	DefaultReasoningLevel *ReasoningLevel
	ReasoningBudgets      map[ReasoningLevel]int
	DefaultVariant        string
	Compatibility         CompatibilityConfig
	Options               ModelOptions
	Variants              map[string]VariantConfig
}

// Clone returns a fully detached model definition.
func (m ModelConfig) Clone() ModelConfig {
	cloned := m
	cloned.Compatibility = m.Compatibility.Clone()
	cloned.ReasoningLevels = slices.Clone(m.ReasoningLevels)
	cloned.DefaultReasoningLevel = clonePointer(m.DefaultReasoningLevel)
	cloned.ReasoningBudgets = maps.Clone(m.ReasoningBudgets)
	cloned.Options = m.Options.Clone()
	cloned.Variants = make(map[string]VariantConfig, len(m.Variants))
	for name, variant := range m.Variants {
		variant.ReasoningLevel = clonePointer(variant.ReasoningLevel)
		variant.Options = variant.Options.Clone()
		cloned.Variants[name] = variant
	}

	return cloned
}

// Equal reports value equality without map or pointer identity.
func (m ModelConfig) Equal(other ModelConfig) bool {
	return reflect.DeepEqual(m, other)
}

// Config is the final immutable-by-convention application configuration.
type Config struct {
	Model      ModelRef
	Variant    string
	Reasoning  *ReasoningLevel
	Providers  map[ai.Provider]ProviderConfig
	Models     []ModelConfig
	ToolSearch bool
	Sandbox    SandboxMode
	Approval   ApprovalMode

	sources map[Field]Source
}

// Patch represents explicitly set values in one selection/settings layer.
// Registry definitions are loaded from exactly one selected file and are not
// patched by environment variables or flags.
type Patch struct {
	Model      *ModelRef
	Variant    *string
	Reasoning  *ReasoningLevel
	ToolSearch *bool
	Sandbox    *SandboxMode
	Approval   *ApprovalMode
}

// Defaults returns the built-in application settings. Model is intentionally
// unset so the application never hard-codes a time-sensitive model ID.
func Defaults() Config {
	source := Source{Kind: SourceDefault, Detail: "built-in"}
	sources := make(map[Field]Source, len(fields))
	for _, field := range fields {
		sources[field] = source
	}

	return Config{
		Providers: map[ai.Provider]ProviderConfig{},
		Sandbox:   SandboxWorkspaceWrite,
		Approval:  ApprovalOnRequest,
		sources:   sources,
	}
}

// Clone returns a fully detached configuration snapshot.
func (c Config) Clone() Config {
	cloned := c
	cloned.Reasoning = clonePointer(c.Reasoning)
	cloned.Providers = maps.Clone(c.Providers)
	for provider, definition := range cloned.Providers {
		cloned.Providers[provider] = definition.Clone()
	}
	cloned.Models = make([]ModelConfig, len(c.Models))
	for index := range c.Models {
		cloned.Models[index] = c.Models[index].Clone()
	}
	cloned.sources = maps.Clone(c.sources)

	return cloned
}

// Equal reports value equality without map or pointer identity.
func (c Config) Equal(other Config) bool {
	return reflect.DeepEqual(c, other)
}

// Source returns the winning source for field.
func (c Config) Source(field Field) (Source, bool) {
	source, ok := c.sources[field]

	return source, ok
}

// ValidateRuntime verifies registry-independent fields required to resolve an
// executable runtime. modelcatalog performs endpoint/protocol resolution.
func (c Config) ValidateRuntime() error {
	if c.Model.String() == "" {
		return fmt.Errorf("%w: model is required in provider/model form", ErrInvalid)
	}
	if _, err := ParseModelRef(c.Model.String()); err != nil {
		return err
	}
	if err := validateSandbox(c.Sandbox); err != nil {
		return err
	}
	if err := validateApproval(c.Approval); err != nil {
		return err
	}

	return validateRegistry(c)
}

// ParseProvider parses a stable provider identifier. Custom providers are
// allowed; protocol support is validated by modelcatalog after inheritance.
func ParseProvider(value string) (ai.Provider, error) {
	parsed, err := parseIdentifier("provider", value, 64, false)
	if err != nil {
		return "", err
	}
	for _, r := range parsed {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}

		return "", fmt.Errorf("%w: provider %q contains an unsupported character", ErrInvalid, value)
	}

	return ai.Provider(parsed), nil
}

// ParseAPI parses a supported wire protocol.
func ParseAPI(value string) (API, error) {
	api := API(strings.TrimSpace(value))
	switch api {
	case APIResponses, APIChatCompletions, APIAnthropicMessages, APIGenerateContent:
		return api, nil
	default:
		return "", fmt.Errorf("%w: unsupported model api %q", ErrInvalid, value)
	}
}

// ParseReasoningLevel parses a model-defined reasoning selector.
func ParseReasoningLevel(value string) (ReasoningLevel, error) {
	parsed, err := parseIdentifier("reasoning level", value, 64, false)

	return ReasoningLevel(parsed), err
}

// ParseVariant parses a named request preset.
func ParseVariant(value string) (string, error) {
	return parseIdentifier("variant", value, 64, false)
}

// ParseSandboxMode parses a supported sandbox mode.
func ParseSandboxMode(value string) (SandboxMode, error) {
	mode := SandboxMode(strings.TrimSpace(value))
	if err := validateSandbox(mode); err != nil {
		return "", err
	}

	return mode, nil
}

// ParseApprovalMode parses a supported approval mode.
func ParseApprovalMode(value string) (ApprovalMode, error) {
	mode := ApprovalMode(strings.TrimSpace(value))
	if err := validateApproval(mode); err != nil {
		return "", err
	}

	return mode, nil
}

func validateSandbox(mode SandboxMode) error {
	switch mode {
	case SandboxWorkspaceWrite, SandboxFullAccess:
		return nil
	default:
		return fmt.Errorf("%w: unsupported sandbox mode %q", ErrInvalid, mode)
	}
}

func validateApproval(mode ApprovalMode) error {
	switch mode {
	case ApprovalOnRequest, ApprovalNever:
		return nil
	default:
		return fmt.Errorf("%w: unsupported approval mode %q", ErrInvalid, mode)
	}
}

func apply(value Config, patch Patch, source Source) Config {
	value = value.Clone()
	if patch.Model != nil {
		value.Model = *patch.Model
		value.Variant = ""
		value.Reasoning = nil
		value.sources[FieldModel] = source
		value.sources[FieldVariant] = source
		value.sources[FieldReasoning] = source
	}
	if patch.Variant != nil {
		value.Variant = *patch.Variant
		value.sources[FieldVariant] = source
	}
	if patch.Reasoning != nil {
		value.Reasoning = clonePointer(patch.Reasoning)
		value.sources[FieldReasoning] = source
	}
	if patch.ToolSearch != nil {
		value.ToolSearch = *patch.ToolSearch
		value.sources[FieldToolSearch] = source
	}
	if patch.Sandbox != nil {
		value.Sandbox = *patch.Sandbox
		value.sources[FieldSandbox] = source
	}
	if patch.Approval != nil {
		value.Approval = *patch.Approval
		value.sources[FieldApproval] = source
	}

	return value
}

func validateRegistry(c Config) error {
	seen := make(map[string]struct{}, len(c.Models))
	for provider, definition := range c.Providers {
		if parsed, err := ParseProvider(string(provider)); err != nil || parsed != provider {
			return fmt.Errorf("%w: invalid provider definition %q", ErrInvalid, provider)
		}
		if definition.API != "" {
			if _, err := ParseAPI(string(definition.API)); err != nil {
				return err
			}
		}
	}

	for _, model := range c.Models {
		if err := validateModel(model); err != nil {
			return err
		}
		key := model.Ref.String()
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate model %q", ErrInvalid, key)
		}
		seen[key] = struct{}{}
	}

	return nil
}

//nolint:gocyclo // This is the single complete model-definition invariant boundary.
func validateModel(model ModelConfig) error {
	if _, err := ParseModelRef(model.Ref.String()); err != nil {
		return err
	}
	if model.API != "" {
		if _, err := ParseAPI(string(model.API)); err != nil {
			return err
		}
	}
	if model.ContextWindow < 0 || model.MaxOutputTokens < 0 {
		return fmt.Errorf("%w: model %q capacity cannot be negative", ErrInvalid, model.Ref)
	}
	if err := validateOptions(model.Options, "model "+model.Ref.String()+" options"); err != nil {
		return err
	}

	levels := make(map[ReasoningLevel]struct{}, len(model.ReasoningLevels))
	for _, level := range model.ReasoningLevels {
		parsed, err := ParseReasoningLevel(string(level))
		if err != nil || parsed != level {
			return fmt.Errorf("%w: model %q has invalid reasoning level", ErrInvalid, model.Ref)
		}
		if _, duplicate := levels[level]; duplicate {
			return fmt.Errorf("%w: model %q repeats reasoning level %q", ErrInvalid, model.Ref, level)
		}
		levels[level] = struct{}{}
	}
	if model.DefaultReasoningLevel != nil {
		if _, ok := levels[*model.DefaultReasoningLevel]; !ok {
			return fmt.Errorf("%w: model %q default reasoning %q is not supported", ErrInvalid, model.Ref, *model.DefaultReasoningLevel)
		}
	}
	for level, budget := range model.ReasoningBudgets {
		if _, ok := levels[level]; !ok || budget <= 0 {
			return fmt.Errorf("%w: model %q has invalid reasoning budget for %q", ErrInvalid, model.Ref, level)
		}
	}
	if model.DefaultVariant != "" {
		if _, ok := model.Variants[model.DefaultVariant]; !ok {
			return fmt.Errorf("%w: model %q default variant %q is not defined", ErrInvalid, model.Ref, model.DefaultVariant)
		}
	}
	for name, variant := range model.Variants {
		if _, err := ParseVariant(name); err != nil {
			return err
		}
		if variant.ReasoningLevel != nil {
			if _, ok := levels[*variant.ReasoningLevel]; !ok {
				return fmt.Errorf("%w: model %q variant %q reasoning %q is not supported", ErrInvalid, model.Ref, name, *variant.ReasoningLevel)
			}
		}
		if err := validateOptions(variant.Options, "model "+model.Ref.String()+" variant "+name); err != nil {
			return err
		}
	}

	return nil
}

//nolint:gocyclo // Presence-aware option validation is intentionally centralized.
func validateOptions(options ModelOptions, location string) error {
	if options.MaxOutputTokens != nil && *options.MaxOutputTokens <= 0 {
		return fmt.Errorf("%w: %s max_output_tokens must be positive", ErrInvalid, location)
	}
	if options.TopK != nil && *options.TopK <= 0 {
		return fmt.Errorf("%w: %s top_k must be positive", ErrInvalid, location)
	}
	if options.TopLogProbs != nil && (*options.TopLogProbs < 0 || *options.TopLogProbs > 20) {
		return fmt.Errorf("%w: %s top_logprobs is outside 0..20", ErrInvalid, location)
	}
	if options.ReasoningBudget != nil && *options.ReasoningBudget <= 0 {
		return fmt.Errorf("%w: %s reasoning_budget must be positive", ErrInvalid, location)
	}
	if options.ReasoningMode != nil {
		switch *options.ReasoningMode {
		case ReasoningAuto, ReasoningEnabled, ReasoningAdaptive, ReasoningDisabled:
		default:
			return fmt.Errorf("%w: %s has unsupported reasoning_mode %q", ErrInvalid, location, *options.ReasoningMode)
		}
	}
	for _, number := range []*float64{
		options.Temperature,
		options.TopP,
		options.MinP,
		options.FrequencyPenalty,
		options.PresencePenalty,
		options.RepetitionPenalty,
	} {
		if number != nil && (*number != *number || *number > 1.7976931348623157e+308 || *number < -1.7976931348623157e+308) {
			return fmt.Errorf("%w: %s contains a non-finite number", ErrInvalid, location)
		}
	}
	if err := ai.ValidateRequestBodyExtension(options.ExtraBody); err != nil {
		return fmt.Errorf("%w: %s extra_body: %w", ErrInvalid, location, err)
	}

	return nil
}

//nolint:gocyclo // Explicit pointer presence is the merge contract for each field.
func applyOptionOverlay(target *ModelOptions, source ModelOptions) error {
	if source.MaxOutputTokens != nil {
		target.MaxOutputTokens = clonePointer(source.MaxOutputTokens)
	}
	if source.Temperature != nil {
		target.Temperature = clonePointer(source.Temperature)
	}
	if source.TopP != nil {
		target.TopP = clonePointer(source.TopP)
	}
	if source.TopK != nil {
		target.TopK = clonePointer(source.TopK)
	}
	if source.MinP != nil {
		target.MinP = clonePointer(source.MinP)
	}
	if source.Seed != nil {
		target.Seed = clonePointer(source.Seed)
	}
	if source.FrequencyPenalty != nil {
		target.FrequencyPenalty = clonePointer(source.FrequencyPenalty)
	}
	if source.PresencePenalty != nil {
		target.PresencePenalty = clonePointer(source.PresencePenalty)
	}
	if source.RepetitionPenalty != nil {
		target.RepetitionPenalty = clonePointer(source.RepetitionPenalty)
	}
	if source.Stop != nil {
		value := slices.Clone(*source.Stop)
		target.Stop = &value
	}
	if source.LogProbs != nil {
		target.LogProbs = clonePointer(source.LogProbs)
	}
	if source.TopLogProbs != nil {
		target.TopLogProbs = clonePointer(source.TopLogProbs)
	}
	if source.ReasoningMode != nil {
		target.ReasoningMode = clonePointer(source.ReasoningMode)
	}
	if source.ReasoningBudget != nil {
		target.ReasoningBudget = clonePointer(source.ReasoningBudget)
	}
	if source.IncludeReasoning != nil {
		target.IncludeReasoning = clonePointer(source.IncludeReasoning)
	}
	if source.ExtraBody != nil {
		if target.ExtraBody == nil {
			target.ExtraBody = map[string]any{}
		}
		if err := mergeRawMap(target.ExtraBody, cloneRawMap(source.ExtraBody), ""); err != nil {
			return err
		}
	}

	return nil
}

func mergeRawMap(target, source map[string]any, parent string) error {
	for key, value := range source {
		path := key
		if parent != "" {
			path = parent + "." + key
		}
		existing, exists := target[key]
		if !exists {
			target[key] = value
			continue
		}
		existingMap, existingOK := existing.(map[string]any)
		valueMap, valueOK := value.(map[string]any)
		if !existingOK || !valueOK {
			return fmt.Errorf("%w: extra_body path %q collides across option layers", ErrInvalid, path)
		}
		if err := mergeRawMap(existingMap, valueMap, path); err != nil {
			return err
		}
	}

	return nil
}

func parseIdentifier(kind, value string, limit int, allowSlash bool) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrInvalid, kind)
	}
	if len(trimmed) > limit || !utf8.ValidString(trimmed) {
		return "", fmt.Errorf("%w: %s is not valid UTF-8 within %d bytes", ErrInvalid, kind, limit)
	}
	if strings.ContainsFunc(trimmed, unicode.IsControl) || (!allowSlash && strings.Contains(trimmed, "/")) {
		return "", fmt.Errorf("%w: %s contains unsupported characters", ErrInvalid, kind)
	}

	return trimmed, nil
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}

	return new(*value)
}

func cloneRawMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}

	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneRawValue(item)
	}

	return cloned
}

func cloneRawValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneRawMap(item)
	case []any:
		cloned := make([]any, len(item))
		for index := range item {
			cloned[index] = cloneRawValue(item[index])
		}
		return cloned
	default:
		return item
	}
}
