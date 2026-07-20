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
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

const maxConfigFileSize = 1 << 20

// Environment variable names understood by the configuration loader.
const (
	ProviderEnv   = "PIPS_PROVIDER"
	ModelEnv      = "PIPS_MODEL"
	ModelAPIEnv   = "PIPS_MODEL_API"
	ToolSearchEnv = "PIPS_TOOL_SEARCH"
	SandboxEnv    = "PIPS_SANDBOX"
	ApprovalEnv   = "PIPS_APPROVAL"
)

var (
	// ErrFile means a configuration file could not be safely inspected or read.
	ErrFile = errors.New("coding config: file error")
	// ErrDecode means a configuration file is not valid for the strict schema.
	ErrDecode = errors.New("coding config: decode error")
)

// LookupEnv is an injected environment lookup. Load never reads the process
// environment implicitly.
type LookupEnv func(string) (string, bool)

// FileState describes whether a configuration file participated in loading.
type FileState string

// Configuration file states.
const (
	FileStateAbsent    FileState = "absent"
	FileStateUntrusted FileState = "untrusted"
	FileStateLoaded    FileState = "loaded"
)

// FileStatus describes one file layer without exposing file contents.
type FileStatus struct {
	Path  string
	State FileState
}

// LoadOptions are all non-default inputs to one configuration snapshot.
type LoadOptions struct {
	UserFile       string
	ProjectRoot    string
	ProjectFile    string
	ProjectTrusted bool
	LookupEnv      LookupEnv
	FlagOverrides  Patch
}

// Result contains the effective configuration and diagnostic layer state.
type Result struct {
	Config      Config
	UserFile    FileStatus
	ProjectFile FileStatus
}

// Load resolves one configuration snapshot in increasing precedence order.
func Load(options LoadOptions) (Result, error) {
	if options.ProjectTrusted && strings.TrimSpace(options.ProjectFile) != "" &&
		strings.TrimSpace(options.ProjectRoot) == "" {
		return Result{}, fmt.Errorf("%w: project root is required for a trusted project file", ErrFile)
	}

	result := Result{
		Config: Defaults(),
		UserFile: FileStatus{
			Path:  options.UserFile,
			State: FileStateAbsent,
		},
		ProjectFile: FileStatus{
			Path:  options.ProjectFile,
			State: FileStateAbsent,
		},
	}

	userPatch, state, err := loadFile(options.UserFile, "")
	if err != nil {
		return Result{}, err
	}

	result.UserFile.State = state
	if state == FileStateLoaded {
		result.Config = apply(result.Config, userPatch, Source{
			Kind: SourceUserFile, Detail: options.UserFile,
		})
	}

	if options.ProjectTrusted {
		projectPatch, projectState, projectErr := loadFile(options.ProjectFile, options.ProjectRoot)
		if projectErr != nil {
			return Result{}, projectErr
		}

		if err := validateProjectPatch(options.ProjectFile, projectPatch); err != nil {
			return Result{}, err
		}

		result.ProjectFile.State = projectState
		if projectState == FileStateLoaded {
			result.Config = apply(result.Config, projectPatch, Source{
				Kind: SourceProjectFile, Detail: options.ProjectFile,
			})
		}
	} else {
		result.ProjectFile.State = untrustedFileState(options.ProjectFile)
	}

	if err := applyEnvironment(&result.Config, options.LookupEnv); err != nil {
		return Result{}, err
	}

	if err := applyFlagOverrides(&result.Config, options.FlagOverrides); err != nil {
		return Result{}, err
	}

	return result, nil
}

func validateProjectPatch(path string, patch Patch) error {
	if patch.Sandbox != nil && *patch.Sandbox == SandboxFullAccess {
		return fmt.Errorf(
			"coding config: %q: %w: project configuration cannot enable sandbox %q",
			path,
			ErrInvalid,
			SandboxFullAccess,
		)
	}

	return nil
}

type fileConfig struct {
	Model      *fileModel `toml:"model"`
	ToolSearch *bool      `toml:"tool_search"`
	Sandbox    *string    `toml:"sandbox"`
	Approval   *string    `toml:"approval"`
}

type fileModel struct {
	Provider *string `toml:"provider"`
	ID       *string `toml:"id"`
	API      *string `toml:"api"`
}

func loadFile(path, projectRoot string) (Patch, FileState, error) {
	if strings.TrimSpace(path) == "" {
		return Patch{}, FileStateAbsent, nil
	}

	data, exists, err := readFile(path, projectRoot)
	if err != nil {
		return Patch{}, FileStateAbsent, err
	}

	if !exists {
		return Patch{}, FileStateAbsent, nil
	}

	patch, err := decodeFile(path, data)
	if err != nil {
		return Patch{}, FileStateAbsent, err
	}

	return patch, FileStateLoaded, nil
}

func readFile(path, projectRoot string) ([]byte, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: resolve %q: %w", ErrFile, path, err)
	}

	if projectRoot != "" {
		resolveErr := validateProjectFile(abs, projectRoot)
		if errors.Is(resolveErr, os.ErrNotExist) {
			return nil, false, nil
		}

		if resolveErr != nil {
			return nil, false, fmt.Errorf("%w: resolve project file %q: %w", ErrFile, path, resolveErr)
		}
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

func validateProjectFile(path, root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}

	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}

	relative, err := filepath.Rel(absRoot, path)
	if err != nil {
		return fmt.Errorf("relate project file to root: %w", err)
	}

	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("project file is outside the project root")
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}

	expected := filepath.Join(resolvedRoot, relative)
	if filepath.Clean(resolved) != filepath.Clean(expected) {
		return fmt.Errorf(
			"project file uses a symbolic link below the project root: resolved %q, expected %q",
			resolved,
			expected,
		)
	}

	return nil
}

func decodeFile(path string, data []byte) (Patch, error) {
	var value fileConfig

	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&value); err != nil {
		return Patch{}, fmt.Errorf("%w: %q: %w", ErrDecode, path, err)
	}

	patch, err := decodeModel(value.Model)
	if err != nil {
		return Patch{}, fmt.Errorf("coding config: %q: %w", path, err)
	}

	patch.ToolSearch = value.ToolSearch

	if value.Sandbox != nil {
		mode, err := ParseSandboxMode(*value.Sandbox)
		if err != nil {
			return Patch{}, fmt.Errorf("coding config: %q: %w", path, err)
		}

		patch.Sandbox = &mode
	}

	if value.Approval != nil {
		mode, err := ParseApprovalMode(*value.Approval)
		if err != nil {
			return Patch{}, fmt.Errorf("coding config: %q: %w", path, err)
		}

		patch.Approval = &mode
	}

	return patch, nil
}

func decodeModel(value *fileModel) (Patch, error) {
	var patch Patch
	if value == nil {
		return patch, nil
	}

	if value.Provider != nil {
		provider, err := ParseProvider(*value.Provider)
		if err != nil {
			return Patch{}, err
		}

		patch.Provider = &provider
	}

	if value.ID != nil {
		modelID, err := parseModelID(*value.ID)
		if err != nil {
			return Patch{}, err
		}

		patch.ModelID = &modelID
	}

	if value.API != nil {
		api, err := ParseModelAPI(*value.API)
		if err != nil {
			return Patch{}, err
		}

		patch.ModelAPI = &api
	}

	return patch, nil
}

func untrustedFileState(path string) FileState {
	if strings.TrimSpace(path) == "" {
		return FileStateAbsent
	}

	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileStateAbsent
	}

	// Do not inspect or follow a project-controlled path until it is trusted.
	return FileStateUntrusted
}

func applyEnvironment(config *Config, lookup LookupEnv) error {
	if lookup == nil {
		return nil
	}

	parsers := []struct {
		name  string
		parse func(string) (Patch, error)
	}{
		{name: ProviderEnv, parse: providerPatch},
		{name: ModelEnv, parse: modelPatch},
		{name: ModelAPIEnv, parse: modelAPIPatch},
		{name: ToolSearchEnv, parse: toolSearchPatch},
		{name: SandboxEnv, parse: sandboxPatch},
		{name: ApprovalEnv, parse: approvalPatch},
	}

	for _, parser := range parsers {
		value, ok := lookup(parser.name)
		if !ok {
			continue
		}

		patch, err := parser.parse(value)
		if err != nil {
			return fmt.Errorf("coding config: environment %s: %w", parser.name, err)
		}

		*config = apply(*config, patch, Source{
			Kind: SourceEnvironment, Detail: parser.name,
		})
	}

	return nil
}

func applyFlagOverrides(config *Config, patch Patch) error {
	validated, err := validatePatch(patch)
	if err != nil {
		return fmt.Errorf("coding config: flags: %w", err)
	}

	overrides := []struct {
		field  Field
		detail string
		patch  Patch
		set    bool
	}{
		{field: FieldProvider, detail: "--provider", patch: Patch{Provider: validated.Provider}, set: validated.Provider != nil},
		{field: FieldModelID, detail: "--model", patch: Patch{ModelID: validated.ModelID}, set: validated.ModelID != nil},
		{field: FieldModelAPI, detail: "--model-api", patch: Patch{ModelAPI: validated.ModelAPI}, set: validated.ModelAPI != nil},
		{field: FieldToolSearch, detail: "--tool-search", patch: Patch{ToolSearch: validated.ToolSearch}, set: validated.ToolSearch != nil},
		{field: FieldSandbox, detail: "--sandbox", patch: Patch{Sandbox: validated.Sandbox}, set: validated.Sandbox != nil},
		{field: FieldApproval, detail: "--approval", patch: Patch{Approval: validated.Approval}, set: validated.Approval != nil},
	}

	for _, override := range overrides {
		if !override.set {
			continue
		}

		*config = apply(*config, override.patch, Source{
			Kind: SourceFlag, Detail: override.detail,
		})
	}

	return nil
}

func validatePatch(patch Patch) (Patch, error) {
	var validated Patch

	if patch.Provider != nil {
		provider, err := ParseProvider(string(*patch.Provider))
		if err != nil {
			return Patch{}, err
		}

		validated.Provider = &provider
	}

	if patch.ModelID != nil {
		modelID, err := parseModelID(*patch.ModelID)
		if err != nil {
			return Patch{}, err
		}

		validated.ModelID = &modelID
	}

	if patch.ModelAPI != nil {
		api, err := ParseModelAPI(string(*patch.ModelAPI))
		if err != nil {
			return Patch{}, err
		}

		validated.ModelAPI = &api
	}

	validated.ToolSearch = patch.ToolSearch
	if patch.Sandbox != nil {
		mode, err := ParseSandboxMode(string(*patch.Sandbox))
		if err != nil {
			return Patch{}, err
		}

		validated.Sandbox = &mode
	}

	if patch.Approval != nil {
		mode, err := ParseApprovalMode(string(*patch.Approval))
		if err != nil {
			return Patch{}, err
		}

		validated.Approval = &mode
	}

	return validated, nil
}

func providerPatch(value string) (Patch, error) {
	provider, err := ParseProvider(value)
	return Patch{Provider: &provider}, err
}

func modelPatch(value string) (Patch, error) {
	modelID, err := parseModelID(value)
	return Patch{ModelID: &modelID}, err
}

func modelAPIPatch(value string) (Patch, error) {
	api, err := ParseModelAPI(value)
	return Patch{ModelAPI: &api}, err
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

func parseModelID(value string) (string, error) {
	modelID := strings.TrimSpace(value)
	if modelID == "" {
		return "", fmt.Errorf("%w: model id is empty", ErrInvalid)
	}

	if strings.ContainsFunc(modelID, unicode.IsControl) {
		return "", fmt.Errorf("%w: model id contains control characters", ErrInvalid)
	}

	return modelID, nil
}
