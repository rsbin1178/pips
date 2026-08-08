// Package mcpstdio constructs trusted, unsandboxed MCP stdio transports with
// the Coding Agent's minimal child environment and owned private directories.
//
//nolint:wsl_v5 // Process and filesystem security checks stay adjacent to their owned values.
package mcpstdio

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	SDK "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	defaultTerminateDuration = 5 * time.Second
	maximumTerminateDuration = 30 * time.Second
)

// Config defines one shell-free stdio server process. Native definitions use
// absolute commands; Agent Plugins may also use a bare platform command.
type Config struct {
	Workspace            workspace.Workspace
	Command              string
	Args                 []string
	TempRoot             string
	Environment          func(string) (string, bool)
	EnvironmentOverrides []execution.EnvVar
	WorkingDirectory     string
	PluginRoot           string
	PluginData           string
	TerminateDuration    time.Duration
}

// Resource owns a prepared SDK transport and its private environment root.
// Close it only after the MCP Client has closed its process connection.
type Resource struct {
	transport  *validatedCommandTransport
	privateDir string
	private    fs.FileInfo

	closeOnce sync.Once
	closeErr  error
}

type validatedCommandTransport struct {
	commandTransport  *SDK.CommandTransport
	configuredCommand string
	pluginRoot        string
	pluginData        string
	workingDirectory  string
	environment       []execution.EnvVar
}

func (t *validatedCommandTransport) Connect(ctx context.Context) (SDK.Connection, error) {
	command, err := validateCommand(t.configuredCommand, t.pluginRoot)
	if err != nil {
		return nil, err
	}
	directory := t.commandTransport.Command.Dir
	if t.pluginRoot != "" {
		directory, err = workingDirectory(Config{
			PluginRoot: t.pluginRoot, PluginData: t.pluginData,
			WorkingDirectory: t.workingDirectory, EnvironmentOverrides: t.environment,
		})
		if err != nil {
			return nil, err
		}
	}

	t.commandTransport.Command.Path = command
	t.commandTransport.Command.Args[0] = command
	t.commandTransport.Command.Dir = directory

	return t.commandTransport.Connect(ctx)
}

// NewTransport validates and prepares one MCP stdio transport. It does not
// start the command.
//
//nolint:gocyclo // Process, private-directory, environment, and plugin-path checks are one boundary.
func NewTransport(config Config) (*Resource, error) {
	if err := validateWorkspace(config.Workspace); err != nil {
		return nil, err
	}

	command, err := validateCommand(config.Command, config.PluginRoot)
	if err != nil {
		return nil, err
	}

	arguments, err := validateArguments(config.Args)
	if err != nil {
		return nil, err
	}

	tempRoot, err := validateTempRoot(config.Workspace, config.TempRoot)
	if err != nil {
		return nil, err
	}

	terminateDuration := config.TerminateDuration
	if terminateDuration == 0 {
		terminateDuration = defaultTerminateDuration
	}

	if terminateDuration < 0 || terminateDuration > maximumTerminateDuration {
		return nil, errors.New("coding mcp stdio: invalid terminate duration")
	}

	privateDir, err := os.MkdirTemp(tempRoot, "mcp-")
	if err != nil {
		return nil, fmt.Errorf("coding mcp stdio: create private directory: %w", err)
	}

	removePrivate := true
	defer func() {
		if removePrivate {
			_ = os.RemoveAll(privateDir)
		}
	}()

	if err := os.Chmod(privateDir, 0o700); err != nil { //nolint:gosec // Directories require owner traversal.
		return nil, fmt.Errorf("coding mcp stdio: secure private directory: %w", err)
	}

	privateInfo, err := os.Lstat(privateDir)
	if err != nil {
		return nil, fmt.Errorf("coding mcp stdio: inspect private directory: %w", err)
	}

	overrides := config.EnvironmentOverrides
	if config.PluginRoot != "" {
		overrides = nil
	}
	environment, err := execution.NewChildEnvironment(
		config.Environment,
		privateDir,
		overrides,
	)
	if err != nil {
		return nil, err
	}

	workingDirectory, err := workingDirectory(config)
	if err != nil {
		return nil, err
	}
	if config.PluginRoot != "" {
		for _, variable := range config.EnvironmentOverrides {
			environment = setEnvironment(environment, variable.Name, variable.Value)
		}
		environment = setEnvironment(environment, "PLUGIN_ROOT", config.PluginRoot)
		environment = setEnvironment(environment, "PLUGIN_DATA", config.PluginData)
	}

	commandValue := exec.CommandContext( //nolint:gosec // Absolute regular executable and args validated above; no shell.
		context.Background(),
		command,
		arguments...,
	)
	commandValue.Dir = workingDirectory
	commandValue.Env = environment

	removePrivate = false

	return &Resource{
		transport: &validatedCommandTransport{
			commandTransport: &SDK.CommandTransport{
				Command: commandValue, TerminateDuration: terminateDuration,
			},
			configuredCommand: config.Command,
			pluginRoot:        config.PluginRoot, pluginData: config.PluginData,
			workingDirectory: config.WorkingDirectory,
			environment:      slices.Clone(config.EnvironmentOverrides),
		},
		privateDir: privateDir,
		private:    privateInfo,
	}, nil
}

//nolint:gocyclo // Native and Agent Plugin directory contracts intentionally fail closed here.
func workingDirectory(config Config) (string, error) {
	if config.PluginRoot == "" && config.PluginData == "" && config.WorkingDirectory == "" {
		return config.Workspace.Root(), nil
	}
	if config.PluginRoot == "" || config.PluginData == "" || config.WorkingDirectory == "" {
		return "", errors.New("coding mcp stdio: incomplete Agent Plugin paths")
	}

	for _, variable := range config.EnvironmentOverrides {
		if variable.Name == "" || strings.ContainsAny(variable.Name, "=\x00") ||
			strings.ContainsRune(variable.Value, '\x00') ||
			variable.Name == "PLUGIN_ROOT" || variable.Name == "PLUGIN_DATA" {
			return "", errors.New("coding mcp stdio: reserved Agent Plugin environment override")
		}
	}

	root, err := validateDirectory(config.PluginRoot)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: plugin root: %w", err)
	}
	data, err := validateDirectory(config.PluginData)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: plugin data: %w", err)
	}
	dataInfo, err := os.Stat(data)
	if err != nil || !execution.IsOwnerWritableDirectory(dataInfo) {
		return "", errors.New("coding mcp stdio: plugin data is not writable")
	}
	if err := execution.ValidateDirectoryWritable(data); err != nil {
		return "", errors.New("coding mcp stdio: plugin data is not writable")
	}
	directory, err := validateDirectory(config.WorkingDirectory)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: working directory: %w", err)
	}
	if !pathContains(root, directory) && !pathContains(data, directory) {
		return "", errors.New("coding mcp stdio: working directory escapes plugin roots")
	}

	return directory, nil
}

func validateDirectory(directory string) (string, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("path must be clean and absolute")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	if resolved != directory {
		return "", errors.New("path must be filesystem-resolved")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}

	return resolved, nil
}

func setEnvironment(environment []string, name, value string) []string {
	filtered := environment[:0]
	for _, entry := range environment {
		entryName, _, exists := strings.Cut(entry, "=")
		if !exists || !execution.EnvironmentNamesEqual(entryName, name) {
			filtered = append(filtered, entry)
		}
	}
	filtered = append(filtered, name+"="+value)
	slices.Sort(filtered)

	return filtered
}

// Transport returns the official SDK transport prepared by NewTransport.
func (r *Resource) Transport() SDK.Transport {
	if r == nil {
		return nil
	}

	return r.transport
}

// Command returns a snapshot of the prepared shell-free command for internal
// inspection. Agent Plugin paths are validated again immediately before start.
func (r *Resource) Command() *exec.Cmd {
	if r == nil || r.transport == nil || r.transport.commandTransport == nil {
		return nil
	}

	command := *r.transport.commandTransport.Command
	command.Args = slices.Clone(command.Args)
	command.Env = slices.Clone(command.Env)

	return &command
}

// PrivateDir returns the owned private directory for lifecycle diagnostics.
func (r *Resource) PrivateDir() string {
	if r == nil {
		return ""
	}

	return r.privateDir
}

// Close removes the owned private directory and is idempotent.
func (r *Resource) Close() error {
	if r == nil {
		return nil
	}

	r.closeOnce.Do(func() {
		r.closeErr = removePrivateDirectory(r.privateDir, r.private)
	})

	return r.closeErr
}

func validateWorkspace(ws workspace.Workspace) error {
	if ws.Root() == "" || ws.Identity().Key() == "" {
		return errors.New("coding mcp stdio: invalid Workspace")
	}

	current, err := workspace.Open(ws.Root())
	if err != nil || current.Identity().Key() != ws.Identity().Key() {
		return errors.New("coding mcp stdio: Workspace identity changed")
	}

	return nil
}

func validateCommand(command, pluginRoot string) (string, error) {
	switch {
	case pluginRoot == "":
		return validateAbsoluteCommand(command)
	case filepath.IsAbs(command):
		return resolveBundledCommand(command, pluginRoot)
	default:
		return resolveBareCommand(command)
	}
}

func resolveBareCommand(command string) (string, error) {
	if command == "" || strings.ContainsAny(command, "/\\\x00") {
		return "", errors.New("coding mcp stdio: invalid bare Agent Plugin command")
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: resolve bare command: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: resolve bare command: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: resolve bare command: %w", err)
	}

	return validateAbsoluteCommand(resolved)
}

func resolveBundledCommand(command, pluginRoot string) (string, error) {
	root, err := validateDirectory(pluginRoot)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: plugin root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(command)
	if err != nil || !pathContains(root, resolved) {
		return "", errors.New("coding mcp stdio: bundled command escapes plugin root")
	}

	return validateAbsoluteCommand(resolved)
}

func validateAbsoluteCommand(command string) (string, error) {
	if !filepath.IsAbs(command) || filepath.Clean(command) != command {
		return "", errors.New("coding mcp stdio: command must be a clean absolute path")
	}
	info, err := os.Lstat(command)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: inspect command: %w", err)
	}

	if !execution.IsExecutableFile(info) {
		return "", errors.New("coding mcp stdio: command is not a regular executable")
	}

	return command, nil
}

func validateArguments(arguments []string) ([]string, error) {
	if len(arguments) > 256 {
		return nil, errors.New("coding mcp stdio: too many arguments")
	}

	values := slices.Clone(arguments)
	total := 0

	for _, argument := range values {
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, 0) {
			return nil, errors.New("coding mcp stdio: malformed argument")
		}

		total += len(argument)
		if total > 256<<10 {
			return nil, errors.New("coding mcp stdio: arguments exceed byte limit")
		}
	}

	return values, nil
}

func validateTempRoot(ws workspace.Workspace, root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || pathContains(ws.Root(), root) {
		return "", errors.New("coding mcp stdio: temp root must be clean, absolute, and outside the Workspace")
	}

	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: inspect temp root: %w", err)
	}

	if !execution.IsOwnerPrivateDirectory(info) {
		return "", errors.New("coding mcp stdio: temp root must be an owner-private directory")
	}

	return root, nil
}

func removePrivateDirectory(directory string, expected fs.FileInfo) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("coding mcp stdio: inspect private directory: %w", err)
	}

	if !info.IsDir() || !os.SameFile(expected, info) {
		return errors.New("coding mcp stdio: private directory identity changed")
	}

	quarantine := directory + ".closing"
	if _, err := os.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("coding mcp stdio: cleanup quarantine already exists")
	}

	if err := os.Rename(directory, quarantine); err != nil {
		return fmt.Errorf("coding mcp stdio: quarantine private directory: %w", err)
	}

	quarantined, err := os.Lstat(quarantine)
	if err != nil || !quarantined.IsDir() || !os.SameFile(expected, quarantined) {
		return errors.New("coding mcp stdio: quarantined directory identity changed")
	}

	if err := os.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("coding mcp stdio: remove private directory: %w", err)
	}

	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}

	return relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
