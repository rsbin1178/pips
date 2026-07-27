package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/rsbin/pips/internal/jsonx"
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

// List returns definitions in user-file then project-file order.
func (d Definitions) List() []Definition {
	values := make([]Definition, len(d.values))
	for index, definition := range d.values {
		values[index] = cloneDefinition(definition)
	}

	return values
}

// LoadDefinitions decodes strict user and trusted-project MCP definition files.
// An untrusted project file is never inspected.
//
//nolint:gocyclo // User/project trust gates and collision checks are one ordered load boundary.
func LoadDefinitions(ctx context.Context, options LoadOptions) (Definitions, error) {
	if options.Paths.Root() == "" || options.Limits.MaxFileBytes <= 0 || options.Limits.MaxServers <= 0 {
		return Definitions{}, fmt.Errorf("%w: invalid load options", ErrInvalid)
	}

	if options.ProjectTrusted && options.Tree == nil {
		return Definitions{}, fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return Definitions{}, err
	}

	userData, userExists, err := readUserFile(options.Paths.MCPFile(), options.Limits.MaxFileBytes)
	if err != nil {
		return Definitions{}, err
	}

	values := make([]Definition, 0)

	if userExists {
		decoded, decodeErr := decodeDefinitions(userData, ScopeUser, options.Limits)
		if decodeErr != nil {
			return Definitions{}, fmt.Errorf("coding mcp: decode user definitions: %w", decodeErr)
		}

		values = append(values, decoded...)
	}

	if options.ProjectTrusted {
		projectData, projectExists, readErr := readProjectFile(
			options.Tree,
			paths.ProjectMCPFile(),
			options.Limits.MaxFileBytes,
		)
		if readErr != nil {
			return Definitions{}, readErr
		}

		if projectExists {
			decoded, decodeErr := decodeDefinitions(projectData, ScopeProject, options.Limits)
			if decodeErr != nil {
				return Definitions{}, fmt.Errorf("coding mcp: decode project definitions: %w", decodeErr)
			}

			values = append(values, decoded...)
		}
	}

	if len(values) > options.Limits.MaxServers {
		return Definitions{}, fmt.Errorf("%w: more than %d servers", ErrLimitExceeded, options.Limits.MaxServers)
	}

	seen := make(map[string]Scope, len(values))
	for _, definition := range values {
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
	}

	return Definitions{values: values}, nil
}

type definitionFile struct {
	Schema  string             `json:"schema"`
	Servers []definitionServer `json:"servers"`
}

type definitionServer struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Command        string   `json:"command,omitempty"`
	Args           []string `json:"args,omitempty"`
	URL            string   `json:"url,omitempty"`
	ConnectTimeout string   `json:"connect_timeout,omitempty"`
}

func decodeDefinitions(data []byte, scope Scope, limits Limits) ([]Definition, error) {
	var file definitionFile
	if err := jsonx.Decode(data, &file); err != nil {
		return nil, err
	}

	if file.Schema != DefinitionSchema {
		return nil, fmt.Errorf("%w: unsupported schema %q", ErrInvalid, file.Schema)
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

		timeout := 10 * time.Second

		if server.ConnectTimeout != "" {
			parsed, err := time.ParseDuration(server.ConnectTimeout)
			if err != nil {
				return nil, fmt.Errorf("%w: server %d connect timeout", ErrInvalid, index)
			}

			timeout = parsed
		}

		definition := Definition{
			ID: server.ID, Scope: scope, Transport: TransportType(server.Type),
			Command: server.Command, Args: slices.Clone(server.Args),
			URL: server.URL, ConnectTimeout: timeout,
		}
		if err := validateDefinition(definition); err != nil {
			return nil, fmt.Errorf("%w: server %d (%q): %w", ErrInvalid, index, server.ID, err)
		}

		seen[server.ID] = struct{}{}

		values = append(values, definition)
	}

	return values, nil
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
