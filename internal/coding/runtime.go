//nolint:wsl_v5 // Runtime construction keeps acquisition and rollback steps adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/bundle"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes/git"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/generation"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	defaultHTTPTimeout      = 30 * time.Second
	defaultMCPTerminateTime = 2 * time.Second
	defaultMCPMaxTools      = 256
	defaultToolTimeout      = 5 * time.Minute
	maximumToolTimeout      = 30 * time.Minute
	componentMCP            = "mcp"
)

// SessionTarget selects a new session when ID is empty or a durable session
// to reopen when ID is set.
type SessionTarget struct {
	ID string
}

// ExecutionOptions contain process- and resource-bound Runtime dependencies.
// Zero limits select the package production defaults.
type ExecutionOptions struct {
	TempRoot     string
	GitPath      string
	Environment  func(string) (string, bool)
	SandboxProbe func(context.Context, *execution.Executor) error
	ToolTimeout  time.Duration

	HTTPClient     *http.Client
	MCPClient      *sdk.Implementation
	MCPTerminate   time.Duration
	MCPMaxTools    int
	ResourceLimits resource.Limits
	MCPLimits      codingmcp.Limits
	ToolLimits     tools.Limits
	GitLimits      git.Limits
	Subagent       subagent.ExecutionOptions
}

// OpenOptions explicitly bind one Runtime to one Workspace, configuration,
// and durable session. Model is an optional test/embedder override; production
// callers normally provide Credentials and let Open construct the model.
type OpenOptions struct {
	Workspace workspace.Workspace
	Trusted   bool
	Config    config.Config
	Paths     paths.Layout
	Session   SessionTarget

	Credentials credential.Store
	Model       ai.LanguageModel
	Resolved    modelcatalog.ResolvedModel
	Execution   ExecutionOptions
	Extensions  []extension.Extension

	// AgentObservers receive raw run, turn, and tool events. TelemetryObservers
	// receive the content-free Coding product projection.
	AgentObservers     []func(context.Context, agent.Event)
	TelemetryObservers []TelemetryObserver
}

// Runtime is the Coding Agent composition root for one Workspace and one
// writable Harness session.
type Runtime struct {
	mu    sync.Mutex
	state State

	workspace     workspace.Workspace
	tree          *workspace.Tree
	config        config.Config
	paths         paths.Layout
	opts          ExecutionOptions
	model         ai.LanguageModel
	resolved      modelcatalog.ResolvedModel
	requestPolicy generation.Policy

	handle      *session.Handle
	repository  *session.Repository
	session     *harness.Session
	journal     *interactionJournal
	policy      execution.Policy
	executor    *execution.Executor
	inspector   *git.Inspector
	permissions *codingmcp.Permissions
	connections *codingmcp.Connections
	extensions  *extension.Runtime
	compiled    []extension.Extension
	resources   resource.Result
	trusted     bool
	controller  *approval.Controller
	resolver    activeResolver
	pending     pendingRunner
	observers   *agentObservers
	telemetry   *telemetryObservers
	subagents   *subagent.Manager

	writer      *eventWriter
	interaction *interaction
	active      *runtimeOperation
	recovery    InteractionRecovery

	closing                    bool
	cleanupRunning             bool
	closed                     bool
	closeDone                  chan struct{}
	closeErr                   error
	autoCompactionFailureToken string
}

type runtimeOperation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type cleanupStack struct {
	values []func(context.Context) error
}

func (stack *cleanupStack) add(value func(context.Context) error) {
	stack.values = append(stack.values, value)
}

func (stack *cleanupStack) close(ctx context.Context) error {
	errs := make([]error, 0, len(stack.values))
	for _, cleanup := range slices.Backward(stack.values) {
		if err := cleanup(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Open validates and acquires every Runtime dependency. A failed open releases
// already-acquired resources in exact reverse order.
//
//nolint:gocyclo,funlen // Composition order and rollback ownership stay explicit here.
func Open(ctx context.Context, options OpenOptions) (_ *Runtime, returnErr error) {
	if err := validateOpenOptions(options); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	configured := withExecutionDefaults(options.Execution)
	if configured.ToolTimeout < 0 || configured.ToolTimeout > maximumToolTimeout ||
		configured.MCPTerminate < 0 || configured.MCPMaxTools < 0 {
		return nil, fmt.Errorf("%w: invalid execution duration or count", ErrRuntimeInvalid)
	}
	if configured.TempRoot == "" {
		configured.TempRoot = filepath.Join(options.Paths.Root(), "tmp")
	}
	if configured.GitPath == "" {
		gitPath, err := findExecutable("git", configured.Environment)
		if err != nil {
			return nil, err
		}
		configured.GitPath = gitPath
	}
	if err := ensurePrivateTempRoot(configured.TempRoot); err != nil {
		return nil, err
	}

	stack := &cleanupStack{}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, stack.close(context.WithoutCancel(ctx)))
		}
	}()

	tree, err := workspace.OpenTree(options.Workspace)
	if err != nil {
		return nil, fmt.Errorf("coding runtime: open workspace tree: %w", err)
	}
	stack.add(func(context.Context) error { return tree.Close() })

	resolved, err := resolveOpenModel(options)
	if err != nil {
		return nil, err
	}
	requestPolicy, err := generation.Compile(resolved)
	if err != nil {
		return nil, err
	}

	baseModel := options.Model
	if baseModel == nil {
		baseModel, err = model.New(ctx, resolved, options.Credentials)
		if err != nil {
			return nil, err
		}
	}
	if baseModel.Provider() != resolved.Ref.Provider ||
		baseModel.ModelID() != resolved.Ref.Model {
		return nil, fmt.Errorf("%w: model override does not match configuration", ErrRuntimeInvalid)
	}

	repository, err := session.NewRepository(options.Paths.SessionsDir())
	if err != nil {
		return nil, err
	}

	handle, resumed, err := openSession(ctx, repository, options)
	if err != nil {
		return nil, err
	}
	stack.add(func(context.Context) error { return handle.Close() })

	policy, err := newRuntimePolicy(options)
	if err != nil {
		return nil, err
	}

	executor, err := execution.NewExecutor(options.Workspace, execution.ExecutorConfig{
		TempRoot:    configured.TempRoot,
		Environment: configured.Environment,
		Protected:   []string{options.Paths.Root()},
	})
	if err != nil {
		return nil, err
	}
	if options.Config.Sandbox == config.SandboxWorkspaceWrite {
		probe := configured.SandboxProbe
		if probe == nil {
			probe = func(probeCtx context.Context, value *execution.Executor) error {
				_, probeErr := value.Probe(probeCtx)

				return probeErr
			}
		}

		if err := probe(ctx, executor); err != nil {
			return nil, err
		}
	}

	inspector, err := git.New(options.Workspace, tree, policy, executor, git.Config{
		GitPath: configured.GitPath, TempRoot: configured.TempRoot, Limits: configured.GitLimits,
	})
	if err != nil {
		return nil, err
	}
	stack.add(func(context.Context) error { return inspector.Close() })

	loadedResources, err := resource.Load(ctx, resource.Options{
		Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
		Limits: configured.ResourceLimits,
	})
	if err != nil {
		return nil, err
	}

	store := workspace.NewStore(options.Paths.WorkspacesFile())
	permissions, err := codingmcp.NewPermissions(codingmcp.PermissionOptions{
		Workspace: options.Workspace, Tree: tree, Store: store, Git: inspector,
		Limits: configured.MCPLimits,
	})
	if err != nil {
		return nil, err
	}

	connections, err := openMCP(ctx, options, configured, tree, permissions)
	if err != nil {
		return nil, err
	}
	stack.add(func(context.Context) error { return connections.Close() })

	extensionRuntime, err := extension.New(extension.WithExtensions(options.Extensions...))
	if err != nil {
		return nil, err
	}
	stack.add(extensionRuntime.Shutdown)

	setup, err := activateResources(
		ctx,
		extensionRuntime,
		loadedResources,
		options.Extensions,
	)
	if err != nil {
		return nil, err
	}
	if err := setup.Release(ctx); err != nil {
		return nil, err
	}

	runtime := &Runtime{
		workspace:     options.Workspace,
		tree:          tree,
		config:        options.Config.Clone(),
		paths:         options.Paths,
		opts:          configured,
		model:         baseModel,
		resolved:      resolved,
		requestPolicy: requestPolicy,
		handle:        handle,
		repository:    repository,
		session:       handle.Session(),
		policy:        policy,
		executor:      executor,
		inspector:     inspector,
		permissions:   permissions,
		connections:   connections,
		extensions:    extensionRuntime,
		compiled:      slices.Clone(options.Extensions),
		resources:     loadedResources,
		trusted:       options.Trusted,
		observers:     newAgentObservers(options.AgentObservers),
		telemetry:     newTelemetryObservers(options.TelemetryObservers),
		closeDone:     make(chan struct{}),
	}

	runtime.journal, err = newInteractionJournal(runtime.session, nil)
	if err != nil {
		return nil, err
	}
	runtime.writer, err = newEventWriter(handle.Metadata().ID, time.Now)
	if err != nil {
		return nil, err
	}

	pending, err := runtime.session.Pending()
	if err != nil {
		return nil, err
	}
	for _, call := range pending {
		if isLegacyCodingTool(call.Name) {
			return nil, fmt.Errorf(
				"%w: legacy_tool_call_unresolved (%s)",
				ErrLegacyToolCallUnresolved,
				call.Name,
			)
		}
	}
	if err := subagent.Reconcile(ctx, repository, handle); err != nil {
		return nil, fmt.Errorf("coding runtime: reconcile subagents: %w", err)
	}
	treeSnapshot, err := runtime.session.Tree(harness.TreeLimits{})
	if err != nil {
		return nil, err
	}
	bootstrap, err := BootstrapState(BootstrapOptions{
		SessionID:           handle.Metadata().ID,
		Provider:            resolved.Ref.Provider,
		ModelID:             resolved.Ref.Model,
		Path:                runtime.session.Path(),
		HasPendingToolCalls: len(pending) > 0,
		Tree:                treeSnapshot,
	})
	if err != nil {
		return nil, err
	}
	runtime.state = bootstrap.State
	runtime.recovery = bootstrap.Recovery

	runtime.controller, err = approval.New(
		options.Workspace,
		runtime.session,
		&runtime.resolver,
		&runtime.pending,
		policy,
		executor,
		tools.NewShellHandler(),
	)
	if err != nil {
		return nil, err
	}
	runtime.subagents, err = subagent.New(subagent.Config{
		Repository:     repository,
		Parent:         handle,
		Tree:           tree,
		Model:          baseModel,
		RequestPolicy:  requestPolicy,
		Options:        configured.Subagent,
		AgentObservers: []func(context.Context, agent.Event){runtime.observers.observe},
	})
	if err != nil {
		return nil, err
	}
	stack.add(runtime.subagents.Close)

	runtime.observeSessionOpened(ctx, resumed)
	runtime.recordOpenDiagnostics(ctx, loadedResources, connections)
	stack.values = nil

	return runtime, nil
}

func resolveOpenModel(options OpenOptions) (modelcatalog.ResolvedModel, error) {
	if options.Resolved.Ref.String() != "" {
		if options.Resolved.Ref != options.Config.Model {
			return modelcatalog.ResolvedModel{}, fmt.Errorf(
				"%w: resolved model does not match configuration selection",
				ErrRuntimeInvalid,
			)
		}

		return options.Resolved.Clone(), nil
	}

	catalog, err := modelcatalog.New(options.Config)
	if err != nil {
		return modelcatalog.ResolvedModel{}, err
	}

	return catalog.Resolve(modelcatalog.SelectionFromConfig(options.Config))
}

func validateOpenOptions(options OpenOptions) error {
	if options.Workspace.Root() == "" || options.Workspace.Identity().Key() == "" ||
		options.Paths.Root() == "" {
		return fmt.Errorf("%w: workspace and paths are required", ErrRuntimeInvalid)
	}

	if err := options.Config.ValidateRuntime(); err != nil {
		return fmt.Errorf("%w: %w", ErrRuntimeInvalid, err)
	}

	if strings.TrimSpace(options.Session.ID) != options.Session.ID {
		return fmt.Errorf("%w: session ID has surrounding whitespace", ErrRuntimeInvalid)
	}

	for _, observer := range options.AgentObservers {
		if observer == nil {
			return fmt.Errorf("%w: nil Agent observer", ErrRuntimeInvalid)
		}
	}

	for _, observer := range options.TelemetryObservers {
		if observer == nil {
			return fmt.Errorf("%w: nil Telemetry observer", ErrRuntimeInvalid)
		}
	}

	return nil
}

func withExecutionDefaults(options ExecutionOptions) ExecutionOptions {
	if options.Environment == nil {
		options.Environment = os.LookupEnv
	}

	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	if options.MCPClient == nil {
		options.MCPClient = &sdk.Implementation{Name: "pips", Version: "dev"}
	}

	if options.MCPTerminate == 0 {
		options.MCPTerminate = defaultMCPTerminateTime
	}

	if options.MCPMaxTools == 0 {
		options.MCPMaxTools = defaultMCPMaxTools
	}

	if options.ToolTimeout == 0 {
		options.ToolTimeout = defaultToolTimeout
	}

	if options.ResourceLimits.MaxEntries == 0 {
		options.ResourceLimits = resource.DefaultLimits()
	}

	if options.MCPLimits.MaxFileBytes == 0 {
		options.MCPLimits = codingmcp.DefaultLimits()
	}

	if options.ToolLimits.OutputBytes == 0 {
		options.ToolLimits = tools.DefaultLimits()
	}

	if options.GitLimits.Files == 0 {
		options.GitLimits = git.DefaultLimits()
	}

	return options
}

func ensurePrivateTempRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%w: temp root must be absolute", ErrRuntimeInvalid)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("coding runtime: create temp root: %w", err)
	}

	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: temp root must be a private directory", ErrRuntimeInvalid)
	}

	return nil
}

func findExecutable(name string, lookup func(string) (string, bool)) (string, error) {
	pathValue, ok := lookup("PATH")
	if !ok {
		return "", fmt.Errorf("%w: PATH is unavailable for %s", ErrRuntimeInvalid, name)
	}

	for _, directory := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(directory) {
			continue
		}

		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", fmt.Errorf("coding runtime: resolve %s: %w", name, resolveErr)
			}

			return resolved, nil
		}
	}

	return "", fmt.Errorf("%w: executable %s was not found", ErrRuntimeInvalid, name)
}

func openSession(
	ctx context.Context,
	repository *session.Repository,
	options OpenOptions,
) (*session.Handle, bool, error) {
	if options.Session.ID == "" {
		handle, err := repository.Create(ctx, session.CreateOptions{
			WorkspaceID: options.Workspace.Identity().Key(),
		})

		return handle, false, err
	}

	handle, err := repository.Open(ctx, session.OpenOptions{
		ID: options.Session.ID, WorkspaceID: options.Workspace.Identity().Key(),
	})
	if err != nil {
		return nil, true, err
	}

	return handle, true, nil
}

func newRuntimePolicy(options OpenOptions) (execution.Policy, error) {
	source, _ := options.Config.Source(config.FieldSandbox)

	return execution.NewPolicy(options.Workspace, execution.PolicyConfig{
		Sandbox: options.Config.Sandbox, Approval: options.Config.Approval,
		SandboxSource: source, Protected: []string{options.Paths.Root()},
	})
}

func openMCP(
	ctx context.Context,
	options OpenOptions,
	configured ExecutionOptions,
	tree *workspace.Tree,
	permissions *codingmcp.Permissions,
) (*codingmcp.Connections, error) {
	definitions, err := codingmcp.LoadDefinitions(ctx, codingmcp.LoadOptions{
		Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
		Limits: configured.MCPLimits,
	})
	if err != nil {
		return nil, err
	}

	resolved, err := permissions.Resolve(ctx, definitions)
	if err != nil {
		return nil, err
	}

	return codingmcp.OpenConnections(ctx, resolved, codingmcp.ConnectionOptions{
		Workspace:      options.Workspace,
		Implementation: configured.MCPClient,
		HTTPClient:     configured.HTTPClient,
		TempRoot:       configured.TempRoot,
		Environment:    configured.Environment,
		TerminateAfter: configured.MCPTerminate,
		MaxTools:       configured.MCPMaxTools,
	})
}

func activateResources(
	ctx context.Context,
	runtime *extension.Runtime,
	loaded resource.Result,
	compiled []extension.Extension,
) (*extension.Activation, error) {
	bundles := loaded.Bundles()
	if len(bundles) == 0 {
		return runtime.Activate(ctx, compiled...)
	}

	return bundle.Activate(ctx, runtime, bundles...)
}

func (r *Runtime) recordOpenDiagnostics(
	ctx context.Context,
	loaded resource.Result,
	connections *codingmcp.Connections,
) {
	for _, diagnostic := range loaded.Diagnostics() {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: "resource", Code: diagnostic.Code, Message: diagnostic.Message,
		})
	}

	for _, diagnostic := range connections.Diagnostics() {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: componentMCP, Code: diagnostic.Code, Message: diagnostic.Message, Disabled: true,
		})
	}
}

func (r *Runtime) recordDiagnostic(ctx context.Context, diagnostic IntegrationDiagnostic) {
	emitter := newEventEmitter(ctx, r, nil, false)
	_ = emitter.emit("", "", EventIntegrationDiagnostic, diagnostic)
}

type agentObservers struct {
	mu       sync.Mutex
	values   []func(context.Context, agent.Event)
	disabled []bool
	pending  int
}

func newAgentObservers(values []func(context.Context, agent.Event)) *agentObservers {
	return &agentObservers{
		values:   slices.Clone(values),
		disabled: make([]bool, len(values)),
	}
}

func (o *agentObservers) observe(ctx context.Context, event agent.Event) {
	if o == nil {
		return
	}

	for index, observe := range o.values {
		o.mu.Lock()
		disabled := o.disabled[index]
		o.mu.Unlock()
		if disabled {
			continue
		}

		panicked := true
		func() {
			defer func() { _ = recover() }()
			observe(ctx, event)
			panicked = false
		}()

		if panicked {
			o.mu.Lock()
			if !o.disabled[index] {
				o.disabled[index] = true
				o.pending++
			}
			o.mu.Unlock()
		}
	}
}

func (o *agentObservers) drainDisabled() int {
	if o == nil {
		return 0
	}

	o.mu.Lock()
	pending := o.pending
	o.pending = 0
	o.mu.Unlock()

	return pending
}

// Snapshot returns a defensive reducer state snapshot.
func (r *Runtime) Snapshot() State {
	if r == nil {
		return State{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state.Clone()
}

// Prompt starts one new interaction. Iteration owns cancellation and cleanup.
func (r *Runtime) Prompt(ctx context.Context, messages ...ai.Message) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationPrompt, approval.Resolution{}, messages)
}

// Continue reconciles a durable pending interaction after reopening a session.
func (r *Runtime) Continue(ctx context.Context) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationContinue, approval.Resolution{}, nil)
}

// Resolve applies one explicit approval decision and continues only when the
// durable pending suffix becomes ready.
func (r *Runtime) Resolve(
	ctx context.Context,
	resolution approval.Resolution,
) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationResolve, resolution, nil)
}

type runtimeOperationKind string

const (
	operationPrompt   runtimeOperationKind = "prompt"
	operationContinue runtimeOperationKind = "continue"
	operationResolve  runtimeOperationKind = "resolve"
	operationPreview  runtimeOperationKind = "preview compaction"
	operationCompact  runtimeOperationKind = "compact"
	operationNavigate runtimeOperationKind = "navigate"
	operationFork     runtimeOperationKind = "fork"
)

func (r *Runtime) runSequence(
	ctx context.Context,
	kind runtimeOperationKind,
	resolution approval.Resolution,
	messages []ai.Message,
) iter.Seq2[Event, error] {
	cloned := cloneMessages(messages)

	return func(yield func(Event, error) bool) {
		if r == nil {
			yield(Event{}, ErrRuntimeClosed)
			return
		}

		r.run(ctx, kind, resolution, cloned, yield)
	}
}

func cloneMessages(messages []ai.Message) []ai.Message {
	cloned := make([]ai.Message, len(messages))
	for index, message := range messages {
		cloned[index] = cloneMessage(message)
	}

	return cloned
}

// Reload installs fresh resource, MCP, and Extension generations only at an
// idle safe boundary. Existing interactions retain their leased snapshot.
//
//nolint:gocyclo // Reload has one rollback branch for each acquired generation.
func (r *Runtime) Reload(ctx context.Context) error {
	if r == nil {
		return ErrRuntimeClosed
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()
		return stateError("reload", phase, ErrRuntimeClosed)
	}

	if r.active != nil || r.interaction != nil || r.state.Phase != PhaseIdle {
		phase := r.state.Phase
		r.mu.Unlock()
		return stateError("reload", phase, ErrRuntimeBusy)
	}

	reloadCtx, cancel := context.WithCancel(ctx)
	operation := &runtimeOperation{cancel: cancel, done: make(chan struct{})}
	r.active = operation
	r.mu.Unlock()
	defer r.endOperation(operation)

	loaded, err := resource.Load(reloadCtx, resource.Options{
		Paths: r.paths, Tree: r.tree, ProjectTrusted: r.trusted, Limits: r.opts.ResourceLimits,
	})
	if err != nil {
		return err
	}

	options := OpenOptions{
		Workspace: r.workspace, Trusted: r.trusted, Paths: r.paths,
	}
	connections, err := openMCP(reloadCtx, options, r.opts, r.tree, r.permissions)
	if err != nil {
		return err
	}

	activation, err := activateResources(reloadCtx, r.extensions, loaded, r.compiled)
	if err != nil {
		return errors.Join(err, connections.Close())
	}
	if err := activation.Release(reloadCtx); err != nil {
		return errors.Join(err, connections.Close())
	}

	r.mu.Lock()
	if r.closed || r.closing || r.active != operation || r.interaction != nil {
		phase := r.state.Phase
		r.mu.Unlock()

		return errors.Join(stateError("reload", phase, ErrRuntimeBusy), connections.Close())
	}

	previous := r.connections
	r.connections = connections
	r.resources = loaded
	r.mu.Unlock()
	r.recordOpenDiagnostics(ctx, loaded, connections)

	return previous.Close()
}
