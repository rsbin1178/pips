//nolint:wsl_v5 // MCP variant validation keeps each narrow failure boundary adjacent.
package agentplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"golang.org/x/net/http/httpguts"
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
		definitions = append(definitions, definition)
	}

	return definitions, diagnostics
}

func decodeMCPServer(
	name string,
	raw json.RawMessage,
	pkg Package,
) (codingmcp.Definition, bool, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return codingmcp.Definition{}, false, errors.New("server must be an object")
	}
	transport, err := requiredString(object, "type")
	if err != nil {
		return codingmcp.Definition{}, false, errors.New("server type is required")
	}

	switch transport {
	case "stdio":
		definition, err := decodeStdioServer(name, object, pkg)
		return definition, true, err
	case "streamable-http":
		definition, err := decodeRemoteServer(name, object, pkg)
		return definition, true, err
	case "sse":
		_, err := decodeRemoteServer(name, object, pkg)
		return codingmcp.Definition{}, false, err
	default:
		return codingmcp.Definition{}, false, errors.New("unsupported server type")
	}
}

func decodeStdioServer(
	name string,
	object map[string]json.RawMessage,
	pkg Package,
) (codingmcp.Definition, error) {
	if err := rejectUnknownFields(object, "type", "command", "args", "env", "cwd"); err != nil {
		return codingmcp.Definition{}, err
	}
	commandToken, err := requiredString(object, "command")
	if err != nil || commandToken == "" {
		return codingmcp.Definition{}, errors.New("stdio command must be a non-empty string")
	}
	command, err := resolveCommand(pkg.Root, commandToken)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	data, err := preparePluginData(pkg.Data)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	var args []string
	if raw, exists := object["args"]; exists {
		if err := json.Unmarshal(raw, &args); err != nil || args == nil {
			return codingmcp.Definition{}, errors.New("stdio args must be an array of strings")
		}
	}
	for index := range args {
		args[index] = expandPluginVariables(args[index], pkg.Root, data)
	}

	environment, err := decodeEnvironment(object["env"], pkg.Root, data)
	if err != nil {
		return codingmcp.Definition{}, err
	}
	workingDirectory, err := resolveWorkingDirectory(object["cwd"], pkg.Root, data)
	if err != nil {
		return codingmcp.Definition{}, err
	}

	return codingmcp.Definition{
		ID: internalServerID(pkg.instance, name), Scope: codingmcp.ScopeAgentPlugin,
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

func decodeRemoteServer(
	name string,
	object map[string]json.RawMessage,
	pkg Package,
) (codingmcp.Definition, error) {
	if err := rejectUnknownFields(object, "type", "url", "headers"); err != nil {
		return codingmcp.Definition{}, err
	}
	endpoint, err := requiredString(object, "url")
	if err != nil {
		return codingmcp.Definition{}, errors.New("remote URL must satisfy Agent Plugins HTTP(S) rules")
	}
	endpoint, err = normalizeRemoteURL(endpoint)
	if err != nil {
		return codingmcp.Definition{}, errors.New("remote URL must satisfy Agent Plugins HTTP(S) rules")
	}
	headers, err := decodeHeaders(object["headers"])
	if err != nil {
		return codingmcp.Definition{}, err
	}

	return codingmcp.Definition{
		ID: internalServerID(pkg.instance, name), Scope: codingmcp.ScopeAgentPlugin,
		Transport: codingmcp.TransportStreamableHTTP, URL: endpoint,
		Headers: headers, ConnectTimeout: 10 * time.Second,
	}, nil
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

func decodeEnvironment(raw json.RawMessage, root, data string) ([]execution.EnvVar, error) {
	if raw == nil {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("stdio env must be an object of strings")
	}
	if _, exists := values["PLUGIN_ROOT"]; exists {
		return nil, errors.New("stdio env cannot override PLUGIN_ROOT")
	}
	if _, exists := values["PLUGIN_DATA"]; exists {
		return nil, errors.New("stdio env cannot override PLUGIN_DATA")
	}
	names := slices.Collect(mapsKeys(values))
	slices.Sort(names)
	environment := make([]execution.EnvVar, 0, len(names))
	for _, name := range names {
		environment = append(environment, execution.EnvVar{
			Name: name, Value: expandPluginVariables(values[name], root, data),
		})
	}

	return environment, nil
}

func resolveWorkingDirectory(raw json.RawMessage, root, data string) (string, error) {
	if raw == nil {
		return root, nil
	}
	var configured string
	if err := json.Unmarshal(raw, &configured); err != nil {
		return "", errors.New("stdio cwd must be a string")
	}
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
	return strings.NewReplacer("${PLUGIN_ROOT}", root, "${PLUGIN_DATA}", data).Replace(value)
}

func normalizeRemoteURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || !validRemoteURLSyntax(value, parsed) {
		return "", errors.New("invalid remote URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme == "https" {
		return parsed.String(), nil
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return parsed.String(), nil
	}
	address := net.ParseIP(host)
	if address == nil || !address.IsLoopback() {
		return "", errors.New("non-loopback HTTP URL")
	}

	return parsed.String(), nil
}

func validRemoteURLSyntax(value string, parsed *url.URL) bool {
	return !strings.ContainsRune(value, '#') && parsed.IsAbs() && parsed.Host != "" &&
		parsed.Opaque == "" && parsed.User == nil && parsed.Fragment == "" &&
		(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
}

func decodeHeaders(raw json.RawMessage) ([]codingmcp.HTTPHeader, error) {
	if raw == nil {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("headers must be an object of literal strings")
	}
	names := slices.Collect(mapsKeys(values))
	slices.Sort(names)
	seen := make(map[string]struct{}, len(names))
	headers := make([]codingmcp.HTTPHeader, 0, len(names))
	for _, name := range names {
		canonical := strings.ToLower(http.CanonicalHeaderKey(name))
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(values[name]) {
			return nil, errors.New("header name or value is invalid")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, errors.New("headers contain a case-insensitive duplicate")
		}
		seen[canonical] = struct{}{}
		headers = append(headers, codingmcp.HTTPHeader{Name: name, Value: values[name]})
	}

	return headers, nil
}

func rejectUnknownFields(object map[string]json.RawMessage, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		set[field] = struct{}{}
	}
	for field := range object {
		if _, exists := set[field]; !exists {
			return fmt.Errorf("unknown server field %q", field)
		}
	}

	return nil
}

func internalServerID(root, server string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + server))

	return "ap-" + hex.EncodeToString(sum[:12])
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
