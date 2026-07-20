package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
)

// ErrInvalid means a configuration value or combination is invalid.
var ErrInvalid = errors.New("coding config: invalid configuration")

// Field identifies one leaf configuration value for provenance queries.
type Field string

// Configuration fields.
const (
	FieldProvider   Field = "model.provider"
	FieldModelID    Field = "model.id"
	FieldModelAPI   Field = "model.api"
	FieldToolSearch Field = "tool_search"
	FieldSandbox    Field = "sandbox"
	FieldApproval   Field = "approval"
)

var fields = []Field{
	FieldProvider,
	FieldModelID,
	FieldModelAPI,
	FieldToolSearch,
	FieldSandbox,
	FieldApproval,
}

// Fields returns all configuration fields in display order.
func Fields() []Field { return slices.Clone(fields) }

// SourceKind identifies a configuration layer.
type SourceKind string

// Configuration source kinds, from lowest to highest precedence.
const (
	SourceDefault     SourceKind = "default"
	SourceUserFile    SourceKind = "user_file"
	SourceProjectFile SourceKind = "project_file"
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

// ModelConfig identifies a provider model without containing credentials.
type ModelConfig struct {
	Provider ai.Provider
	ID       string
	API      openai.API
}

// Config is the final immutable-by-convention application configuration.
type Config struct {
	Model      ModelConfig
	ToolSearch bool
	Sandbox    SandboxMode
	Approval   ApprovalMode

	sources map[Field]Source
}

// Patch represents explicitly set values in one configuration layer. Nil
// fields do not override lower-precedence values.
type Patch struct {
	Provider   *ai.Provider
	ModelID    *string
	ModelAPI   *openai.API
	ToolSearch *bool
	Sandbox    *SandboxMode
	Approval   *ApprovalMode
}

// Defaults returns the built-in configuration. Provider and model ID are
// intentionally unset so the application never hard-codes a time-sensitive
// vendor model default.
func Defaults() Config {
	source := Source{Kind: SourceDefault, Detail: "built-in"}

	sources := make(map[Field]Source, len(fields))
	for _, field := range fields {
		sources[field] = source
	}

	return Config{
		Model:    ModelConfig{API: openai.APIAuto},
		Sandbox:  SandboxWorkspaceWrite,
		Approval: ApprovalOnRequest,
		sources:  sources,
	}
}

// Source returns the winning source for field.
func (c Config) Source(field Field) (Source, bool) {
	source, ok := c.sources[field]

	return source, ok
}

// ValidateRuntime verifies fields required to construct a model and open an
// executable coding runtime.
func (c Config) ValidateRuntime() error {
	if !supportedProvider(c.Model.Provider) {
		if c.Model.Provider == "" {
			return fmt.Errorf("%w: model provider is required", ErrInvalid)
		}

		return fmt.Errorf("%w: unsupported model provider %q", ErrInvalid, c.Model.Provider)
	}

	if _, err := parseModelID(c.Model.ID); err != nil {
		return err
	}

	api, err := ParseModelAPI(string(c.Model.API))
	if err != nil {
		return err
	}

	if c.Model.Provider != ai.ProviderOpenAI && api != openai.APIAuto {
		return fmt.Errorf(
			"%w: model api %q is only supported by provider %q",
			ErrInvalid,
			api,
			ai.ProviderOpenAI,
		)
	}

	if err := validateSandbox(c.Sandbox); err != nil {
		return err
	}

	if err := validateApproval(c.Approval); err != nil {
		return err
	}

	return nil
}

// ParseProvider parses a provider supported by the P0 coding application.
func ParseProvider(value string) (ai.Provider, error) {
	provider := ai.Provider(strings.TrimSpace(value))
	if !supportedProvider(provider) {
		return "", fmt.Errorf("%w: unsupported model provider %q", ErrInvalid, value)
	}

	return provider, nil
}

// ParseModelAPI parses an OpenAI API surface. An empty value has the same
// meaning as the zero-value model configuration: automatic selection.
func ParseModelAPI(value string) (openai.API, error) {
	api := openai.API(strings.TrimSpace(value))
	if api == "" {
		return openai.APIAuto, nil
	}

	switch api {
	case openai.APIAuto, openai.APIChatCompletions, openai.APIResponses:
		return api, nil
	default:
		return "", fmt.Errorf("%w: unsupported model api %q", ErrInvalid, value)
	}
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

func supportedProvider(provider ai.Provider) bool {
	switch provider {
	case ai.ProviderOpenAI, ai.ProviderAnthropic, ai.ProviderGemini:
		return true
	default:
		return false
	}
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

// apply returns config with one explicit layer applied and its source map
// cloned. It is the single merge primitive used by file, environment, and
// command-line loaders.
func apply(config Config, patch Patch, source Source) Config {
	sources := make(map[Field]Source, len(config.sources))
	maps.Copy(sources, config.sources)

	config.sources = sources

	if patch.Provider != nil {
		config.Model.Provider = *patch.Provider
		config.sources[FieldProvider] = source
	}

	if patch.ModelID != nil {
		config.Model.ID = *patch.ModelID
		config.sources[FieldModelID] = source
	}

	if patch.ModelAPI != nil {
		config.Model.API = *patch.ModelAPI
		config.sources[FieldModelAPI] = source
	}

	if patch.ToolSearch != nil {
		config.ToolSearch = *patch.ToolSearch
		config.sources[FieldToolSearch] = source
	}

	if patch.Sandbox != nil {
		config.Sandbox = *patch.Sandbox
		config.sources[FieldSandbox] = source
	}

	if patch.Approval != nil {
		config.Approval = *patch.Approval
		config.sources[FieldApproval] = source
	}

	return config
}
