// Package mcpstdio constructs trusted, unsandboxed MCP stdio transports with
// the Coding Agent's minimal child environment and owned private directories.
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

// Config defines one absolute, shell-free stdio server process.
type Config struct {
	Workspace         workspace.Workspace
	Command           string
	Args              []string
	TempRoot          string
	Environment       func(string) (string, bool)
	TerminateDuration time.Duration
}

// Resource owns a prepared SDK transport and its private environment root.
// Close it only after the MCP Client has closed its process connection.
type Resource struct {
	transport  *SDK.CommandTransport
	privateDir string
	private    fs.FileInfo

	closeOnce sync.Once
	closeErr  error
}

// NewTransport validates and prepares one MCP stdio transport. It does not
// start the command.
func NewTransport(config Config) (*Resource, error) {
	if err := validateWorkspace(config.Workspace); err != nil {
		return nil, err
	}

	command, err := validateCommand(config.Command)
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

	environment, err := execution.NewChildEnvironment(config.Environment, privateDir, nil)
	if err != nil {
		return nil, err
	}

	commandValue := exec.CommandContext( //nolint:gosec // Absolute regular executable and args validated above; no shell.
		context.Background(),
		command,
		arguments...,
	)
	commandValue.Dir = config.Workspace.Root()
	commandValue.Env = environment

	removePrivate = false

	return &Resource{
		transport: &SDK.CommandTransport{
			Command:           commandValue,
			TerminateDuration: terminateDuration,
		},
		privateDir: privateDir,
		private:    privateInfo,
	}, nil
}

// Transport returns the official SDK transport prepared by NewTransport.
func (r *Resource) Transport() SDK.Transport {
	if r == nil {
		return nil
	}

	return r.transport
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

func validateCommand(command string) (string, error) {
	if !filepath.IsAbs(command) || filepath.Clean(command) != command {
		return "", errors.New("coding mcp stdio: command must be a clean absolute path")
	}

	info, err := os.Lstat(command)
	if err != nil {
		return "", fmt.Errorf("coding mcp stdio: inspect command: %w", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
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

	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
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
