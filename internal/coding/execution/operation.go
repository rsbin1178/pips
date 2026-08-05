package execution

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	maxToolBytes          = 128
	maxArgumentCount      = 1024
	maxArgumentBytes      = 1 << 20
	maxEnvironmentCount   = 128
	maxEnvironmentBytes   = 1 << 20
	maxJustificationBytes = 4 << 10
	maxWriteDirectories   = 8
	maxTimeout            = 30 * time.Minute
	maxCaptureBytes       = 8 << 20
	maxOutputBytes        = 64 << 20
	maxChunkBytes         = 64 << 10
	maxQueueDepth         = 256
)

// MaxStdinBytes is the largest standard-input payload accepted by one operation.
const MaxStdinBytes = 1 << 20

// Kind identifies the trusted operation producer.
type Kind uint8

// Supported operation kinds.
const (
	KindUnknown Kind = iota
	KindShell
	KindGit
	KindProcess
)

// WorkspaceAccess identifies the requested workspace permission.
type WorkspaceAccess uint8

// Supported workspace access levels.
const (
	WorkspaceAccessUnknown WorkspaceAccess = iota
	WorkspaceReadOnly
	WorkspaceWrite
)

// NetworkAccess identifies the requested host network permission.
type NetworkAccess uint8

// Supported network access levels.
const (
	NetworkUnknown NetworkAccess = iota
	NetworkNone
	NetworkAny
)

// EnvVar is one trusted environment override.
type EnvVar struct {
	Name  string
	Value string
}

// OutputLimits bound captured, streamed, and total process output.
type OutputLimits struct {
	CaptureBytes int64
	MaxBytes     int64
	ChunkBytes   int
	QueueDepth   int
}

// OperationSpec describes a requested command before canonicalization.
type OperationSpec struct {
	Kind       Kind
	Tool       string
	Executable string
	Args       []string
	CWD        string
	Env        []EnvVar
	Stdin      []byte
	Timeout    time.Duration
	Output     OutputLimits
	Workspace  WorkspaceAccess
	WriteDirs  []string
	Network    NetworkAccess
	// NetworkByConfiguration means NetworkAny came from an explicit user
	// sandbox policy rather than a per-call permission request. Policy still
	// decides authority; this flag only avoids demanding model-authored network
	// justification for authority the user already supplied.
	NetworkByConfiguration bool
	Justification          string
}

// Operation is an immutable-by-API, canonical command and permission request.
type Operation struct {
	kind          Kind
	tool          string
	executable    fileObject
	args          []string
	cwd           string
	env           []EnvVar
	stdin         []byte
	timeout       time.Duration
	output        OutputLimits
	workspace     WorkspaceAccess
	workspaceKey  string
	writeDirs     []fileObject
	network       NetworkAccess
	justification string
}

// NewOperation validates and canonicalizes a command request against workspace.
func NewOperation(ctx context.Context, ws workspace.Workspace, spec OperationSpec) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}

	if err := validateWorkspace(ws); err != nil {
		return Operation{}, err
	}

	if err := validateScalarFields(spec); err != nil {
		return Operation{}, err
	}

	executable, err := inspectExecutable(spec.Executable)
	if err != nil {
		return Operation{}, err
	}

	args, err := canonicalArguments(spec.Args)
	if err != nil {
		return Operation{}, err
	}

	cwd, err := canonicalCWD(ws, spec.CWD)
	if err != nil {
		return Operation{}, err
	}

	env, err := canonicalEnvironment(spec.Env)
	if err != nil {
		return Operation{}, err
	}

	writeDirs, err := canonicalWriteDirs(ws, spec.WriteDirs)
	if err != nil {
		return Operation{}, err
	}

	elevated := spec.Network == NetworkAny && !spec.NetworkByConfiguration || len(writeDirs) > 0
	if elevated && strings.TrimSpace(spec.Justification) == "" {
		return Operation{}, invalidOperation(
			"justification",
			"justification_required",
			true,
			"add justification for network or external write access",
			nil,
		)
	}

	return Operation{
		kind:          spec.Kind,
		tool:          spec.Tool,
		executable:    executable,
		args:          args,
		cwd:           cwd,
		env:           env,
		stdin:         slices.Clone(spec.Stdin),
		timeout:       spec.Timeout,
		output:        spec.Output,
		workspace:     spec.Workspace,
		workspaceKey:  ws.Identity().Key(),
		writeDirs:     writeDirs,
		network:       spec.Network,
		justification: spec.Justification,
	}, nil
}

// Kind returns the operation kind.
func (o Operation) Kind() Kind { return o.kind }

// Tool returns the stable producer name.
func (o Operation) Tool() string { return o.tool }

// Executable returns the canonical absolute executable path.
func (o Operation) Executable() string { return o.executable.path }

// Args returns an owned copy of the exact argument vector.
func (o Operation) Args() []string { return slices.Clone(o.args) }

// CWD returns the normalized workspace-relative working directory.
func (o Operation) CWD() string { return o.cwd }

// Env returns an owned copy of the sorted trusted environment overrides.
func (o Operation) Env() []EnvVar { return slices.Clone(o.env) }

// Stdin returns an owned copy of standard input.
func (o Operation) Stdin() []byte { return slices.Clone(o.stdin) }

// Timeout returns the operation deadline duration.
func (o Operation) Timeout() time.Duration { return o.timeout }

// Output returns the process output limits.
func (o Operation) Output() OutputLimits { return o.output }

// WorkspaceAccess returns the requested workspace permission.
func (o Operation) WorkspaceAccess() WorkspaceAccess { return o.workspace }

// WriteDirs returns canonical external write directories in stable order.
func (o Operation) WriteDirs() []string {
	paths := make([]string, len(o.writeDirs))
	for index := range o.writeDirs {
		paths[index] = o.writeDirs[index].path
	}

	return paths
}

// Network returns the requested network permission.
func (o Operation) Network() NetworkAccess { return o.network }

// Justification returns display-only approval context.
func (o Operation) Justification() string { return o.justification }

func validateWorkspace(ws workspace.Workspace) error {
	if ws.Root() == "" || ws.Identity().Key() == "" {
		return fmt.Errorf("%w: empty workspace", ErrInvalidOperation)
	}

	current, err := workspace.Open(ws.Root())
	if err != nil {
		return fmt.Errorf("%w: inspect workspace: %w", ErrInvalidOperation, err)
	}

	if current.Identity().Key() != ws.Identity().Key() {
		return fmt.Errorf("%w: workspace identity changed", ErrInvalidOperation)
	}

	return nil
}

func validateScalarFields(spec OperationSpec) error {
	switch spec.Kind {
	case KindShell, KindGit, KindProcess:
	default:
		return fmt.Errorf("%w: unsupported kind", ErrInvalidOperation)
	}

	if err := validateText("tool", spec.Tool, maxToolBytes, false); err != nil {
		return err
	}

	switch spec.Workspace {
	case WorkspaceReadOnly, WorkspaceWrite:
	default:
		return fmt.Errorf("%w: unsupported workspace access", ErrInvalidOperation)
	}

	switch spec.Network {
	case NetworkNone, NetworkAny:
	default:
		return fmt.Errorf("%w: unsupported network access", ErrInvalidOperation)
	}

	if spec.Timeout <= 0 || spec.Timeout > maxTimeout {
		return fmt.Errorf("%w: timeout outside supported range", ErrInvalidOperation)
	}

	if err := validateOutputLimits(spec.Output); err != nil {
		return err
	}

	if len(spec.Stdin) > MaxStdinBytes {
		return fmt.Errorf("%w: stdin exceeds byte limit", ErrInvalidOperation)
	}

	if err := validateText("justification", spec.Justification, maxJustificationBytes, true); err != nil {
		return err
	}

	return nil
}

func validateOutputLimits(limits OutputLimits) error {
	if limits.CaptureBytes <= 0 || limits.CaptureBytes > maxCaptureBytes ||
		limits.MaxBytes < limits.CaptureBytes || limits.MaxBytes > maxOutputBytes ||
		limits.ChunkBytes <= 0 || limits.ChunkBytes > maxChunkBytes ||
		limits.QueueDepth <= 0 || limits.QueueDepth > maxQueueDepth {
		return fmt.Errorf("%w: output limits outside supported range", ErrInvalidOperation)
	}

	return nil
}

func canonicalArguments(input []string) ([]string, error) {
	if len(input) > maxArgumentCount {
		return nil, fmt.Errorf("%w: too many arguments", ErrInvalidOperation)
	}

	total := 0

	output := make([]string, len(input))
	for index, argument := range input {
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, 0) {
			return nil, fmt.Errorf("%w: malformed argument %d", ErrInvalidOperation, index)
		}

		total += len(argument)
		if total > maxArgumentBytes {
			return nil, fmt.Errorf("%w: arguments exceed byte limit", ErrInvalidOperation)
		}

		output[index] = argument
	}

	return output, nil
}

func canonicalCWD(ws workspace.Workspace, input string) (string, error) {
	if input == "" {
		input = "."
	}

	normalized, err := normalizeCWDSpelling(ws, input)
	if err != nil {
		return "", err
	}

	absolute := filepath.Join(ws.Root(), filepath.FromSlash(normalized))
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		reason := "cwd_invalid"
		hint := "choose an existing Workspace directory"
		if errors.Is(err, fs.ErrNotExist) {
			reason = "cwd_not_found"
			hint = "create the directory first or choose an existing Workspace directory"
		}

		return "", invalidOperation("cwd", reason, true, hint, err)
	}

	if !pathContains(ws.Root(), resolved) {
		return "", invalidOperation(
			"cwd",
			"cwd_symlink_escape",
			true,
			"choose a directory whose resolved target remains inside the Workspace",
			nil,
		)
	}

	relative, err := filepath.Rel(ws.Root(), resolved)
	if err != nil {
		return "", invalidOperation(
			"cwd", "cwd_invalid", true, "choose an existing Workspace directory", err,
		)
	}

	normalized, err = workspace.NormalizePath(filepath.ToSlash(relative), true)
	if err != nil {
		return "", invalidOperation(
			"cwd", "cwd_invalid", true, "choose an existing Workspace directory", err,
		)
	}

	tree, err := workspace.OpenTree(ws)
	if err != nil {
		return "", fmt.Errorf("%w: open workspace: %w", ErrInvalidOperation, err)
	}
	defer func() { _ = tree.Close() }()

	info, err := tree.Stat(normalized)
	if err != nil {
		reason := "cwd_invalid"
		hint := "choose an existing Workspace directory"
		if errors.Is(err, fs.ErrNotExist) {
			reason = "cwd_not_found"
			hint = "create the directory first or choose an existing Workspace directory"
		}

		return "", invalidOperation("cwd", reason, true, hint, err)
	}

	if !info.IsDir() {
		return "", invalidOperation(
			"cwd",
			"cwd_not_directory",
			true,
			"choose an existing Workspace directory",
			nil,
		)
	}

	return normalized, nil
}

func normalizeCWDSpelling(ws workspace.Workspace, input string) (string, error) {
	if !filepath.IsAbs(input) {
		normalized, err := workspace.NormalizePath(input, true)
		if err == nil {
			return normalized, nil
		}

		reason := "cwd_invalid"
		hint := "choose an existing Workspace directory"
		if errors.Is(err, workspace.ErrOutsideRoot) {
			reason = "cwd_outside_workspace"
			hint = "omit cwd for the Workspace root or choose a directory inside the Workspace"
		}

		return "", invalidOperation("cwd", reason, true, hint, err)
	}

	cleaned := filepath.Clean(input)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil &&
		pathContains(ws.Root(), resolved) {
		cleaned = resolved
	}
	if !pathContains(ws.Root(), cleaned) {
		return "", invalidOperation(
			"cwd",
			"cwd_outside_workspace",
			true,
			"omit cwd for the Workspace root or choose a directory inside the Workspace",
			nil,
		)
	}

	relative, err := filepath.Rel(ws.Root(), cleaned)
	if err != nil {
		return "", invalidOperation(
			"cwd", "cwd_invalid", true, "choose an existing Workspace directory", err,
		)
	}

	normalized, err := workspace.NormalizePath(filepath.ToSlash(relative), true)
	if err != nil {
		return "", invalidOperation(
			"cwd", "cwd_invalid", true, "choose an existing Workspace directory", err,
		)
	}

	return normalized, nil
}

func canonicalEnvironment(input []EnvVar) ([]EnvVar, error) {
	if len(input) > maxEnvironmentCount {
		return nil, fmt.Errorf("%w: too many environment overrides", ErrInvalidOperation)
	}

	output := slices.Clone(input)
	sort.Slice(output, func(left, right int) bool { return output[left].Name < output[right].Name })

	total := 0

	for index, variable := range output {
		if !validEnvironmentName(variable.Name) || unsafeEnvironmentName(variable.Name) {
			return nil, fmt.Errorf("%w: unsafe environment name %q", ErrInvalidOperation, variable.Name)
		}

		if index > 0 && output[index-1].Name == variable.Name {
			return nil, fmt.Errorf("%w: duplicate environment name %q", ErrInvalidOperation, variable.Name)
		}

		if !validPlainText(variable.Value, true) {
			return nil, fmt.Errorf("%w: malformed environment value for %q", ErrInvalidOperation, variable.Name)
		}

		total += len(variable.Name) + len(variable.Value)
		if total > maxEnvironmentBytes {
			return nil, fmt.Errorf("%w: environment exceeds byte limit", ErrInvalidOperation)
		}
	}

	return output, nil
}

func canonicalWriteDirs(ws workspace.Workspace, input []string) ([]fileObject, error) {
	if len(input) > maxWriteDirectories {
		return nil, invalidOperation(
			"permissions.write_paths",
			"write_paths_limit",
			true,
			"request no more than eight exact external write directories",
			nil,
		)
	}

	output := make([]fileObject, 0, len(input))
	for _, directory := range input {
		object, err := inspectWriteDirectory(directory)
		if err != nil {
			return nil, err
		}
		if pathContains(ws.Root(), object.path) {
			continue
		}

		output = append(output, object)
	}

	sort.Slice(output, func(left, right int) bool { return output[left].path < output[right].path })
	output = slices.CompactFunc(output, func(left, right fileObject) bool { return left.path == right.path })

	return output, nil
}

func inspectExecutable(input string) (fileObject, error) {
	if !filepath.IsAbs(input) {
		return fileObject{}, fmt.Errorf("%w: executable must be absolute", ErrInvalidOperation)
	}

	object, info, err := inspectFileObject(input)
	if err != nil {
		return fileObject{}, err
	}

	if !info.Mode().IsRegular() || !executableMode(info.Mode()) {
		return fileObject{}, fmt.Errorf("%w: executable is not an executable regular file", ErrInvalidOperation)
	}

	return object, nil
}

func inspectWriteDirectory(input string) (fileObject, error) {
	if !filepath.IsAbs(input) {
		return fileObject{}, invalidOperation(
			"permissions.write_paths",
			"write_path_absolute",
			true,
			"use an exact absolute directory or omit Workspace-contained write paths",
			nil,
		)
	}

	object, info, err := inspectFileObject(input)
	if err != nil {
		reason := "write_path_invalid"
		hint := "choose an existing absolute external directory"
		if errors.Is(err, fs.ErrNotExist) {
			reason = "write_path_not_found"
			hint = "create the external directory first or remove it from write_paths"
		}

		return fileObject{}, invalidOperation(
			"permissions.write_paths", reason, true, hint, err,
		)
	}

	if !info.IsDir() {
		return fileObject{}, invalidOperation(
			"permissions.write_paths",
			"write_path_not_directory",
			true,
			"choose an existing absolute external directory",
			nil,
		)
	}

	if object.path == string(filepath.Separator) {
		return fileObject{}, invalidOperation(
			"permissions.write_paths",
			"write_path_unsafe",
			true,
			"request only the narrow external directory required by this command",
			nil,
		)
	}

	if home, homeErr := os.UserHomeDir(); homeErr == nil && pathContains(object.path, filepath.Clean(home)) {
		return fileObject{}, invalidOperation(
			"permissions.write_paths",
			"write_path_unsafe",
			true,
			"request only the narrow external directory required by this command",
			nil,
		)
	}

	return object, nil
}

func inspectFileObject(input string) (fileObject, fs.FileInfo, error) {
	canonical, err := filepath.EvalSymlinks(filepath.Clean(input))
	if err != nil {
		return fileObject{}, nil, fmt.Errorf("%w: canonicalize path: %w", ErrInvalidOperation, err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		return fileObject{}, nil, fmt.Errorf("%w: inspect path: %w", ErrInvalidOperation, err)
	}

	device, inode, err := fileIdentity(info)
	if err != nil {
		return fileObject{}, nil, err
	}

	return fileObject{path: canonical, device: device, inode: inode, mode: info.Mode()}, info, nil
}

func validateText(name, value string, limit int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > limit || !validPlainText(value, allowEmpty) {
		return fmt.Errorf("%w: malformed %s", ErrInvalidOperation, name)
	}

	return nil
}

func validPlainText(value string, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validEnvironmentName(name string) bool {
	if name == "" || !validEnvironmentFirstCharacter(name[0]) {
		return false
	}

	for index := 1; index < len(name); index++ {
		if !validEnvironmentCharacter(name[index]) {
			return false
		}
	}

	return true
}

func validEnvironmentFirstCharacter(character byte) bool {
	return character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
}

func validEnvironmentCharacter(character byte) bool {
	return validEnvironmentFirstCharacter(character) || character >= '0' && character <= '9'
}

func unsafeEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	if hasSensitiveEnvironmentSuffix(upper) ||
		strings.HasPrefix(upper, "OTEL_") ||
		strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
		return true
	}

	switch upper {
	case "TOKEN", "PASSWORD", "SECRET", "AUTHORIZATION", "CREDENTIALS",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SSH_AUTH_SOCK", "GPG_AGENT_INFO", "DOCKER_HOST", "KUBECONFIG",
		"BASH_ENV", "ENV", "CDPATH", "SHELLOPTS", "IFS",
		"PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS", "NODE_PATH",
		"RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB",
		"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH":
		return true
	}

	if strings.HasPrefix(upper, "GIT_") {
		_, allowed := safeGitEnvironment[upper]

		return !allowed
	}

	return false
}

func hasSensitiveEnvironmentSuffix(name string) bool {
	return name == "API_KEY" || strings.HasSuffix(name, "_API_KEY") ||
		strings.HasSuffix(name, "_TOKEN") || strings.HasSuffix(name, "_PASSWORD") ||
		strings.HasSuffix(name, "_SECRET") || strings.HasSuffix(name, "_ACCESS_KEY") ||
		strings.HasSuffix(name, "_ACCESS_KEY_ID") || strings.HasSuffix(name, "_PRIVATE_KEY")
}

var safeGitEnvironment = map[string]struct{}{
	"GIT_CEILING_DIRECTORIES": {},
	"GIT_CONFIG_GLOBAL":       {},
	"GIT_CONFIG_NOSYSTEM":     {},
	"GIT_OPTIONAL_LOCKS":      {},
	"GIT_PAGER":               {},
	"GIT_TERMINAL_PROMPT":     {},
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}

	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
