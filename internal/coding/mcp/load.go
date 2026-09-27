package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// Limits bound MCP definition discovery and decoding.
type Limits struct {
	MaxFileBytes int64
	MaxServers   int
}

// DefaultLimits returns conservative MCP definition limits.
func DefaultLimits() Limits {
	return Limits{MaxFileBytes: 1 << 20, MaxServers: 64}
}

// LoadOptions select user and optional trusted-project MCP files.
type LoadOptions struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	Limits         Limits
}

// Definitions is an immutable validated definition set.
type Definitions struct {
	values []Definition
}

// NewDefinitions returns an immutable validated definition set.
func NewDefinitions(values []Definition, maximum int) (Definitions, error) {
	if maximum <= 0 {
		return Definitions{}, fmt.Errorf("%w: invalid definition limit", ErrInvalid)
	}
	if len(values) > maximum {
		return Definitions{}, fmt.Errorf("%w: more than %d servers", ErrLimitExceeded, maximum)
	}

	cloned := make([]Definition, 0, len(values))
	seen := make(map[string]Scope, len(values))
	for index, definition := range values {
		if err := validateDefinition(definition); err != nil {
			return Definitions{}, fmt.Errorf(
				"%w: server %d (%q): %w",
				ErrInvalid,
				index,
				definition.ID,
				err,
			)
		}
		if previous, duplicate := seen[definition.ID]; duplicate {
			return Definitions{}, fmt.Errorf(
				"%w: server %q in %s and %s scopes",
				ErrDuplicate,
				definition.ID,
				previous,
				definition.Scope,
			)
		}

		seen[definition.ID] = definition.Scope
		cloned = append(cloned, cloneDefinition(definition))
	}

	return Definitions{values: cloned}, nil
}

// Merge returns one immutable definition set and rejects cross-set collisions.
func (d Definitions) Merge(other Definitions, maximum int) (Definitions, error) {
	values := append(d.List(), other.List()...)

	return NewDefinitions(values, maximum)
}

// List returns definitions in user-file then project-file order.
func (d Definitions) List() []Definition {
	values := make([]Definition, len(d.values))
	for index, definition := range d.values {
		values[index] = cloneDefinition(definition)
	}

	return values
}

// LoadDefinitions decodes strict user and trusted-project MCP definition files.
// An untrusted project file is never inspected. File-level violations fail the
// load; a pips.mcp/v1alpha2 entry that is invalid, unsupported, or duplicates
// an earlier derived ID is skipped and reported as a diagnostic.
//
//nolint:gocyclo // User/project trust gates and collision checks are one ordered load boundary.
func LoadDefinitions(ctx context.Context, options LoadOptions) (Definitions, []ConnectionDiagnostic, error) {
	if options.Paths.Root() == "" || options.Limits.MaxFileBytes <= 0 || options.Limits.MaxServers <= 0 {
		return Definitions{}, nil, fmt.Errorf("%w: invalid load options", ErrInvalid)
	}

	if options.ProjectTrusted && options.Tree == nil {
		return Definitions{}, nil, fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return Definitions{}, nil, err
	}

	userData, userExists, err := readUserFile(options.Paths.MCPFile(), options.Limits.MaxFileBytes)
	if err != nil {
		return Definitions{}, nil, err
	}

	values := make([]Definition, 0)
	diagnostics := make([]ConnectionDiagnostic, 0)

	if userExists {
		decoded, skipped, decodeErr := decodeDefinitions(userData, ScopeUser, options.Limits)
		if decodeErr != nil {
			return Definitions{}, nil, fmt.Errorf("coding mcp: decode user definitions: %w", decodeErr)
		}

		values = append(values, decoded...)
		diagnostics = append(diagnostics, skipped...)
	}

	if options.ProjectTrusted {
		projectData, projectExists, readErr := readProjectFile(
			options.Tree,
			paths.ProjectMCPFile(),
			options.Limits.MaxFileBytes,
		)
		if readErr != nil {
			return Definitions{}, nil, readErr
		}

		if projectExists {
			decoded, skipped, decodeErr := decodeDefinitions(projectData, ScopeProject, options.Limits)
			if decodeErr != nil {
				return Definitions{}, nil, fmt.Errorf("coding mcp: decode project definitions: %w", decodeErr)
			}

			values = append(values, decoded...)
			diagnostics = append(diagnostics, skipped...)
		}
	}

	definitions, err := NewDefinitions(values, options.Limits.MaxServers)
	if err != nil {
		return Definitions{}, nil, err
	}

	return definitions, diagnostics, nil
}

func decodeDefinitions(data []byte, scope Scope, limits Limits) ([]Definition, []ConnectionDiagnostic, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		// Report the strict decoder's precise syntax error, and never let a
		// file the header probe rejected load as an empty definition set.
		var file definitionFile
		if strictErr := jsonx.Decode(data, &file); strictErr != nil {
			return nil, nil, strictErr
		}

		return nil, nil, fmt.Errorf("%w: definition file schema must be a string", ErrInvalid)
	}

	switch header.Schema {
	case DefinitionSchema:
		return decodeServers(data, scope, limits)
	case LegacyDefinitionSchema:
		values, err := decodeLegacyDefinitions(data, scope, limits)

		return values, nil, err
	default:
		return nil, nil, fmt.Errorf(
			"%w: unsupported schema %q (use %q with an mcpServers object)",
			ErrInvalid, header.Schema, DefinitionSchema,
		)
	}
}

type serversFile struct {
	Schema     string                     `json:"schema"`
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// decodeServers decodes a pips.mcp/v1alpha2 file. Entries use the same
// closed variants and field rules as Agent Plugins mcp.json.
func decodeServers(data []byte, scope Scope, limits Limits) ([]Definition, []ConnectionDiagnostic, error) {
	var file serversFile
	if err := jsonx.Decode(data, &file); err != nil {
		return nil, nil, err
	}

	if file.MCPServers == nil {
		return nil, nil, fmt.Errorf("%w: mcpServers must be an object", ErrInvalid)
	}

	if len(file.MCPServers) > limits.MaxServers {
		return nil, nil, fmt.Errorf("%w: more than %d servers", ErrLimitExceeded, limits.MaxServers)
	}

	values := make([]Definition, 0, len(file.MCPServers))
	diagnostics := make([]ConnectionDiagnostic, 0)
	seen := make(map[string]string, len(file.MCPServers))
	skip := func(name, code, reason string) {
		diagnostics = append(diagnostics, connectionDiagnostic(
			ServerID(name), "configuration", code,
			fmt.Sprintf("%s MCP server %q was ignored: %s", scope, name, reason),
		))
	}

	for _, name := range sortedKeys(file.MCPServers) {
		entry, err := DecodeServerEntry(file.MCPServers[name], DialectNative)
		if errors.Is(err, ErrUnsupportedTransport) {
			skip(name, "transport_unsupported", "legacy HTTP+SSE is not supported; use streamable-http")

			continue
		}

		if err != nil {
			skip(name, "definition_invalid", err.Error())

			continue
		}

		timeout := entry.ConnectTimeout
		if timeout == 0 {
			timeout = defaultConnectTimeout
		}

		definition := Definition{
			ID: ServerID(name), Scope: scope, Transport: entry.Transport,
			Visibility: entry.Visibility, Command: entry.Command, Args: entry.Args,
			Environment: entry.Environment, URL: entry.URL, Headers: entry.Headers,
			ConnectTimeout: timeout,
		}
		if err := validateDefinition(definition); err != nil {
			skip(name, "definition_invalid", err.Error())

			continue
		}

		if previous, duplicate := seen[definition.ID]; duplicate {
			skip(name, "definition_duplicate", fmt.Sprintf("its ID %q is already used by %q", definition.ID, previous))

			continue
		}

		seen[definition.ID] = name
		values = append(values, definition)
	}

	return values, diagnostics, nil
}

const defaultConnectTimeout = 10 * time.Second

type definitionFile struct {
	Schema  string             `json:"schema"`
	Servers []definitionServer `json:"servers"`
}

type definitionServer struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Visibility     string   `json:"visibility,omitempty"`
	Command        string   `json:"command,omitempty"`
	Args           []string `json:"args,omitempty"`
	URL            string   `json:"url,omitempty"`
	ConnectTimeout string   `json:"connect_timeout,omitempty"`
}

// decodeLegacyDefinitions decodes a pips.mcp/v1alpha1 file. Its rules are
// frozen: absolute commands, control-free arguments, no environment or
// headers, and any violation fails the whole file.
func decodeLegacyDefinitions(data []byte, scope Scope, limits Limits) ([]Definition, error) {
	var file definitionFile
	if err := jsonx.Decode(data, &file); err != nil {
		return nil, err
	}

	if len(file.Servers) > limits.MaxServers {
		return nil, fmt.Errorf("%w: more than %d servers", ErrLimitExceeded, limits.MaxServers)
	}

	values := make([]Definition, 0, len(file.Servers))
	seen := make(map[string]struct{}, len(file.Servers))

	for index, server := range file.Servers {
		if _, duplicate := seen[server.ID]; duplicate {
			return nil, fmt.Errorf("%w: server %q", ErrDuplicate, server.ID)
		}

		timeout := defaultConnectTimeout

		if server.ConnectTimeout != "" {
			parsed, err := time.ParseDuration(server.ConnectTimeout)
			if err != nil {
				return nil, fmt.Errorf("%w: server %d connect timeout", ErrInvalid, index)
			}

			timeout = parsed
		}

		definition := Definition{
			ID: server.ID, Scope: scope, Transport: TransportType(server.Type),
			Visibility: Visibility(server.Visibility),
			Command:    server.Command, Args: slices.Clone(server.Args),
			URL: server.URL, ConnectTimeout: timeout,
		}
		if err := validateLegacyDefinition(definition); err != nil {
			return nil, fmt.Errorf("%w: server %d (%q): %w", ErrInvalid, index, server.ID, err)
		}

		seen[server.ID] = struct{}{}

		values = append(values, definition)
	}

	return values, nil
}

// validateLegacyDefinition applies the frozen v1alpha1 rules. They are
// exactly the strict session-scope rules (clean absolute command, control-free
// arguments), so validating a session-scoped view keeps v1alpha1 acceptance,
// error text, and check order byte-for-byte unchanged.
func validateLegacyDefinition(definition Definition) error {
	if definition.Scope != ScopeUser && definition.Scope != ScopeProject {
		return errors.New("unsupported definition scope")
	}

	legacy := definition
	legacy.Scope = ScopeSession

	return validateDefinition(legacy)
}

func readUserFile(filePath string, maximum int64) ([]byte, bool, error) {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: inspect user definitions: %w", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, false, fmt.Errorf("%w: user MCP file", ErrUnsafeFile)
	}

	file, err := os.Open(filePath) //nolint:gosec // Lstat and SameFile bind this trusted user path.
	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: open user definitions: %w", err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, false, fmt.Errorf("%w: user MCP file changed while opening", ErrUnsafeFile)
	}

	data, err := readBounded(file, maximum)
	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: read user definitions: %w", err)
	}

	return data, true, nil
}

func readProjectFile(
	tree *workspace.Tree,
	filePath string,
	maximum int64,
) ([]byte, bool, error) {
	info, err := tree.Lstat(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: inspect project file %q: %w", filePath, err)
	}

	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: project file %q is not regular", ErrUnsafeFile, filePath)
	}

	file, err := tree.Open(filePath)
	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: open project file %q: %w", filePath, err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, false, fmt.Errorf("%w: project file %q changed while opening", ErrUnsafeFile, filePath)
	}

	data, err := readBounded(file, maximum)
	if err != nil {
		return nil, false, fmt.Errorf("coding mcp: read project file %q: %w", filePath, err)
	}

	return data, true, nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maximum {
		return nil, ErrLimitExceeded
	}

	return data, nil
}
