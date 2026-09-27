//nolint:wsl_v5 // MCP variant validation keeps each narrow failure boundary adjacent.
package agentplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
)

var mcpTopFields = map[string]struct{}{"$schema": {}, "mcpServers": {}}

//nolint:gocyclo // Top-level isolation and independent server-entry recovery stay explicit.
func loadMCP(
	ctx context.Context,
	root *os.Root,
	pkg Package,
	limits Limits,
) ([]codingmcp.Definition, []Diagnostic) {
	info, err := root.Stat("mcp.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "mcp", "component_invalid", "mcp.json is not a resolvable regular file",
		)}
	}
	data, err := readRootFile(root, "mcp.json", limits.MaxMCPBytes)
	if err != nil {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "mcp", "component_invalid", "mcp.json cannot be read within client limits",
		)}
	}

	object, err := decodeObject(data)
	if err != nil {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "mcp", "component_invalid", "mcp.json must be a JSON object",
		)}
	}
	for field := range object {
		if _, allowed := mcpTopFields[field]; !allowed {
			return nil, []Diagnostic{componentDiagnostic(
				pkg, "mcp", "component_invalid", "mcp.json contains an unknown top-level field",
			)}
		}
	}
	schema, err := requiredString(object, "$schema")
	if err != nil || schema != MCPSchema {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "mcp", "component_invalid", "mcp.json has an unsupported or missing $schema",
		)}
	}
	var servers map[string]json.RawMessage
	if raw, exists := object["mcpServers"]; !exists || json.Unmarshal(raw, &servers) != nil || servers == nil {
		return nil, []Diagnostic{componentDiagnostic(
			pkg, "mcp", "component_invalid", "mcpServers must be an object",
		)}
	}
	names := slices.Collect(mapsKeys(servers))
	slices.Sort(names)
	var diagnostics []Diagnostic
	if len(names) > limits.MaxMCPServers {
		diagnostics = append(diagnostics, componentDiagnostic(
			pkg, "mcp", "component_limit",
			fmt.Sprintf("mcpServers beyond the first %d were ignored", limits.MaxMCPServers),
		))
		names = names[:limits.MaxMCPServers]
	}
	definitions := make([]codingmcp.Definition, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "mcp", "load_canceled", "MCP loading was canceled",
			))
			break
		}
		definition, supported, err := decodeMCPServer(name, servers[name], pkg)
		if err != nil {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "mcp:"+name, "server_invalid", err.Error(),
			))
			continue
		}
		if !supported {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "mcp:"+name, "transport_unsupported", "legacy HTTP+SSE is not supported",
			))
			continue
		}
		if _, err := codingmcp.NewDefinitions([]codingmcp.Definition{definition}, 1); err != nil {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "mcp:"+name, "server_invalid", "server failed native validation",
			))
			continue
		}
		if _, duplicate := seen[definition.ID]; duplicate {
			diagnostics = append(diagnostics, componentDiagnostic(
				pkg, "mcp:"+name, "server_duplicate",
				fmt.Sprintf("server name resolves to the same ID %q as an earlier server", definition.ID),
			))
			continue
		}
		seen[definition.ID] = struct{}{}
		definitions = append(definitions, definition)
	}

	return definitions, diagnostics
}

func decodeMCPServer(
	name string,
	raw json.RawMessage,
	pkg Package,
) (codingmcp.Definition, bool, error) {
	entry, err := codingmcp.DecodeServerEntry(raw, codingmcp.DialectAgentPlugin)
	if errors.Is(err, codingmcp.ErrUnsupportedTransport) {
		return codingmcp.Definition{}, false, nil
	}
	if err != nil {
		return codingmcp.Definition{}, false, err
	}
	if entry.Transport == codingmcp.TransportStdio {
		definition, err := resolveStdioServer(name, entry, pkg)

		return definition, true, err
	}

	return codingmcp.Definition{
		ID: codingmcp.ServerID(name), Scope: codingmcp.ScopeAgentPlugin,
		Transport: codingmcp.TransportStreamableHTTP, URL: entry.URL,
		Headers: entry.Headers, ConnectTimeout: 10 * time.Second,
	}, true, nil
}

// resolveStdioServer applies the Agent Plugin-only parts of a stdio entry:
// plugin-relative commands, PLUGIN_ROOT/PLUGIN_DATA placeholders, and the
// plugin-rooted working directory.
func resolveStdioServer(
	name string,
	entry codingmcp.ServerEntry,
	pkg Package,
) (codingmcp.Definition, error) {
	command, err := resolveCommand(pkg.Root, entry.Command)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	data, err := preparePluginData(pkg.Data)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	args := slices.Clone(entry.Args)
	for index := range args {
		args[index] = expandPluginVariables(args[index], pkg.Root, data)
	}

	environment := slices.Clone(entry.Environment)
	for index := range environment {
		environment[index].Value = expandPluginVariables(environment[index].Value, pkg.Root, data)
	}

	var configuredDirectory *string
	if entry.HasWorkingDir {
		configuredDirectory = &entry.WorkingDir
	}
	workingDirectory, err := resolveWorkingDirectory(configuredDirectory, pkg.Root, data)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	return codingmcp.Definition{
		ID: codingmcp.ServerID(name), Scope: codingmcp.ScopeAgentPlugin,
		Transport: codingmcp.TransportStdio, Command: command, Args: args,
		Environment: environment, WorkingDir: workingDirectory,
		PluginRoot: pkg.Root, PluginData: data, ConnectTimeout: 10 * time.Second,
	}, nil
}

func preparePluginData(directory string) (string, error) {
	base := filepath.Dir(directory)
	if err := securePluginDataDirectory(base, true); err != nil {
		return "", errors.New("PLUGIN_DATA base is not a private client-owned directory")
	}
	if err := securePluginDataDirectory(directory, false); err != nil {
		return "", errors.New("PLUGIN_DATA is not a private client-owned directory")
	}

	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", errors.New("PLUGIN_DATA base cannot be resolved")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || !pathContains(resolvedBase, resolved) {
		return "", errors.New("PLUGIN_DATA cannot be resolved inside its base")
	}
	if err := execution.ValidateDirectoryWritable(resolved); err != nil {
		return "", errors.New("PLUGIN_DATA is not writable to the subprocess identity")
	}

	return resolved, nil
}

func securePluginDataDirectory(directory string, parents bool) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if parents {
			err = os.MkdirAll(directory, 0o700)
		} else {
			err = os.Mkdir(directory, 0o700)
		}
		if err != nil {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !execution.IsOwnerPrivateDirectory(info) {
		return errors.New("directory is not private")
	}

	return os.Chmod(directory, 0o700) //nolint:gosec // Client-managed data must be owner-writable.
}

func resolveCommand(root, token string) (string, error) {
	if relative, found := strings.CutPrefix(token, "./"); found {
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !pathContains(root, resolved) {
			return "", errors.New("plugin-relative command escapes or cannot be resolved")
		}
		if err := validateExecutable(resolved); err != nil {
			return "", err
		}

		return resolved, nil
	}
	if strings.ContainsAny(token, "/\\") {
		return "", errors.New("stdio command must be bare or begin with ./")
	}
	return token, nil
}

func validateExecutable(command string) error {
	info, err := os.Stat(command)
	if err != nil || !execution.IsExecutableFile(info) {
		return errors.New("stdio command is not a regular executable")
	}

	return nil
}

func resolveWorkingDirectory(configuredDirectory *string, root, data string) (string, error) {
	if configuredDirectory == nil {
		return root, nil
	}
	configured := *configuredDirectory
	var permittedRoot string
	switch {
	case strings.HasPrefix(configured, "./"):
		permittedRoot = root
	case configured == "${PLUGIN_ROOT}" || strings.HasPrefix(configured, "${PLUGIN_ROOT}/"):
		permittedRoot = root
	case configured == "${PLUGIN_DATA}" || strings.HasPrefix(configured, "${PLUGIN_DATA}/"):
		permittedRoot = data
	default:
		return "", errors.New("stdio cwd is not rooted by ./, PLUGIN_ROOT, or PLUGIN_DATA")
	}
	expanded := expandPluginVariables(configured, root, data)
	if strings.HasPrefix(configured, "./") {
		expanded = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(expanded, "./")))
	}
	expanded = filepath.Clean(expanded)
	resolved, err := filepath.EvalSymlinks(expanded)
	if err != nil || !pathContains(permittedRoot, resolved) {
		return "", errors.New("stdio cwd escapes or cannot be resolved")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("stdio cwd is not a directory")
	}

	return resolved, nil
}

func expandPluginVariables(value, root, data string) string {
	separator := string(filepath.Separator)

	return strings.NewReplacer(
		"${PLUGIN_ROOT}/", root+separator,
		"${PLUGIN_DATA}/", data+separator,
		"${PLUGIN_ROOT}", root,
		"${PLUGIN_DATA}", data,
	).Replace(value)
}

func pluginDataPath(base, root string) string {
	sum := sha256.Sum256([]byte(root))

	return filepath.Join(base, "ap-"+hex.EncodeToString(sum[:16]))
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}

	return relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func mapsKeys[Map ~map[Key]Value, Key comparable, Value any](value Map) func(func(Key) bool) {
	return func(yield func(Key) bool) {
		for key := range value {
			if !yield(key) {
				return
			}
		}
	}
}
