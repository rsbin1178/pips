//nolint:wsl_v5 // Connection lifecycle and redirect checks keep ownership steps adjacent.
package mcp

import (
	"context"
	"encoding/json"
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
	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	agentmcp "github.com/rsbin1178/pips/agent/mcp"
	"github.com/rsbin1178/pips/internal/coding/execution/mcpstdio"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
	// ReadOnlyTools marks tools, keyed by server ID, as read-only so they may
	// run concurrently within one model response. Values are remote MCP tool
	// names or ReadOnlyWildcard. Risk stays privileged; remote readOnlyHint
	// annotations are never consulted.
	ReadOnlyTools map[string][]string
}

// ReadOnlyWildcard in ConnectionOptions.ReadOnlyTools marks every tool of a
// server as read-only.
const ReadOnlyWildcard = "*"

// ConnectionDiagnostic is a safe, non-fatal server lifecycle condition.
type ConnectionDiagnostic struct {
	ServerID string
	Stage    string
	Code     string
	Message  string
}

// ServerState is the closed lifecycle state of one configured server.
type ServerState string

// Supported server lifecycle states.
const (
	// ServerStatePending means the project permission record is missing or
	// stale, so the server was never started.
	ServerStatePending ServerState = "pending"
	// ServerStateDisabled means the project permission decision is deny.
	ServerStateDisabled ServerState = "disabled"
	// ServerStateConnecting means the background connect is still running.
	ServerStateConnecting ServerState = "connecting"
	// ServerStateConnected means the server's tools are in the snapshot.
	ServerStateConnected ServerState = "connected"
	// ServerStateFailed means the server disabled only itself with a safe
	// diagnostic.
	ServerStateFailed ServerState = "failed"
)

// ServerStatus is a detached, credential-free projection of one configured
// server. It never carries URL, headers, command, args, environment, or
// working directory.
type ServerStatus struct {
	ID         string        `json:"id"`
	Scope      Scope         `json:"scope"`
	Transport  TransportType `json:"transport"`
	Visibility Visibility    `json:"visibility"`
	State      ServerState   `json:"state"`
	// Tools lists the advertised (prefixed) tool names of a connected server.
	Tools []string `json:"tools,omitempty"`
	// Stage, Code, and Message describe a failed server; they hold the same
	// fixed safe text as the matching ConnectionDiagnostic.
	Stage   string `json:"stage,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// StartedAt is zero for pending and disabled servers. SettledAt is zero
	// while the server is still connecting.
	StartedAt time.Time `json:"started_at,omitzero"`
	SettledAt time.Time `json:"settled_at,omitzero"`
}

var errConnectionsClosed = errors.New("coding mcp: connections closed")

// ConnectedServer is a detached, credential-free view of one successfully
// connected server in the current snapshot.
type ConnectedServer struct {
	ID          string
	Fingerprint string
	Visibility  Visibility
	Entries     []catalog.Entry
}

type managedServer struct {
	id             string
	fingerprint    string
	scope          Scope
	transport      TransportType
	visibility     Visibility
	connectTimeout time.Duration
	definition     Definition

	state     ServerState
	stage     string
	code      string
	message   string
	startedAt time.Time
	settledAt time.Time
	reported  bool

	client   *agentmcp.Client
	resource io.Closer
	tools    []agent.Tool
}

// Connections owns MCP clients and one atomic, versioned tool snapshot. Every
// enabled server connects concurrently in the background: a server joins the
// snapshot as soon as it settles, and individual failures are diagnostics that
// do not disable unrelated servers.
type Connections struct {
	mu            sync.Mutex
	servers       []*managedServer
	snapshot      agentmcp.RegistrySnapshot
	signature     string
	options       []ConnectionDiagnostic
	optionsTaken  bool
	refresh       []ConnectionDiagnostic
	refreshFailed bool
	pending       int
	settled       chan struct{}
	closed        bool
	readOnly      map[string]map[string]struct{}

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// OpenConnections validates options and every enabled definition, installs an
// empty initial snapshot, and starts one background connect per enabled
// server. It returns before any server has connected; callers that need the
// complete set use Wait.
//
// Background connects derive from a detached copy of ctx so a request-scoped
// caller context cannot abort them; Close is the only stop.
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

	servers, err := newManagedServers(resolved)
	if err != nil {
		return nil, err
	}

	background, cancel := context.WithCancel(context.WithoutCancel(ctx))
	connections := &Connections{
		servers:  servers,
		options:  slices.Clone(options.Diagnostics),
		settled:  make(chan struct{}),
		cancel:   cancel,
		snapshot: agentmcp.RegistrySnapshot{Version: 1, UpdatedAt: time.Now().UTC()},
		readOnly: readOnlySets(options.ReadOnlyTools),
	}
	connections.signature, err = entriesSignature(nil)
	if err != nil {
		cancel()

		return nil, err
	}

	connections.start(background, factory, options)

	return connections, nil
}

// start marks every enabled server as connecting and launches its background
// connect. With nothing enabled the connection set settles immediately.
func (c *Connections) start(
	ctx context.Context,
	factory TransportFactory,
	options ConnectionOptions,
) {
	now := time.Now().UTC()
	for _, server := range c.servers {
		if server.state != ServerStateConnecting {
			continue
		}
		server.startedAt = now
		c.pending++
	}
	if c.pending == 0 {
		close(c.settled)

		return
	}

	for _, server := range c.servers {
		if server.state != ServerStateConnecting {
			continue
		}
		c.wg.Add(1)
		go c.connect(ctx, server, factory, options)
	}
}

func newManagedServers(resolved []ResolvedDefinition) ([]*managedServer, error) {
	servers := make([]*managedServer, 0, len(resolved))
	for _, selected := range resolved {
		definition := cloneDefinition(selected.Definition)
		server := &managedServer{
			id: definition.ID, fingerprint: definition.Fingerprint(),
			scope: definition.Scope, transport: definition.Transport,
			visibility:     definition.effectiveVisibility(),
			connectTimeout: definition.ConnectTimeout,
			definition:     definition,
		}
		switch selected.Status {
		case StatusEnabled:
			if err := validateDefinition(definition); err != nil {
				return nil, fmt.Errorf("%w: enabled server %q: %w", ErrInvalid, definition.ID, err)
			}
			server.state = ServerStateConnecting
		case StatusDisabled:
			server.state = ServerStateDisabled
		case StatusPending:
			server.state = ServerStatePending
		default:
			return nil, fmt.Errorf("%w: server %q has unknown status", ErrInvalid, definition.ID)
		}
		servers = append(servers, server)
	}

	return servers, nil
}

func (c *Connections) connect(
	ctx context.Context,
	server *managedServer,
	factory TransportFactory,
	options ConnectionOptions,
) {
	defer c.wg.Done()
	defer c.settle()

	transport, resource, transportErr := factory(ctx, server.definition)
	if transportErr != nil {
		c.fail(server, "transport", "transport_failed", "server transport could not be prepared")

		return
	}

	connectCtx, cancel := context.WithTimeout(ctx, server.connectTimeout)
	client, connectErr := agentmcp.Connect(
		connectCtx,
		options.Implementation,
		transport,
		agentmcp.WithToolNamePrefix(server.id),
		agentmcp.WithMaxTools(options.MaxTools),
	)
	cancel()
	if connectErr != nil {
		_ = closeConnection(nil, resource)
		c.fail(server, "connect", "connect_failed", "server connection could not be initialized")

		return
	}

	listCtx, listCancel := context.WithTimeout(ctx, server.connectTimeout)
	tools, listErr := client.Tools(listCtx)
	listCancel()
	if listErr != nil {
		_ = closeConnection(client, resource)
		c.fail(server, "list", "list_failed", "server tools could not be listed")

		return
	}

	if err := c.install(server, client, resource, tools); err != nil {
		_ = closeConnection(client, resource)
		if !errors.Is(err, errConnectionsClosed) {
			c.fail(server, "list", "catalog_invalid", "server tools could not join the tool catalog")
		}
	}
}

// install publishes one connected server. A client that arrives after Close
// is rejected so the caller closes it instead of leaking it.
func (c *Connections) install(
	server *managedServer,
	client *agentmcp.Client,
	resource io.Closer,
	tools []agent.Tool,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return errConnectionsClosed
	}

	previousTools, previousState := server.tools, server.state
	server.tools = slices.Clone(tools)
	server.state = ServerStateConnected
	if err := c.rebuildLocked(); err != nil {
		server.tools = previousTools
		server.state = previousState

		return err
	}

	server.settledAt = time.Now().UTC()
	server.client = client
	server.resource = resource

	return nil
}

func (c *Connections) fail(server *managedServer, stage, code, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	server.state = ServerStateFailed
	server.stage = stage
	server.code = code
	server.message = message
	server.settledAt = time.Now().UTC()
	server.tools = nil
}

func (c *Connections) settle() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending--
	if c.pending == 0 {
		close(c.settled)
	}
}

// Wait blocks until every enabled server has connected or failed, or until
// ctx ends. A nil receiver has nothing to wait for.
func (c *Connections) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}

	select {
	case <-c.settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Settled reports whether every enabled server has connected or failed.
func (c *Connections) Settled() bool {
	if c == nil {
		return true
	}

	select {
	case <-c.settled:
		return true
	default:
		return false
	}
}

// Servers returns a credential-free status for every configured definition
// in resolved order, including pending and disabled servers.
func (c *Connections) Servers() []ServerStatus {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	values := make([]ServerStatus, 0, len(c.servers))
	for _, server := range c.servers {
		values = append(values, c.serverStatusLocked(server))
	}

	return values
}

func (c *Connections) serverStatusLocked(server *managedServer) ServerStatus {
	status := ServerStatus{
		ID: server.id, Scope: server.scope, Transport: server.transport,
		Visibility: server.visibility, State: server.state,
		Stage: server.stage, Code: server.code, Message: server.message,
		StartedAt: server.startedAt, SettledAt: server.settledAt,
	}
	if server.state == ServerStateConnected {
		status.Tools = make([]string, 0, len(server.tools))
		for _, tool := range server.tools {
			status.Tools = append(status.Tools, tool.Decl().Name)
		}
		slices.Sort(status.Tools)
	}

	return status
}

// Snapshot returns the latest successfully installed tool snapshot.
func (c *Connections) Snapshot() agentmcp.RegistrySnapshot {
	if c == nil {
		return agentmcp.RegistrySnapshot{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return cloneSnapshot(c.snapshot)
}

// Entries returns a detached current catalog filtered by configured
// visibility. Unknown visibility values return no entries.
func (c *Connections) Entries(visibility Visibility) []catalog.Entry {
	if c == nil || (visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate) {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	selected := make([]catalog.Entry, 0, len(c.snapshot.Entries))
	for _, entry := range c.snapshot.Entries {
		server := c.serverLocked(entry.Source.ID)
		if server == nil || server.visibility != visibility {
			continue
		}
		selected = append(selected, cloneEntry(entry))
	}

	return selected
}

// ConnectedServers returns credential-free bindings for successfully
// connected servers. Entry snapshots reflect the latest successfully
// installed snapshot.
func (c *Connections) ConnectedServers(visibility Visibility) []ConnectedServer {
	if c == nil || (visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate) {
		return nil
	}

	byID := make(map[string][]catalog.Entry)
	for _, entry := range c.Entries(visibility) {
		byID[entry.Source.ID] = append(byID[entry.Source.ID], cloneEntry(entry))
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	values := make([]ConnectedServer, 0, len(c.servers))
	for _, server := range c.servers {
		if server.state != ServerStateConnected || server.visibility != visibility {
			continue
		}
		values = append(values, ConnectedServer{
			ID: server.id, Fingerprint: server.fingerprint, Visibility: server.visibility,
			Entries: cloneEntries(byID[server.id]),
		})
	}

	return values
}

func (c *Connections) serverLocked(id string) *managedServer {
	for _, server := range c.servers {
		if server.id == id {
			return server
		}
	}

	return nil
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

func cloneSnapshot(snapshot agentmcp.RegistrySnapshot) agentmcp.RegistrySnapshot {
	snapshot.Entries = cloneEntries(snapshot.Entries)

	return snapshot
}

// Diagnostics returns every safe lifecycle diagnostic in a deterministic
// order: option diagnostics, then one per failed server in resolved order,
// then refresh diagnostics. Goroutine completion order never changes it.
func (c *Connections) Diagnostics() []ConnectionDiagnostic {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	values := slices.Clone(c.options)
	for _, server := range c.servers {
		if server.state == ServerStateFailed {
			values = append(values, serverDiagnostic(server))
		}
	}

	return append(values, c.refresh...)
}

// TakeDiagnostics returns the option and failed-server diagnostics that no
// earlier call has returned, in the same order as Diagnostics. Refresh
// diagnostics are excluded because the caller reports refresh failures
// inline at the interaction boundary.
func (c *Connections) TakeDiagnostics() []ConnectionDiagnostic {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	values := make([]ConnectionDiagnostic, 0)
	if !c.optionsTaken {
		c.optionsTaken = true
		values = append(values, c.options...)
	}
	for _, server := range c.servers {
		if server.state != ServerStateFailed || server.reported {
			continue
		}
		server.reported = true
		values = append(values, serverDiagnostic(server))
	}

	return values
}

func serverDiagnostic(server *managedServer) ConnectionDiagnostic {
	return connectionDiagnostic(server.id, server.stage, server.code, server.message)
}

// RefreshChanged consumes pending tool list-change notifications only at an
// application-selected safe boundary. Only the servers that signaled are
// listed again. A failed refresh preserves the prior snapshot and emits one
// coalesced diagnostic until a later successful attempt.
func (c *Connections) RefreshChanged(
	ctx context.Context,
) (agentmcp.RegistrySnapshot, bool, error) {
	if c == nil {
		return agentmcp.RegistrySnapshot{}, false, errors.New("coding mcp: connections not initialized")
	}

	changed := c.changedServers()
	if len(changed) == 0 {
		return c.Snapshot(), false, nil
	}

	listed := make(map[*managedServer][]agent.Tool, len(changed))
	var listErr error
	for _, server := range changed {
		listCtx, cancel := context.WithTimeout(ctx, server.connectTimeout)
		tools, err := server.client.Tools(listCtx)
		cancel()
		if err != nil {
			listErr = fmt.Errorf("coding mcp: refresh %q: %w", server.id, err)

			break
		}
		listed[server] = tools
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if listErr == nil {
		listErr = c.applyRefreshLocked(listed)
	}
	if listErr != nil {
		if !c.refreshFailed {
			c.refresh = append(c.refresh, connectionDiagnostic(
				"registry",
				"refresh",
				"refresh_failed",
				"MCP tool refresh failed; the previous snapshot remains active",
			))
		}
		c.refreshFailed = true

		return cloneSnapshot(c.snapshot), true, listErr
	}
	c.refreshFailed = false

	return cloneSnapshot(c.snapshot), true, nil
}

func (c *Connections) changedServers() []*managedServer {
	c.mu.Lock()
	defer c.mu.Unlock()

	changed := make([]*managedServer, 0)
	for _, server := range c.servers {
		if server.state != ServerStateConnected || c.closed {
			continue
		}
		select {
		case <-server.client.ToolListChanged():
			changed = append(changed, server)
		default:
		}
	}

	return changed
}

func (c *Connections) applyRefreshLocked(listed map[*managedServer][]agent.Tool) error {
	if c.closed {
		return errConnectionsClosed
	}

	previous := make(map[*managedServer][]agent.Tool, len(listed))
	for server, tools := range listed {
		previous[server] = server.tools
		server.tools = slices.Clone(tools)
	}
	if err := c.rebuildLocked(); err != nil {
		for server, tools := range previous {
			server.tools = tools
		}

		return err
	}

	return nil
}

// rebuildLocked assembles the merged catalog from every connected server in
// resolved order and installs it only when it validates and materially
// changed.
func (c *Connections) rebuildLocked() error {
	entries := make([]catalog.Entry, 0)
	for _, server := range c.servers {
		if server.state != ServerStateConnected {
			continue
		}
		entries = append(entries, catalog.MCP(server.id, catalog.RiskPrivileged, c.markReadOnly(server)...)...)
	}
	if _, err := catalog.New(entries...); err != nil {
		return fmt.Errorf("coding mcp: invalid tool snapshot: %w", err)
	}

	signature, err := entriesSignature(entries)
	if err != nil {
		return err
	}
	if signature == c.signature {
		return nil
	}

	c.signature = signature
	c.snapshot = agentmcp.RegistrySnapshot{
		Version:   c.snapshot.Version + 1,
		UpdatedAt: time.Now().UTC(),
		Entries:   cloneEntries(entries),
	}

	return nil
}

// markReadOnly wraps the server's user-marked read-only tools with
// agent.Parallel. Every other tool stays serial.
func (c *Connections) markReadOnly(server *managedServer) []agent.Tool {
	names, ok := c.readOnly[server.id]
	if !ok {
		return server.tools
	}

	_, all := names[ReadOnlyWildcard]
	tools := make([]agent.Tool, len(server.tools))
	for index, tool := range server.tools {
		tools[index] = tool
		if remote, ok := tool.(interface{ RemoteName() string }); ok {
			if _, marked := names[remote.RemoteName()]; all || marked {
				tools[index] = agent.Parallel(tool)
			}
		}
	}

	return tools
}

func readOnlySets(values map[string][]string) map[string]map[string]struct{} {
	sets := make(map[string]map[string]struct{}, len(values))
	for server, names := range values {
		set := make(map[string]struct{}, len(names))
		for _, name := range names {
			set[name] = struct{}{}
		}
		sets[server] = set
	}

	return sets
}

func concurrent(tool agent.Tool) bool {
	safe, ok := tool.(agent.ConcurrencySafe)

	return ok && safe.Concurrent()
}

// entriesSignature mirrors the agent/mcp Registry material-change rule: the
// JSON encoding of every advertised declaration with its source and risk,
// plus its read-only (concurrent) marking.
func entriesSignature(entries []catalog.Entry) (string, error) {
	declarations := make([]any, 0, len(entries))
	for _, entry := range entries {
		declarations = append(declarations, struct {
			Name   string
			Source catalog.Source
			Risk   catalog.Risk
			Decl   any
			Par    bool `json:",omitempty"`
		}{entry.Tool.Decl().Name, entry.Source, entry.Risk, entry.Tool.Decl(), concurrent(entry.Tool)})
	}

	data, err := json.Marshal(declarations)
	if err != nil {
		return "", fmt.Errorf("coding mcp: encode tool snapshot: %w", err)
	}

	return string(data), nil
}

// Close cancels in-flight connects, waits for them, and closes connected
// clients and their resources in reverse resolved order.
func (c *Connections) Close() error {
	if c == nil {
		return nil
	}

	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.cancel()
		c.wg.Wait()

		c.mu.Lock()
		owned := make([]*managedServer, 0, len(c.servers))
		for _, server := range c.servers {
			if server.client != nil {
				owned = append(owned, server)
			}
		}
		c.mu.Unlock()

		errs := make([]error, 0, len(owned))
		for _, server := range slices.Backward(owned) {
			if err := closeConnection(server.client, server.resource); err != nil {
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
