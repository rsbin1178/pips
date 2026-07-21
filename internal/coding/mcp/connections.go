package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	agentmcp "github.com/rsbin/pips/agent/mcp"
	"github.com/rsbin/pips/internal/coding/execution/mcpstdio"
	"github.com/rsbin/pips/internal/coding/workspace"
)

// TransportFactory prepares one definition transport and optional resource
// that must be closed after its MCP client.
type TransportFactory func(context.Context, Definition) (sdk.Transport, io.Closer, error)

// ConnectionOptions configure concrete MCP transports and client bounds.
type ConnectionOptions struct {
	Workspace      workspace.Workspace
	Implementation *sdk.Implementation
	HTTPClient     *http.Client
	TempRoot       string
	Environment    func(string) (string, bool)
	TerminateAfter time.Duration
	MaxTools       int
	Transport      TransportFactory
}

// ConnectionDiagnostic is a safe, non-fatal server lifecycle condition.
type ConnectionDiagnostic struct {
	ServerID string
	Stage    string
	Code     string
	Message  string
}

type ownedConnection struct {
	client   *agentmcp.Client
	resource io.Closer
}

// Connections owns successful MCP clients and one atomic Registry. Individual
// server failures are diagnostics and do not disable unrelated servers.
type Connections struct {
	registry *agentmcp.Registry
	owned    []ownedConnection

	mu            sync.Mutex
	diagnostics   []ConnectionDiagnostic
	refreshFailed bool
	closeOnce     sync.Once
	closeErr      error
}

// OpenConnections connects enabled definitions, primes each complete tool
// snapshot, and installs the initial Registry generation.
//
//nolint:gocyclo // Each lifecycle stage has an independent per-server diagnostic and cleanup path.
func OpenConnections(
	ctx context.Context,
	resolved []ResolvedDefinition,
	options ConnectionOptions,
) (*Connections, error) {
	if options.Implementation == nil || options.Implementation.Name == "" ||
		options.Implementation.Version == "" || options.MaxTools <= 0 {
		return nil, fmt.Errorf("%w: incomplete MCP connection options", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	factory, err := connectionTransportFactory(options, resolved)
	if err != nil {
		return nil, err
	}

	connections := &Connections{}
	servers := make([]agentmcp.RegistryServer, 0, len(resolved))

	for _, selected := range resolved {
		if selected.Status != StatusEnabled {
			continue
		}

		definition := cloneDefinition(selected.Definition)
		if err := validateDefinition(definition); err != nil {
			_ = connections.Close()

			return nil, fmt.Errorf("%w: enabled server %q: %w", ErrInvalid, definition.ID, err)
		}

		transport, resource, transportErr := factory(ctx, definition)
		if transportErr != nil {
			connections.addDiagnostic(connectionDiagnostic(
				definition.ID,
				"transport",
				"transport_failed",
				"server transport could not be prepared",
			))

			continue
		}

		connectCtx, cancel := context.WithTimeout(ctx, definition.ConnectTimeout)
		client, connectErr := agentmcp.Connect(
			connectCtx,
			options.Implementation,
			transport,
			agentmcp.WithToolNamePrefix(definition.ID),
			agentmcp.WithMaxTools(options.MaxTools),
		)

		cancel()

		if connectErr != nil {
			_ = closeConnection(nil, resource)

			connections.addDiagnostic(connectionDiagnostic(
				definition.ID,
				"connect",
				"connect_failed",
				"server connection could not be initialized",
			))

			continue
		}

		listCtx, listCancel := context.WithTimeout(ctx, definition.ConnectTimeout)
		tools, listErr := client.Tools(listCtx)

		listCancel()

		if listErr != nil {
			_ = closeConnection(client, resource)

			connections.addDiagnostic(connectionDiagnostic(
				definition.ID,
				"list",
				"list_failed",
				"server tools could not be listed",
			))

			continue
		}

		connections.owned = append(connections.owned, ownedConnection{
			client: client, resource: resource,
		})
		servers = append(servers, agentmcp.RegistryServer{
			ID: definition.ID,
			Source: &primedSource{
				client: client,
				tools:  slices.Clone(tools),
			},
			Risk: catalog.RiskPrivileged,
		})
	}

	registry, err := agentmcp.NewRegistry(servers...)
	if err != nil {
		return nil, errors.Join(err, connections.Close())
	}

	if _, err := registry.Refresh(ctx); err != nil {
		return nil, errors.Join(err, connections.Close())
	}

	connections.registry = registry

	return connections, nil
}

// Registry returns the initialized MCP Registry.
func (c *Connections) Registry() *agentmcp.Registry {
	if c == nil {
		return nil
	}

	return c.registry
}

// Snapshot returns the latest successfully installed Registry snapshot.
func (c *Connections) Snapshot() agentmcp.RegistrySnapshot {
	if c == nil || c.registry == nil {
		return agentmcp.RegistrySnapshot{}
	}

	return c.registry.Snapshot()
}

// Diagnostics returns safe lifecycle diagnostics in occurrence order.
func (c *Connections) Diagnostics() []ConnectionDiagnostic {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.diagnostics)
}

// RefreshChanged refreshes only at an application-selected safe boundary.
// A failed refresh preserves the prior snapshot and emits one coalesced
// diagnostic until a later successful attempt.
func (c *Connections) RefreshChanged(
	ctx context.Context,
) (agentmcp.RegistrySnapshot, bool, error) {
	if c == nil || c.registry == nil {
		return agentmcp.RegistrySnapshot{}, false, errors.New("coding mcp: connections not initialized")
	}

	snapshot, attempted, err := c.registry.RefreshChanged(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		if !c.refreshFailed {
			c.diagnostics = append(c.diagnostics, connectionDiagnostic(
				"registry",
				"refresh",
				"refresh_failed",
				"MCP tool refresh failed; the previous snapshot remains active",
			))
		}

		c.refreshFailed = true

		return snapshot, attempted, err
	}

	if attempted {
		c.refreshFailed = false
	}

	return snapshot, attempted, nil
}

// Close closes successful clients and their resources in reverse order.
func (c *Connections) Close() error {
	if c == nil {
		return nil
	}

	c.closeOnce.Do(func() {
		errs := make([]error, 0, len(c.owned)*2)

		for _, connection := range slices.Backward(c.owned) {
			if err := closeConnection(connection.client, connection.resource); err != nil {
				errs = append(errs, err)
			}
		}

		c.closeErr = errors.Join(errs...)
	})

	return c.closeErr
}

//nolint:gocyclo // Transport dependencies are validated only for enabled transport kinds.
func connectionTransportFactory(
	options ConnectionOptions,
	resolved []ResolvedDefinition,
) (TransportFactory, error) {
	if options.Transport != nil {
		return options.Transport, nil
	}

	needsStdio := false
	needsHTTP := false

	for _, selected := range resolved {
		if selected.Status != StatusEnabled {
			continue
		}

		switch selected.Definition.Transport {
		case TransportStdio:
			needsStdio = true
		case TransportStreamableHTTP:
			needsHTTP = true
		}
	}

	if needsStdio && (options.Workspace.Root() == "" || options.TempRoot == "" || options.Environment == nil) {
		return nil, fmt.Errorf("%w: stdio transport dependencies are missing", ErrInvalid)
	}

	if needsHTTP && (options.HTTPClient == nil || options.HTTPClient.Timeout <= 0) {
		return nil, fmt.Errorf("%w: HTTP transport requires a bounded client", ErrInvalid)
	}

	return func(_ context.Context, definition Definition) (sdk.Transport, io.Closer, error) {
		switch definition.Transport {
		case TransportStdio:
			resource, err := mcpstdio.NewTransport(mcpstdio.Config{
				Workspace:         options.Workspace,
				Command:           definition.Command,
				Args:              definition.Args,
				TempRoot:          options.TempRoot,
				Environment:       options.Environment,
				TerminateDuration: options.TerminateAfter,
			})
			if err != nil {
				return nil, nil, err
			}

			return resource.Transport(), resource, nil
		case TransportStreamableHTTP:
			return &sdk.StreamableClientTransport{
				Endpoint:   definition.URL,
				HTTPClient: options.HTTPClient,
			}, nil, nil
		default:
			return nil, nil, fmt.Errorf("%w: unsupported transport", ErrInvalid)
		}
	}, nil
}

func (c *Connections) addDiagnostic(diagnostic ConnectionDiagnostic) {
	c.mu.Lock()
	c.diagnostics = append(c.diagnostics, diagnostic)
	c.mu.Unlock()
}

func connectionDiagnostic(serverID, stage, code, message string) ConnectionDiagnostic {
	return ConnectionDiagnostic{ServerID: serverID, Stage: stage, Code: code, Message: message}
}

func closeConnection(client *agentmcp.Client, resource io.Closer) error {
	var clientErr error
	if client != nil {
		clientErr = client.Close()
	}

	var resourceErr error
	if resource != nil {
		resourceErr = resource.Close()
	}

	return errors.Join(clientErr, resourceErr)
}

type primedSource struct {
	client *agentmcp.Client

	mu    sync.Mutex
	tools []agent.Tool
}

func (s *primedSource) Tools(ctx context.Context) ([]agent.Tool, error) {
	s.mu.Lock()
	if s.tools != nil {
		tools := slices.Clone(s.tools)
		s.tools = nil
		s.mu.Unlock()

		return tools, nil
	}
	s.mu.Unlock()

	return s.client.Tools(ctx)
}

func (s *primedSource) ToolListChanged() <-chan struct{} {
	return s.client.ToolListChanged()
}
