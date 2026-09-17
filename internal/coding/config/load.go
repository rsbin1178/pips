//nolint:wsl_v5 // Strict decode keeps each presence-aware assignment explicit.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/statusline"
)

const maxConfigFileSize = 1 << 20

// Environment variable names understood by the configuration loader.
const (
	ModelEnv            = "PIPS_MODEL"
	VariantEnv          = "PIPS_VARIANT"
	ReasoningEnv        = "PIPS_REASONING"
	ToolSearchEnv       = "PIPS_TOOL_SEARCH"
	DynamicSubagentsEnv = "PIPS_DYNAMIC_SUBAGENTS"
	ModeEnv             = "PIPS_MODE"
	SandboxEnv          = "PIPS_SANDBOX"
	ApprovalEnv         = "PIPS_APPROVAL"
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
	patch                 Patch
	providers             map[ai.Provider]ProviderConfig
	models                []ModelConfig
	compaction            *CompactionConfig
	subagent              *SubagentConfig
	subagentFields        []Field
	sandboxWorkspaceWrite *SandboxWorkspaceWriteConfig
	statusLine            *[]statusline.Item
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
		if layer.compaction != nil {
			result.Config.Compaction = *layer.compaction
		}
		if layer.subagent != nil {
			result.Config.Subagent = *layer.subagent
			for _, field := range layer.subagentFields {
				result.Config.sources[field] = Source{
					Kind: SourceConfigFile, Detail: options.ConfigFile,
				}
			}
		}
		if layer.sandboxWorkspaceWrite != nil {
			result.Config.SandboxWorkspaceWrite = *layer.sandboxWorkspaceWrite
			result.Config.sources[FieldSandboxNetwork] = Source{
				Kind: SourceConfigFile, Detail: options.ConfigFile,
			}
		}
		if layer.statusLine != nil {
			result.Config.TUI.StatusLine = slices.Clone(*layer.statusLine)
			result.Config.sources[FieldStatusLine] = Source{
				Kind: SourceConfigFile, Detail: options.ConfigFile,
			}
		}
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
	Variant               *string                    `toml:"variant"`
	Reasoning             *string                    `toml:"reasoning"`
	Providers             map[string]fileProvider    `toml:"providers"`
	ToolSearch            *bool                      `toml:"tool_search"`
	DynamicSubagents      *bool                      `toml:"dynamic_subagents"`
	Mode                  *string                    `toml:"mode"`
	TUI                   *fileTUI                   `toml:"tui"`
	Sandbox               *string                    `toml:"sandbox"`
	SandboxWorkspaceWrite *fileSandboxWorkspaceWrite `toml:"sandbox_workspace_write"`
	Approval              *string                    `toml:"approval"`
	Compaction            *fileCompaction            `toml:"compaction"`
	Subagent              *fileSubagent              `toml:"subagent"`
}

type fileTUI struct {
	Theme      *string   `toml:"theme"`
	StatusLine *[]string `toml:"status_line"`
}

type fileSandboxWorkspaceWrite struct {
	Network *string `toml:"network"`
}

type fileCompaction struct {
	Enabled          *bool `toml:"enabled"`
	ReserveTokens    *int  `toml:"reserve_tokens"`
	KeepRecentTokens *int  `toml:"keep_recent_tokens"`
	SummaryMaxTokens *int  `toml:"summary_max_tokens"`
}

type fileSubagent struct {
	MaxDepth                     *int `toml:"max_depth"`
	MaxConcurrent                *int `toml:"max_concurrent"`
	MaxSpawnedPerRootInteraction *int `toml:"max_spawned_per_root_interaction"`
	MaxAutoFollowUps             *int `toml:"max_auto_follow_ups"`
	MaxTurns                     *int `toml:"max_turns"`
	MaxTokens                    *int `toml:"max_tokens"`
	MaxToolCalls                 *int `toml:"max_tool_calls"`
	MaxDurationMinutes           *int `toml:"max_duration_minutes"`
}

type fileProvider struct {
	BaseURL         *string              `toml:"base_url"`
	Protocol        *string              `toml:"protocol"`
	AllowHTTP       *bool                `toml:"allow_http"`
	AllowPrivateIPs *bool                `toml:"allow_private_ips"`
	Compatibility   fileCompatibility    `toml:"compatibility"`
	Capabilities    fileCapabilities     `toml:"capabilities"`
	Models          map[string]fileModel `toml:"models"`
}

type fileModel struct {
	Default               bool                   `toml:"default"`
	Protocol              *string                `toml:"protocol"`
	ContextWindow         *int                   `toml:"context_window"`
	ReasoningLevels       []string               `toml:"reasoning_levels"`
	DefaultReasoningLevel *string                `toml:"default_reasoning_level"`
	ReasoningBudgets      map[string]int         `toml:"reasoning_budgets"`
	DefaultVariant        *string                `toml:"default_variant"`
	Compatibility         fileCompatibility      `toml:"compatibility"`
	Capabilities          fileCapabilities       `toml:"capabilities"`
	Request               fileOptions            `toml:"request"`
	Variants              map[string]fileVariant `toml:"variants"`
}

type fileVariant struct {
	ReasoningLevel *string     `toml:"reasoning_level"`
	Request        fileOptions `toml:"request"`
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

// fileCapabilities is the presence-aware TOML shape of a capability
// declaration. Every field is optional; an absent key inherits the layer
// below it, which keeps an explicit false distinct from "not declared".
type fileCapabilities struct {
	Text             *bool `toml:"text"`
	Vision           *bool `toml:"vision"`
	Documents        *bool `toml:"documents"`
	AudioInput       *bool `toml:"audio_input"`
	VideoInput       *bool `toml:"video_input"`
	Tools            *bool `toml:"tools"`
	StructuredOutput *bool `toml:"structured_output"`
	Reasoning        *bool `toml:"reasoning"`
	ImageGeneration  *bool `toml:"image_generation"`
	Embeddings       *bool `toml:"embeddings"`
	Reranking        *bool `toml:"reranking"`
	PromptCaching    *bool `toml:"prompt_caching"`
	TokenCounting    *bool `toml:"token_counting"`
	WebSearch        *bool `toml:"web_search"`
	CodeExecution    *bool `toml:"code_execution"`
}

// decodeCapabilities translates the TOML shape into the portable override.
// Every field is presence-aware, so no validation can fail here.
func decodeCapabilities(value fileCapabilities) ai.CapabilityOverride {
	return ai.CapabilityOverride{
		Text:             value.Text,
		Vision:           value.Vision,
		Documents:        value.Documents,
		AudioInput:       value.AudioInput,
		VideoInput:       value.VideoInput,
		Tools:            value.Tools,
		StructuredOutput: value.StructuredOutput,
		Reasoning:        value.Reasoning,
		ImageGeneration:  value.ImageGeneration,
		Embeddings:       value.Embeddings,
		Reranking:        value.Reranking,
		PromptCaching:    value.PromptCaching,
		TokenCounting:    value.TokenCounting,
		WebSearch:        value.WebSearch,
		CodeExecution:    value.CodeExecution,
	}
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
	if err := rejectLegacySchema(data); err != nil {
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
	if layer.statusLine != nil {
		probe.TUI.StatusLine = slices.Clone(*layer.statusLine)
		probe.sources[FieldStatusLine] = Source{Kind: SourceConfigFile, Detail: path}
	}
	probe.Providers = layer.providers
	probe.Models = layer.models
	if err := validateRegistry(probe); err != nil {
		return fileLayer{}, fmt.Errorf("coding config: %q: %w", path, err)
	}

	return layer, nil
}

func rejectLegacySchema(data []byte) error {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %w", ErrDecode, err)
	}
	if _, exists := raw["model"]; exists {
		return fmt.Errorf(
			"%w: remove top-level model; a single nested model is selected automatically, or set default = true on one nested model",
			ErrMigration,
		)
	}
	if _, exists := raw["models"]; exists {
		return fmt.Errorf(
			"%w: replace [[models]] with [providers.<provider>.models.\"<model-id>\"]",
			ErrMigration,
		)
	}

	providers, ok := raw["providers"].(map[string]any)
	if !ok {
		return nil
	}

	return rejectLegacyProviders(providers)
}

func rejectLegacyProviders(providers map[string]any) error {
	for _, provider := range sortedMapKeys(providers) {
		definition, ok := providers[provider].(map[string]any)
		if !ok {
			continue
		}
		if err := rejectLegacyProvider(provider, definition); err != nil {
			return err
		}
	}

	return nil
}

func rejectLegacyProvider(provider string, definition map[string]any) error {
	if _, exists := definition["api"]; exists {
		return fmt.Errorf(
			"%w: provider %q api was renamed to protocol",
			ErrMigration,
			provider,
		)
	}
	models, ok := definition["models"].(map[string]any)
	if !ok {
		return nil
	}
	for _, model := range sortedMapKeys(models) {
		metadata, ok := models[model].(map[string]any)
		if !ok {
			continue
		}
		if err := rejectLegacyModel(provider+"/"+model, metadata); err != nil {
			return err
		}
	}

	return nil
}

func rejectLegacyModel(ref string, metadata map[string]any) error {
	if _, exists := metadata["api"]; exists {
		return fmt.Errorf("%w: model %q api was renamed to protocol", ErrMigration, ref)
	}
	if _, exists := metadata["options"]; exists {
		return fmt.Errorf("%w: model %q options was renamed to request", ErrMigration, ref)
	}
	if _, exists := metadata["max_output_tokens"]; exists {
		return fmt.Errorf(
			"%w: model %q max_output_tokens moved to request.max_output_tokens",
			ErrMigration,
			ref,
		)
	}
	variants, ok := metadata["variants"].(map[string]any)
	if !ok {
		return nil
	}
	for _, variant := range sortedMapKeys(variants) {
		preset, ok := variants[variant].(map[string]any)
		if !ok {
			continue
		}
		if err := rejectLegacyVariant(ref, variant, preset); err != nil {
			return err
		}
	}

	return nil
}

func rejectLegacyVariant(ref, variant string, preset map[string]any) error {
	if _, exists := preset["options"]; exists {
		return fmt.Errorf(
			"%w: model %q variant %q options was renamed to request",
			ErrMigration,
			ref,
			variant,
		)
	}
	if hasLegacyRequestField(preset) {
		return fmt.Errorf(
			"%w: model %q variant %q request fields moved under request",
			ErrMigration,
			ref,
			variant,
		)
	}

	return nil
}

func hasLegacyRequestField(values map[string]any) bool {
	for _, field := range []string{
		"max_output_tokens", "temperature", "top_p", "top_k", "min_p", "seed",
		"frequency_penalty", "presence_penalty", "repetition_penalty", "stop",
		"logprobs", "top_logprobs", "reasoning_mode", "reasoning_budget",
		"include_reasoning", "extra_body",
	} {
		if _, exists := values[field]; exists {
			return true
		}
	}

	return false
}

//nolint:gocyclo,nestif // Strict TOML presence is translated field by field.
func decodeLayer(value fileConfig) (fileLayer, error) {
	layer := fileLayer{
		providers: make(map[ai.Provider]ProviderConfig, len(value.Providers)),
		models:    []ModelConfig{},
	}
	if value.Compaction != nil {
		compaction := DefaultCompactionConfig()
		if value.Compaction.Enabled != nil {
			compaction.Enabled = *value.Compaction.Enabled
		}
		if value.Compaction.ReserveTokens != nil {
			compaction.ReserveTokens = *value.Compaction.ReserveTokens
		}
		if value.Compaction.KeepRecentTokens != nil {
			compaction.KeepRecentTokens = *value.Compaction.KeepRecentTokens
		}
		if value.Compaction.SummaryMaxTokens != nil {
			compaction.SummaryMaxTokens = *value.Compaction.SummaryMaxTokens
		}
		if err := validateCompaction(compaction); err != nil {
			return fileLayer{}, err
		}
		layer.compaction = &compaction
	}
	if value.Subagent != nil {
		subagent := DefaultSubagentConfig()

		setSubagent := func(field Field, source, target *int) {
			if source == nil {
				return
			}
			*target = *source
			layer.subagentFields = append(layer.subagentFields, field)
		}
		setSubagent(FieldSubagentMaxDepth, value.Subagent.MaxDepth, &subagent.MaxDepth)
		setSubagent(FieldSubagentMaxConcurrent, value.Subagent.MaxConcurrent, &subagent.MaxConcurrent)
		setSubagent(
			FieldSubagentMaxSpawned,
			value.Subagent.MaxSpawnedPerRootInteraction,
			&subagent.MaxSpawnedPerRootInteraction,
		)
		setSubagent(FieldSubagentMaxFollowUps, value.Subagent.MaxAutoFollowUps, &subagent.MaxAutoFollowUps)
		setSubagent(FieldSubagentMaxTurns, value.Subagent.MaxTurns, &subagent.MaxTurns)
		setSubagent(FieldSubagentMaxTokens, value.Subagent.MaxTokens, &subagent.MaxTokens)
		setSubagent(FieldSubagentMaxToolCalls, value.Subagent.MaxToolCalls, &subagent.MaxToolCalls)
		setSubagent(
			FieldSubagentMaxDuration,
			value.Subagent.MaxDurationMinutes,
			&subagent.MaxDurationMinutes,
		)
		if err := validateSubagent(subagent); err != nil {
			return fileLayer{}, err
		}
		layer.subagent = &subagent
	}
	if value.SandboxWorkspaceWrite != nil {
		settings := SandboxWorkspaceWriteConfig{Network: SandboxNetworkOnRequest}
		if value.SandboxWorkspaceWrite.Network != nil {
			network, err := ParseSandboxNetworkMode(*value.SandboxWorkspaceWrite.Network)
			if err != nil {
				return fileLayer{}, err
			}
			settings.Network = network
		}
		layer.sandboxWorkspaceWrite = &settings
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
	layer.patch.DynamicSubagents = value.DynamicSubagents
	if value.Mode != nil {
		mode, err := ParseOperatingMode(*value.Mode)
		if err != nil {
			return fileLayer{}, err
		}
		layer.patch.Mode = &mode
	}
	if value.TUI != nil {
		if value.TUI.Theme != nil {
			theme, err := ParseThemeSelection(*value.TUI.Theme)
			if err != nil {
				return fileLayer{}, err
			}
			layer.patch.Theme = &theme
		}
		if value.TUI.StatusLine != nil {
			items := make([]statusline.Item, len(*value.TUI.StatusLine))
			for index, item := range *value.TUI.StatusLine {
				items[index] = statusline.Item(item)
			}
			if err := statusline.Validate(items); err != nil {
				return fileLayer{}, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			layer.statusLine = &items
		}
	}
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

	defaultModels := []ModelRef{}
	for _, key := range sortedMapKeys(value.Providers) {
		raw := value.Providers[key]
		provider, err := ParseProvider(key)
		if err != nil {
			return fileLayer{}, err
		}
		definition, err := decodeProvider(raw)
		if err != nil {
			return fileLayer{}, fmt.Errorf("provider %q: %w", key, err)
		}
		layer.providers[provider] = definition
		for _, modelID := range sortedMapKeys(raw.Models) {
			model, isDefault, decodeErr := decodeModel(provider, modelID, raw.Models[modelID])
			if decodeErr != nil {
				return fileLayer{}, fmt.Errorf(
					"provider %q model %q: %w",
					provider,
					modelID,
					decodeErr,
				)
			}
			layer.models = append(layer.models, model)
			if isDefault {
				defaultModels = append(defaultModels, model.Ref)
			}
		}
	}
	if len(defaultModels) > 1 {
		return fileLayer{}, fmt.Errorf("%w: multiple models set default = true", ErrInvalid)
	}
	if len(defaultModels) == 1 {
		layer.patch.Model = clonePointer(&defaultModels[0])
	} else if len(layer.models) == 1 {
		layer.patch.Model = clonePointer(&layer.models[0].Ref)
	}

	return layer, nil
}

func decodeProvider(value fileProvider) (ProviderConfig, error) {
	var result ProviderConfig
	if value.BaseURL != nil {
		result.BaseURL = strings.TrimSpace(*value.BaseURL)
	}
	if value.Protocol != nil {
		protocol, err := ParseProtocol(*value.Protocol)
		if err != nil {
			return ProviderConfig{}, err
		}
		result.Protocol = protocol
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
	result.Capabilities = decodeCapabilities(value.Capabilities)

	return result, nil
}

//nolint:gocyclo // One decoder owns the complete model schema and path context.
func decodeModel(provider ai.Provider, modelID string, value fileModel) (ModelConfig, bool, error) {
	parsedModelID, err := parseIdentifier("model id", modelID, 512, true)
	if err != nil {
		return ModelConfig{}, false, err
	}
	result := ModelConfig{
		Ref:              ModelRef{Provider: provider, Model: parsedModelID},
		ReasoningBudgets: make(map[ReasoningLevel]int, len(value.ReasoningBudgets)),
		Variants:         make(map[string]VariantConfig, len(value.Variants)),
	}
	if value.Protocol != nil {
		result.Protocol, err = ParseProtocol(*value.Protocol)
		if err != nil {
			return ModelConfig{}, false, err
		}
	}
	if value.ContextWindow != nil {
		result.ContextWindow = *value.ContextWindow
	}
	for _, raw := range value.ReasoningLevels {
		level, parseErr := ParseReasoningLevel(raw)
		if parseErr != nil {
			return ModelConfig{}, false, parseErr
		}
		result.ReasoningLevels = append(result.ReasoningLevels, level)
	}
	if value.DefaultReasoningLevel != nil {
		level, parseErr := ParseReasoningLevel(*value.DefaultReasoningLevel)
		if parseErr != nil {
			return ModelConfig{}, false, parseErr
		}
		result.DefaultReasoningLevel = &level
	}
	for raw, budget := range value.ReasoningBudgets {
		level, parseErr := ParseReasoningLevel(raw)
		if parseErr != nil {
			return ModelConfig{}, false, parseErr
		}
		result.ReasoningBudgets[level] = budget
	}
	if value.DefaultVariant != nil {
		result.DefaultVariant, err = ParseVariant(*value.DefaultVariant)
		if err != nil {
			return ModelConfig{}, false, err
		}
	}
	result.Compatibility, err = decodeCompatibility(value.Compatibility)
	if err != nil {
		return ModelConfig{}, false, err
	}
	result.Capabilities = decodeCapabilities(value.Capabilities)
	result.Options, err = decodeOptions(value.Request)
	if err != nil {
		return ModelConfig{}, false, err
	}
	for _, name := range sortedMapKeys(value.Variants) {
		raw := value.Variants[name]
		variantName, parseErr := ParseVariant(name)
		if parseErr != nil {
			return ModelConfig{}, false, parseErr
		}
		variant := VariantConfig{}
		if raw.ReasoningLevel != nil {
			level, levelErr := ParseReasoningLevel(*raw.ReasoningLevel)
			if levelErr != nil {
				return ModelConfig{}, false, levelErr
			}
			variant.ReasoningLevel = &level
		}
		variant.Options, parseErr = decodeOptions(raw.Request)
		if parseErr != nil {
			return ModelConfig{}, false, parseErr
		}
		result.Variants[variantName] = variant
	}

	return result, value.Default, nil
}

func sortedMapKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
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

	return result, validateOptions(result, "request")
}

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
		switch parsed {
		case openai.ReasoningHistoryContent,
			openai.ReasoningHistoryReasoning,
			openai.ReasoningHistoryDetails,
			openai.ReasoningHistoryContentChunks:
		default:
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
			return fmt.Errorf("%w: %s was removed; use PIPS_MODEL=provider/model and configure protocol under [providers.<id>]", ErrMigration, legacy)
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
		{name: DynamicSubagentsEnv, parse: dynamicSubagentsPatch},
		{name: ModeEnv, parse: operatingModePatch},
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
		{detail: "--dynamic-subagents", patch: Patch{DynamicSubagents: validated.DynamicSubagents}, set: validated.DynamicSubagents != nil},
		{detail: "--mode", patch: Patch{Mode: validated.Mode}, set: validated.Mode != nil},
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
	result.DynamicSubagents = patch.DynamicSubagents
	if patch.Mode != nil {
		mode, err := ParseOperatingMode(string(*patch.Mode))
		if err != nil {
			return Patch{}, err
		}
		result.Mode = &mode
	}
	if patch.Theme != nil {
		theme, err := ParseThemeSelection(*patch.Theme)
		if err != nil {
			return Patch{}, err
		}
		result.Theme = &theme
	}
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
	enabled, err := parseBoolPatch(value)
	if err != nil {
		return Patch{}, err
	}
	return Patch{ToolSearch: &enabled}, nil
}

func dynamicSubagentsPatch(value string) (Patch, error) {
	enabled, err := parseBoolPatch(value)
	if err != nil {
		return Patch{}, err
	}

	return Patch{DynamicSubagents: &enabled}, nil
}

func parseBoolPatch(value string) (bool, error) {
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%w: invalid boolean %q", ErrInvalid, value)
	}

	return enabled, nil
}

func operatingModePatch(value string) (Patch, error) {
	mode, err := ParseOperatingMode(value)

	return Patch{Mode: &mode}, err
}

func sandboxPatch(value string) (Patch, error) {
	mode, err := ParseSandboxMode(value)
	return Patch{Sandbox: &mode}, err
}

func approvalPatch(value string) (Patch, error) {
	mode, err := ParseApprovalMode(value)
	return Patch{Approval: &mode}, err
}
