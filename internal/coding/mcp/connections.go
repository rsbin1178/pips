//nolint:wsl_v5 // Connection lifecycle and redirect checks keep ownership steps adjacent.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
	Diagnostics    []ConnectionDiagnostic
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

type connectedServer struct {
	fingerprint string
	visibility  Visibility
}

// ConnectedServer is a detached, credential-free view of one successfully
// connected server in the current Registry snapshot.
type ConnectedServer struct {
	ID          string
	Fingerprint string
	Visibility  Visibility
	Entries     []catalog.Entry
}

// Connections owns successful MCP clients and one atomic Registry. Individual
// server failures are diagnostics and do not disable unrelated servers.
type Connections struct {
	registry *agentmcp.Registry
	owned    []ownedConnection
	servers  map[string]connectedServer

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

	connections := &Connections{
		diagnostics: slices.Clone(options.Diagnostics),
		servers:     make(map[string]connectedServer),
	}
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
		connections.servers[definition.ID] = connectedServer{
			fingerprint: definition.Fingerprint(),
			visibility:  definition.effectiveVisibility(),
		}
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

// Entries returns a detached current catalog filtered by configured
// visibility. Unknown visibility values return no entries.
func (c *Connections) Entries(visibility Visibility) []catalog.Entry {
	if c == nil || (visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate) {
		return nil
	}

	entries := c.Snapshot().Entries
	selected := make([]catalog.Entry, 0, len(entries))
	for _, entry := range entries {
		server, exists := c.servers[entry.Source.ID]
		if !exists || server.visibility != visibility {
			continue
		}
		selected = append(selected, cloneEntry(entry))
	}

	return selected
}

// ConnectedServers returns credential-free bindings for successfully
// connected servers. Entry snapshots reflect the latest successfully
// installed Registry generation.
func (c *Connections) ConnectedServers(visibility Visibility) []ConnectedServer {
	if c == nil || (visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate) {
		return nil
	}

	byID := make(map[string][]catalog.Entry)
	for _, entry := range c.Entries(visibility) {
		byID[entry.Source.ID] = append(byID[entry.Source.ID], cloneEntry(entry))
	}
	values := make([]ConnectedServer, 0, len(c.servers))
	for id, server := range c.servers {
		if server.visibility != visibility {
			continue
		}
		values = append(values, ConnectedServer{
			ID: id, Fingerprint: server.fingerprint, Visibility: server.visibility,
			Entries: cloneEntries(byID[id]),
		})
	}
	slices.SortFunc(values, func(left, right ConnectedServer) int {
		return strings.Compare(left.ID, right.ID)
	})

	return values
}

func cloneEntries(values []catalog.Entry) []catalog.Entry {
	cloned := make([]catalog.Entry, len(values))
	for index, value := range values {
		cloned[index] = cloneEntry(value)
	}

	return cloned
}

func cloneEntry(value catalog.Entry) catalog.Entry {
	value.Tags = slices.Clone(value.Tags)

	return value
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
				Workspace:            options.Workspace,
				Command:              definition.Command,
				Args:                 definition.Args,
				TempRoot:             options.TempRoot,
				Environment:          options.Environment,
				EnvironmentOverrides: definition.Environment,
				WorkingDirectory:     definition.WorkingDir,
				PluginRoot:           definition.PluginRoot,
				PluginData:           definition.PluginData,
				TerminateDuration:    options.TerminateAfter,
			})
			if err != nil {
				return nil, nil, err
			}

			return resource.Transport(), resource, nil
		case TransportStreamableHTTP:
			httpClient, err := definitionHTTPClient(options.HTTPClient, definition)
			if err != nil {
				return nil, nil, err
			}

			return &sdk.StreamableClientTransport{
				Endpoint:   definition.URL,
				HTTPClient: httpClient,
			}, nil, nil
		default:
			return nil, nil, fmt.Errorf("%w: unsupported transport", ErrInvalid)
		}
	}, nil
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers []HTTPHeader
	origin  *url.URL
}

func (t headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	if sameOrigin(t.origin, cloned.URL) {
		for _, header := range t.headers {
			if !hasHeader(cloned.Header, header.Name) {
				cloned.Header.Set(header.Name, header.Value)
			}
		}
	}

	return t.base.RoundTrip(cloned)
}

func hasHeader(headers http.Header, name string) bool {
	for existing := range headers {
		if strings.EqualFold(existing, name) {
			return true
		}
	}

	return false
}

func definitionHTTPClient(base *http.Client, definition Definition) (*http.Client, error) {
	if base == nil {
		return nil, fmt.Errorf("%w: nil HTTP client", ErrInvalid)
	}

	endpoint, err := url.Parse(definition.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse HTTP endpoint", ErrInvalid)
	}
	client := *base
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = headerRoundTripper{
		base: transport, headers: slices.Clone(definition.Headers), origin: endpoint,
	}
	if len(definition.Headers) == 0 {
		return &client, nil
	}

	previousCheck := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !sameOrigin(endpoint, request.URL) {
			return errors.New("coding mcp: configured headers cannot cross origins")
		}
		if previousCheck != nil {
			return previousCheck(request, via)
		}
		if len(via) >= 10 {
			return errors.New("coding mcp: stopped after 10 redirects")
		}

		return nil
	}

	return &client, nil
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}

	return "80"
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
