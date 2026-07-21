//nolint:wsl_v5 // Strict decode keeps each presence-aware assignment explicit.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

const maxConfigFileSize = 1 << 20

// Environment variable names understood by the configuration loader.
const (
	ModelEnv      = "PIPS_MODEL"
	VariantEnv    = "PIPS_VARIANT"
	ReasoningEnv  = "PIPS_REASONING"
	ToolSearchEnv = "PIPS_TOOL_SEARCH"
	SandboxEnv    = "PIPS_SANDBOX"
	ApprovalEnv   = "PIPS_APPROVAL"
)

var (
	// ErrFile means a configuration file could not be safely inspected or read.
	ErrFile = errors.New("coding config: file error")
	// ErrDecode means a configuration file is not valid for the strict schema.
	ErrDecode = errors.New("coding config: decode error")
	// ErrMigration means a removed configuration surface was detected.
	ErrMigration = errors.New("coding config: migration required")
)

// LookupEnv is an injected environment lookup. Load never reads the process
// environment implicitly.
type LookupEnv func(string) (string, bool)

// FileState describes whether a configuration file participated in loading.
type FileState string

// Configuration file states.
const (
	FileStateAbsent FileState = "absent"
	FileStateLoaded FileState = "loaded"
)

// FileStatus describes one file layer without exposing file contents.
type FileStatus struct {
	Path  string
	State FileState
}

// LoadOptions are all non-default inputs to one configuration snapshot.
type LoadOptions struct {
	ConfigFile    string
	LookupEnv     LookupEnv
	FlagOverrides Patch
}

// Result contains the effective configuration and diagnostic layer state.
type Result struct {
	Config     Config
	ConfigFile FileStatus
}

type fileLayer struct {
	patch     Patch
	providers map[ai.Provider]ProviderConfig
	models    []ModelConfig
}

// Load resolves one configuration snapshot in increasing precedence order.
func Load(options LoadOptions) (Result, error) {
	result := Result{
		Config: Defaults(),
		ConfigFile: FileStatus{
			Path:  options.ConfigFile,
			State: FileStateAbsent,
		},
	}

	layer, state, err := loadFile(options.ConfigFile)
	if err != nil {
		return Result{}, err
	}

	result.ConfigFile.State = state
	if state == FileStateLoaded {
		result.Config = apply(result.Config, layer.patch, Source{
			Kind: SourceConfigFile, Detail: options.ConfigFile,
		})
		result.Config.Providers = layer.providers
		result.Config.Models = layer.models
	}

	if err := applyEnvironment(&result.Config, options.LookupEnv); err != nil {
		return Result{}, err
	}
	if err := applyFlagOverrides(&result.Config, options.FlagOverrides); err != nil {
		return Result{}, err
	}
	if err := validateRegistry(result.Config); err != nil {
		return Result{}, err
	}

	return result, nil
}

type fileConfig struct {
	Model      *string                 `toml:"model"`
	Variant    *string                 `toml:"variant"`
	Reasoning  *string                 `toml:"reasoning"`
	Providers  map[string]fileProvider `toml:"providers"`
	Models     []fileModel             `toml:"models"`
	ToolSearch *bool                   `toml:"tool_search"`
	Sandbox    *string                 `toml:"sandbox"`
	Approval   *string                 `toml:"approval"`
}

type fileProvider struct {
	BaseURL         *string           `toml:"base_url"`
	API             *string           `toml:"api"`
	AllowHTTP       *bool             `toml:"allow_http"`
	AllowPrivateIPs *bool             `toml:"allow_private_ips"`
	Compatibility   fileCompatibility `toml:"compatibility"`
}

type fileModel struct {
	ID                    string                 `toml:"id"`
	API                   *string                `toml:"api"`
	ContextWindow         *int                   `toml:"context_window"`
	MaxOutputTokens       *int                   `toml:"max_output_tokens"`
	ReasoningLevels       []string               `toml:"reasoning_levels"`
	DefaultReasoningLevel *string                `toml:"default_reasoning_level"`
	ReasoningBudgets      map[string]int         `toml:"reasoning_budgets"`
	DefaultVariant        *string                `toml:"default_variant"`
	Compatibility         fileCompatibility      `toml:"compatibility"`
	Options               fileOptions            `toml:"options"`
	Variants              map[string]fileVariant `toml:"variants"`
}

type fileVariant struct {
	ReasoningLevel *string `toml:"reasoning_level"`
	fileOptions
}

type fileOptions struct {
	MaxOutputTokens   *int           `toml:"max_output_tokens"`
	Temperature       *float64       `toml:"temperature"`
	TopP              *float64       `toml:"top_p"`
	TopK              *int           `toml:"top_k"`
	MinP              *float64       `toml:"min_p"`
	Seed              *int64         `toml:"seed"`
	FrequencyPenalty  *float64       `toml:"frequency_penalty"`
	PresencePenalty   *float64       `toml:"presence_penalty"`
	RepetitionPenalty *float64       `toml:"repetition_penalty"`
	Stop              *[]string      `toml:"stop"`
	LogProbs          *bool          `toml:"logprobs"`
	TopLogProbs       *int           `toml:"top_logprobs"`
	ReasoningMode     *string        `toml:"reasoning_mode"`
	ReasoningBudget   *int           `toml:"reasoning_budget"`
	IncludeReasoning  *bool          `toml:"include_reasoning"`
	ExtraBody         map[string]any `toml:"extra_body"`
}

type fileCompatibility struct {
	MaxTokensField            *string `toml:"max_tokens_field"`
	StreamUsage               *string `toml:"stream_usage"`
	StructuredOutput          *string `toml:"structured_output"`
	ChatReasoning             *string `toml:"chat_reasoning"`
	ReasoningHistory          *string `toml:"reasoning_history"`
	IncludeEncryptedReasoning *bool   `toml:"include_encrypted_reasoning"`
}

func loadFile(path string) (fileLayer, FileState, error) {
	if strings.TrimSpace(path) == "" {
		return fileLayer{}, FileStateAbsent, nil
	}

	data, exists, err := readFile(path)
	if err != nil {
		return fileLayer{}, FileStateAbsent, err
	}
	if !exists {
		return fileLayer{}, FileStateAbsent, nil
	}

	layer, err := decodeFile(path, data)
	if err != nil {
		return fileLayer{}, FileStateAbsent, err
	}

	return layer, FileStateLoaded, nil
}

func readFile(path string) ([]byte, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: resolve %q: %w", ErrFile, path, err)
	}

	info, exists, err := inspectFile(abs, path)
	if err != nil || !exists {
		return nil, exists, err
	}

	data, err := readInspectedFile(abs, path, info)
	if err != nil {
		return nil, false, err
	}

	return data, true, nil
}

func inspectFile(abs, path string) (os.FileInfo, bool, error) {
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect %q: %w", ErrFile, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %q is not a regular file", ErrFile, path)
	}
	if info.Size() > maxConfigFileSize {
		return nil, false, fmt.Errorf("%w: %q exceeds %d bytes", ErrFile, path, maxConfigFileSize)
	}

	return info, true, nil
}

func readInspectedFile(abs, path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.Open(abs) //nolint:gosec // abs was resolved and verified as a bounded regular config file.
	if err != nil {
		return nil, fmt.Errorf("%w: open %q: %w", ErrFile, path, err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: inspect opened file %q: %w", ErrFile, path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		return nil, fmt.Errorf("%w: %q changed while opening", ErrFile, path)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %w", ErrFile, path, err)
	}
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrFile, path, maxConfigFileSize)
	}

	return data, nil
}

func decodeFile(path string, data []byte) (fileLayer, error) {
	if err := rejectLegacyModelTable(data); err != nil {
		return fileLayer{}, fmt.Errorf("coding config: %q: %w", path, err)
	}

	var value fileConfig
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return fileLayer{}, fmt.Errorf("%w: %q: %w", ErrDecode, path, err)
	}

	layer, err := decodeLayer(value)
	if err != nil {
		return fileLayer{}, fmt.Errorf("coding config: %q: %w", path, err)
	}

	probe := Defaults()
	probe = apply(probe, layer.patch, Source{Kind: SourceConfigFile, Detail: path})
	probe.Providers = layer.providers
	probe.Models = layer.models
	if err := validateRegistry(probe); err != nil {
		return fileLayer{}, fmt.Errorf("coding config: %q: %w", path, err)
	}

	return layer, nil
}

func rejectLegacyModelTable(data []byte) error {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err == nil {
		_, legacy := raw["model"].(map[string]any)
		if !legacy {
			return nil
		}

		return fmt.Errorf(
			"%w: replace [model] with model = \"provider/model-id\"; configure endpoints under [providers.<id>]",
			ErrMigration,
		)
	}

	return nil
}

//nolint:gocyclo // Strict TOML presence is translated field by field.
func decodeLayer(value fileConfig) (fileLayer, error) {
	layer := fileLayer{
		providers: make(map[ai.Provider]ProviderConfig, len(value.Providers)),
		models:    make([]ModelConfig, 0, len(value.Models)),
	}
	if value.Model != nil {
		ref, err := ParseModelRef(*value.Model)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Model = &ref
	}
	if value.Variant != nil {
		variant, err := ParseVariant(*value.Variant)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Variant = &variant
	}
	if value.Reasoning != nil {
		level, err := ParseReasoningLevel(*value.Reasoning)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Reasoning = &level
	}
	layer.patch.ToolSearch = value.ToolSearch
	if value.Sandbox != nil {
		mode, err := ParseSandboxMode(*value.Sandbox)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Sandbox = &mode
	}
	if value.Approval != nil {
		mode, err := ParseApprovalMode(*value.Approval)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Approval = &mode
	}

	for key, raw := range value.Providers {
		provider, err := ParseProvider(key)
		if err != nil {
			return fileLayer{}, err
		}
		definition, err := decodeProvider(raw)
		if err != nil {
			return fileLayer{}, fmt.Errorf("provider %q: %w", key, err)
		}
		layer.providers[provider] = definition
	}
	for index, raw := range value.Models {
		definition, err := decodeModel(raw)
		if err != nil {
			return fileLayer{}, fmt.Errorf("models[%d]: %w", index, err)
		}
		layer.models = append(layer.models, definition)
	}

	return layer, nil
}

func decodeProvider(value fileProvider) (ProviderConfig, error) {
	var result ProviderConfig
	if value.BaseURL != nil {
		result.BaseURL = strings.TrimSpace(*value.BaseURL)
	}
	if value.API != nil {
		api, err := ParseAPI(*value.API)
		if err != nil {
			return ProviderConfig{}, err
		}
		result.API = api
	}
	if value.AllowHTTP != nil {
		result.AllowHTTP = *value.AllowHTTP
	}
	if value.AllowPrivateIPs != nil {
		result.AllowPrivateIPs = *value.AllowPrivateIPs
	}
	compatibility, err := decodeCompatibility(value.Compatibility)
	if err != nil {
		return ProviderConfig{}, err
	}
	result.Compatibility = compatibility

	return result, nil
}

//nolint:gocyclo // One decoder owns the complete model schema and path context.
func decodeModel(value fileModel) (ModelConfig, error) {
	ref, err := ParseModelRef(value.ID)
	if err != nil {
		return ModelConfig{}, err
	}
	result := ModelConfig{
		Ref:              ref,
		ReasoningBudgets: make(map[ReasoningLevel]int, len(value.ReasoningBudgets)),
		Variants:         make(map[string]VariantConfig, len(value.Variants)),
	}
	if value.API != nil {
		result.API, err = ParseAPI(*value.API)
		if err != nil {
			return ModelConfig{}, err
		}
	}
	if value.ContextWindow != nil {
		result.ContextWindow = *value.ContextWindow
	}
	if value.MaxOutputTokens != nil {
		result.MaxOutputTokens = *value.MaxOutputTokens
	}
	for _, raw := range value.ReasoningLevels {
		level, parseErr := ParseReasoningLevel(raw)
		if parseErr != nil {
			return ModelConfig{}, parseErr
		}
		result.ReasoningLevels = append(result.ReasoningLevels, level)
	}
	if value.DefaultReasoningLevel != nil {
		level, parseErr := ParseReasoningLevel(*value.DefaultReasoningLevel)
		if parseErr != nil {
			return ModelConfig{}, parseErr
		}
		result.DefaultReasoningLevel = &level
	}
	for raw, budget := range value.ReasoningBudgets {
		level, parseErr := ParseReasoningLevel(raw)
		if parseErr != nil {
			return ModelConfig{}, parseErr
		}
		result.ReasoningBudgets[level] = budget
	}
	if value.DefaultVariant != nil {
		result.DefaultVariant, err = ParseVariant(*value.DefaultVariant)
		if err != nil {
			return ModelConfig{}, err
		}
	}
	result.Compatibility, err = decodeCompatibility(value.Compatibility)
	if err != nil {
		return ModelConfig{}, err
	}
	result.Options, err = decodeOptions(value.Options)
	if err != nil {
		return ModelConfig{}, err
	}
	for name, raw := range value.Variants {
		variantName, parseErr := ParseVariant(name)
		if parseErr != nil {
			return ModelConfig{}, parseErr
		}
		variant := VariantConfig{}
		if raw.ReasoningLevel != nil {
			level, levelErr := ParseReasoningLevel(*raw.ReasoningLevel)
			if levelErr != nil {
				return ModelConfig{}, levelErr
			}
			variant.ReasoningLevel = &level
		}
		variant.Options, parseErr = decodeOptions(raw.fileOptions)
		if parseErr != nil {
			return ModelConfig{}, parseErr
		}
		result.Variants[variantName] = variant
	}

	return result, nil
}

func decodeOptions(value fileOptions) (ModelOptions, error) {
	result := ModelOptions{
		MaxOutputTokens: value.MaxOutputTokens, Temperature: value.Temperature,
		TopP: value.TopP, TopK: value.TopK, MinP: value.MinP, Seed: value.Seed,
		FrequencyPenalty: value.FrequencyPenalty, PresencePenalty: value.PresencePenalty,
		RepetitionPenalty: value.RepetitionPenalty, Stop: value.Stop,
		LogProbs: value.LogProbs, TopLogProbs: value.TopLogProbs,
		ReasoningBudget: value.ReasoningBudget, IncludeReasoning: value.IncludeReasoning,
		ExtraBody: cloneRawMap(value.ExtraBody),
	}
	if value.ReasoningMode != nil {
		mode := ReasoningMode(strings.TrimSpace(*value.ReasoningMode))
		result.ReasoningMode = &mode
	}

	return result, validateOptions(result, "options")
}

//nolint:gocyclo // Every enum is validated before entering the runtime snapshot.
func decodeCompatibility(value fileCompatibility) (CompatibilityConfig, error) {
	result := CompatibilityConfig{IncludeEncryptedReasoning: value.IncludeEncryptedReasoning}
	if value.MaxTokensField != nil {
		parsed := openai.MaxTokensField(strings.TrimSpace(*value.MaxTokensField))
		if parsed != openai.MaxTokensFieldCompletion && parsed != openai.MaxTokensFieldLegacy {
			return CompatibilityConfig{}, fmt.Errorf("%w: invalid max_tokens_field", ErrInvalid)
		}
		result.MaxTokensField = &parsed
	}
	if value.StreamUsage != nil {
		parsed := openai.StreamUsageMode(strings.TrimSpace(*value.StreamUsage))
		if parsed != openai.StreamUsageInclude && parsed != openai.StreamUsageOmit {
			return CompatibilityConfig{}, fmt.Errorf("%w: invalid stream_usage", ErrInvalid)
		}
		result.StreamUsage = &parsed
	}
	if value.StructuredOutput != nil {
		parsed := openai.StructuredOutputMode(strings.TrimSpace(*value.StructuredOutput))
		switch parsed {
		case openai.StructuredOutputJSONSchema, openai.StructuredOutputJSONObject, openai.StructuredOutputOmit:
		default:
			return CompatibilityConfig{}, fmt.Errorf("%w: invalid structured_output", ErrInvalid)
		}
		result.StructuredOutput = &parsed
	}
	if value.ChatReasoning != nil {
		parsed := openai.ChatReasoningFormat(strings.TrimSpace(*value.ChatReasoning))
		switch parsed {
		case openai.ChatReasoningEffort, openai.ChatReasoningObject, openai.ChatReasoningDeepSeek, openai.ChatReasoningOmit:
		default:
			return CompatibilityConfig{}, fmt.Errorf("%w: invalid chat_reasoning", ErrInvalid)
		}
		result.ChatReasoning = &parsed
	}
	if value.ReasoningHistory != nil {
		parsed := openai.ReasoningHistoryField(strings.TrimSpace(*value.ReasoningHistory))
		if parsed != openai.ReasoningHistoryContent && parsed != openai.ReasoningHistoryReasoning {
			return CompatibilityConfig{}, fmt.Errorf("%w: invalid reasoning_history", ErrInvalid)
		}
		result.ReasoningHistory = &parsed
	}

	return result, nil
}

func applyEnvironment(value *Config, lookup LookupEnv) error {
	if lookup == nil {
		return nil
	}
	for _, legacy := range []string{"PIPS_PROVIDER", "PIPS_MODEL_API"} {
		if _, set := lookup(legacy); set {
			return fmt.Errorf("%w: %s was removed; use PIPS_MODEL=provider/model and configure api under [providers.<id>]", ErrMigration, legacy)
		}
	}

	parsers := []struct {
		name  string
		parse func(string) (Patch, error)
	}{
		{name: ModelEnv, parse: modelPatch},
		{name: VariantEnv, parse: variantPatch},
		{name: ReasoningEnv, parse: reasoningPatch},
		{name: ToolSearchEnv, parse: toolSearchPatch},
		{name: SandboxEnv, parse: sandboxPatch},
		{name: ApprovalEnv, parse: approvalPatch},
	}
	for _, parser := range parsers {
		raw, ok := lookup(parser.name)
		if !ok {
			continue
		}
		patch, err := parser.parse(raw)
		if err != nil {
			return fmt.Errorf("coding config: environment %s: %w", parser.name, err)
		}
		*value = apply(*value, patch, Source{Kind: SourceEnvironment, Detail: parser.name})
	}

	return nil
}

func applyFlagOverrides(value *Config, patch Patch) error {
	validated, err := validatePatch(patch)
	if err != nil {
		return fmt.Errorf("coding config: flags: %w", err)
	}
	overrides := []struct {
		detail string
		patch  Patch
		set    bool
	}{
		{detail: "--model", patch: Patch{Model: validated.Model}, set: validated.Model != nil},
		{detail: "--variant", patch: Patch{Variant: validated.Variant}, set: validated.Variant != nil},
		{detail: "--reasoning", patch: Patch{Reasoning: validated.Reasoning}, set: validated.Reasoning != nil},
		{detail: "--tool-search", patch: Patch{ToolSearch: validated.ToolSearch}, set: validated.ToolSearch != nil},
		{detail: "--sandbox", patch: Patch{Sandbox: validated.Sandbox}, set: validated.Sandbox != nil},
		{detail: "--approval", patch: Patch{Approval: validated.Approval}, set: validated.Approval != nil},
	}
	for _, override := range overrides {
		if override.set {
			*value = apply(*value, override.patch, Source{Kind: SourceFlag, Detail: override.detail})
		}
	}

	return nil
}

func validatePatch(patch Patch) (Patch, error) {
	var result Patch
	if patch.Model != nil {
		ref, err := ParseModelRef(patch.Model.String())
		if err != nil {
			return Patch{}, err
		}
		result.Model = &ref
	}
	if patch.Variant != nil {
		variant, err := ParseVariant(*patch.Variant)
		if err != nil {
			return Patch{}, err
		}
		result.Variant = &variant
	}
	if patch.Reasoning != nil {
		level, err := ParseReasoningLevel(string(*patch.Reasoning))
		if err != nil {
			return Patch{}, err
		}
		result.Reasoning = &level
	}
	result.ToolSearch = patch.ToolSearch
	if patch.Sandbox != nil {
		mode, err := ParseSandboxMode(string(*patch.Sandbox))
		if err != nil {
			return Patch{}, err
		}
		result.Sandbox = &mode
	}
	if patch.Approval != nil {
		mode, err := ParseApprovalMode(string(*patch.Approval))
		if err != nil {
			return Patch{}, err
		}
		result.Approval = &mode
	}

	return result, nil
}

func modelPatch(value string) (Patch, error) {
	ref, err := ParseModelRef(value)
	return Patch{Model: &ref}, err
}

func variantPatch(value string) (Patch, error) {
	variant, err := ParseVariant(value)
	return Patch{Variant: &variant}, err
}

func reasoningPatch(value string) (Patch, error) {
	level, err := ParseReasoningLevel(value)
	return Patch{Reasoning: &level}, err
}

func toolSearchPatch(value string) (Patch, error) {
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return Patch{}, fmt.Errorf("%w: invalid boolean %q", ErrInvalid, value)
	}
	return Patch{ToolSearch: &enabled}, nil
}

func sandboxPatch(value string) (Patch, error) {
	mode, err := ParseSandboxMode(value)
	return Patch{Sandbox: &mode}, err
}

func approvalPatch(value string) (Patch, error) {
	mode, err := ParseApprovalMode(value)
	return Patch{Approval: &mode}, err
}
