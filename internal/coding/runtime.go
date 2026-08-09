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
	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/rsbin/pips/internal/coding/agentprofile"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes/git"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/generation"
	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/rsbin/pips/internal/coding/instructions"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/plandoc"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/skillsettings"
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
	componentAgentPlugin    = "agent-plugin"
	componentHook           = "hook"
	componentIntegration    = "integration"
)

// SessionTarget selects a new session when ID is empty or a durable session
// to reopen when ID is set.
type SessionTarget struct {
	ID          string
	RetainEmpty bool
}

// ExecutionOptions contain process- and resource-bound Runtime dependencies.
// Zero limits select the package production defaults.
type ExecutionOptions struct {
	TempRoot    string
	GitPath     string
	Environment func(string) (string, bool)
	// HooksEnvironment is the invoking process environment snapshot supplied
	// to reviewed lifecycle hook commands. It is intentionally distinct from
	// Environment, which is a lookup used by Coding tool execution.
	HooksEnvironment []string
	SandboxProbe     func(context.Context, *execution.Executor) error
	ToolTimeout      time.Duration

	HTTPClient         *http.Client
	MCPClient          *sdk.Implementation
	MCPTerminate       time.Duration
	MCPMaxTools        int
	ResourceLimits     resource.Limits
	AgentProfileLimits agentprofile.Limits
	AgentPluginLimits  agentplugin.Limits
	MCPLimits          codingmcp.Limits
	MCPDefinitions     codingmcp.Definitions
	ToolLimits         tools.Limits
	GitLimits          git.Limits
	Subagent           subagent.ExecutionOptions
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

	profile             runtimeProfile
	worker              *workerRuntimeBinding
	workspace           workspace.Workspace
	tree                *workspace.Tree
	config              config.Config
	paths               paths.Layout
	opts                ExecutionOptions
	model               ai.LanguageModel
	modelCatalog        modelcatalog.Catalog
	credentials         credential.Store
	resolved            modelcatalog.ResolvedModel
	requestPolicy       generation.Policy
	instructionResolver *instructions.Resolver
	promptDate          string
	projectInstructions string
	hookContext         []string
	hookToolContext     map[string][]string
	hookDefinitions     []hooks.Definition
	hookRunner          hooks.Runner

	handle          *session.Handle
	repository      *session.Repository
	session         *harness.Session
	journal         *interactionJournal
	plans           plandoc.Repository
	planRef         plandoc.Ref
	policy          execution.Policy
	executor        *execution.Executor
	tempRoot        *execution.PrivateTempRoot
	inspector       *git.Inspector
	permissions     *codingmcp.Permissions
	extensions      *extension.Runtime
	compiled        []extension.Extension
	skillSettings   *skillsettings.Manager
	integration     *IntegrationGeneration
	generationID    uint64
	trusted         bool
	controller      *approval.Controller
	questions       *question.Controller
	planReviews     *planreview.Controller
	resolver        activeResolver
	pending         pendingRunner
	observers       *agentObservers
	telemetry       *telemetryObservers
	subagents       *subagent.Manager
	childControls   *childControlRegistry
	notifications   *subagent.NotificationInbox
	children        map[string]*childProjection
	teamGuard       teamCapabilityGuard
	admission       *teamAdmission
	team            *teamCoordinator
	teamRecovery    []TeamRecoveryCandidate
	teamRecoveryErr error

	writer      *eventWriter
	publisher   *eventPublisher
	interaction *interaction
	active      *runtimeOperation
	recovery    InteractionRecovery

	closing                    bool
	cleanupRunning             bool
	closed                     bool
	closeDone                  chan struct{}
	closeErr                   error
	autoCompactionFailureToken string

	notificationMu       sync.Mutex
	notificationCancel   context.CancelFunc
	notificationSignal   chan struct{}
	notificationDone     chan struct{}
	notificationInflight map[string]struct{}
	notificationMessages map[string][]string
	notificationBatches  map[string]int
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
func Open(ctx context.Context, options OpenOptions) (*Runtime, error) {
	return openRuntime(ctx, options, runtimeOpenPolicy{profile: profileLead})
}

// openRuntime is the only Runtime construction path. The Team Worker profile
// remains package-private so configuration, CLI, MCP, and Extensions cannot
// select a more privileged composition shape.
//
//nolint:gocyclo,funlen // Composition order and rollback ownership stay explicit here.
func openRuntime(
	ctx context.Context,
	options OpenOptions,
	openPolicy runtimeOpenPolicy,
) (_ *Runtime, returnErr error) {
	var err error
	options, openPolicy, err = normalizeRuntimeOpen(options, openPolicy)
	if err != nil {
		return nil, err
	}
	if err := validateOpenOptions(options); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	configured := withExecutionDefaults(options.Execution)
	configured.Subagent = configuredSubagentOptions(
		options.Config.Subagent,
		options.Execution.Subagent,
	)
	if configured.ToolTimeout < 0 || configured.ToolTimeout > maximumToolTimeout ||
		configured.MCPTerminate < 0 || configured.MCPMaxTools < 0 {
		return nil, fmt.Errorf("%w: invalid execution duration or count", ErrRuntimeInvalid)
	}

	var scratchRoot *execution.PrivateTempRoot
	if configured.TempRoot == "" {
		scratchRoot, err = newRuntimeScratchRoot(options.Paths.Root())
		if err != nil {
			return nil, err
		}
		configured.TempRoot = scratchRoot.Path()
	}
	if configured.GitPath == "" {
		gitPath, err := findExecutable("git", configured.Environment)
		if err != nil {
			if scratchRoot != nil {
				_ = scratchRoot.Close()
			}

			return nil, err
		}
		configured.GitPath = gitPath
	}
	if err := ensurePrivateTempRoot(configured.TempRoot); err != nil {
		if scratchRoot != nil {
			_ = scratchRoot.Close()
		}

		return nil, err
	}
	if err := ensurePrivateApplicationRoot(options.Paths.Root()); err != nil {
		if scratchRoot != nil {
			_ = scratchRoot.Close()
		}

		return nil, err
	}

	stack := &cleanupStack{}
	if scratchRoot != nil {
		stack.add(func(context.Context) error { return scratchRoot.Close() })
	}
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

	instructionResolver, err := instructions.New(tree, instructions.DefaultLimits())
	if err != nil {
		return nil, fmt.Errorf("coding runtime: open project instructions: %w", err)
	}
	projectInstructions, err := instructionResolver.Resolve(ctx, ".")
	if err != nil {
		return nil, fmt.Errorf("coding runtime: resolve project instructions: %w", err)
	}

	modelCatalog, err := modelcatalog.New(options.Config)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveOpenModel(options, modelCatalog)
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

	handle, resumed, err := openSession(ctx, repository, options, openPolicy)
	if err != nil {
		return nil, err
	}
	stack.add(func(context.Context) error { return handle.Close() })
	planRepository, err := plandoc.New(options.Paths.PlansDir(), plandoc.DefaultLimits())
	if err != nil {
		return nil, err
	}
	planRef := plandoc.Ref{
		SessionID: handle.Metadata().ID, WorkspaceID: handle.Metadata().WorkspaceID,
	}

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

	skillSettings, err := skillsettings.New(skillsettings.Options{
		Tree: tree, Git: inspector, Trusted: options.Trusted, Limits: skillsettings.DefaultLimits(),
	})
	if err != nil {
		return nil, err
	}
	skillPolicy, err := skillSettings.Load(ctx)
	if err != nil {
		return nil, err
	}

	loadedResources, err := resource.Load(ctx, resource.Options{
		Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
		Limits: configured.ResourceLimits,
	})
	if err != nil {
		return nil, err
	}
	loadedProfiles, err := agentprofile.Load(ctx, agentprofile.Options{
		Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
		Limits: configured.AgentProfileLimits,
	})
	if err != nil {
		return nil, err
	}
	loadedPlugins := agentplugin.Result{}
	if !openPolicy.teamWorker() {
		loadedPlugins, err = agentplugin.Load(ctx, agentplugin.Options{
			Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
			Limits: configured.AgentPluginLimits,
		})
		if err != nil {
			return nil, err
		}
	}
	trustedHooks := make([]hooks.Definition, 0)
	pendingHookDiagnostics := make([]hooks.Diagnostic, 0)
	if !openPolicy.teamWorker() {
		loadedHooks, loadHooksErr := hooks.Load(ctx, hooks.LoadOptions{
			Paths:          options.Paths,
			Tree:           tree,
			ProjectTrusted: options.Trusted,
			Limits:         hooks.DefaultLimits(),
		})
		if loadHooksErr != nil {
			return nil, fmt.Errorf("coding runtime: load lifecycle hooks: %w", loadHooksErr)
		}
		hookStatuses, resolveHooksErr := hooks.NewTrustStore(options.Paths.HookTrustFile()).Resolve(
			ctx,
			loadedHooks,
			options.Workspace.Identity().Key(),
		)
		if resolveHooksErr != nil {
			return nil, fmt.Errorf("coding runtime: resolve lifecycle hook trust: %w", resolveHooksErr)
		}
		for _, status := range hookStatuses {
			if status.Status == hooks.StatusTrusted {
				trustedHooks = append(trustedHooks, status.Definition)
				continue
			}
			pendingHookDiagnostics = append(pendingHookDiagnostics, hooks.Diagnostic{
				Reference: status.Definition.Reference,
				Code:      "pending_trust",
				Message:   "hook is pending explicit trust; review it with pips hooks list",
			})
		}
	}

	store := workspace.NewStore(options.Paths.WorkspacesFile())
	permissions, err := codingmcp.NewPermissions(codingmcp.PermissionOptions{
		Workspace: options.Workspace, Tree: tree, Store: store, Git: inspector,
		Limits: configured.MCPLimits,
	})
	if err != nil {
		return nil, err
	}

	connections, err := openRuntimeMCP(
		ctx, options, configured, tree, permissions, openPolicy,
		loadedPlugins.MCPDefinitions(),
	)
	if err != nil {
		return nil, err
	}
	// The IntegrationGeneration is not constructed until Extension activation
	// succeeds, so register MCP ownership immediately. Otherwise a later
	// extension.New or activation failure can leak the already-open connections.
	connectionsOwnedByStack := true
	stack.add(func(context.Context) error {
		if !connectionsOwnedByStack {
			return nil
		}

		return connections.Close()
	})
	extensionOptions := make([]extension.Option, 0, 1)
	if !openPolicy.teamWorker() {
		extensionOptions = append(extensionOptions, extension.WithExtensions(options.Extensions...))
	}
	extensionRuntime, err := extension.New(extensionOptions...)
	if err != nil {
		return nil, err
	}
	stack.add(extensionRuntime.Shutdown)

	setup, activationErr := activateRuntimeResources(
		ctx, extensionRuntime, loadedResources, options.Extensions, openPolicy,
	)
	if setup == nil {
		if activationErr == nil {
			activationErr = fmt.Errorf("%w: extension activation returned no lease", ErrRuntimeInvalid)
		}

		return nil, activationErr
	}
	if activationErr != nil {
		// Open has no previously published application generation to retain.
		// Do not leak the installed Activation; reject this incomplete open.
		// The cleanup stack owns the MCP connections from this point onward.
		return nil, errors.Join(
			activationErr,
			setup.Release(context.WithoutCancel(ctx)),
		)
	}
	setupOwnedByStack := true
	stack.add(func(ctx context.Context) error {
		if !setupOwnedByStack {
			return nil
		}

		return setup.Release(ctx)
	})
	integration := newIntegrationGeneration(
		1, setup, connections, loadedResources, loadedProfiles, skillPolicy,
		projectInstructions.SystemPrompt(), loadedPlugins,
	)
	connectionsOwnedByStack = false
	setupOwnedByStack = false
	stack.add(integration.retire)

	runtime := &Runtime{
		profile:             openPolicy.profile,
		worker:              openPolicy.worker,
		workspace:           options.Workspace,
		tree:                tree,
		config:              options.Config.Clone(),
		paths:               options.Paths,
		opts:                configured,
		model:               baseModel,
		modelCatalog:        modelCatalog,
		credentials:         options.Credentials,
		resolved:            resolved,
		requestPolicy:       requestPolicy,
		instructionResolver: instructionResolver,
		promptDate:          time.Now().Format(time.DateOnly),
		projectInstructions: projectInstructions.SystemPrompt(),
		hookDefinitions:     trustedHooks,
		hookToolContext:     make(map[string][]string),
		hookRunner: hooks.Runner{
			Workspace:   options.Workspace.Root(),
			Environment: slices.Clone(configured.HooksEnvironment),
			Limits:      hooks.DefaultLimits(),
		},
		handle:        handle,
		repository:    repository,
		session:       handle.Session(),
		plans:         planRepository,
		planRef:       planRef,
		policy:        policy,
		executor:      executor,
		tempRoot:      scratchRoot,
		inspector:     inspector,
		permissions:   permissions,
		extensions:    extensionRuntime,
		compiled:      slices.Clone(options.Extensions),
		skillSettings: skillSettings,
		integration:   integration,
		generationID:  integration.ID(),
		trusted:       options.Trusted,
		observers:     newAgentObservers(options.AgentObservers),
		telemetry:     newTelemetryObservers(options.TelemetryObservers),
		admission:     newTeamAdmission(),
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
	if !openPolicy.teamWorker() {
		if err := subagent.Reconcile(ctx, repository, handle); err != nil {
			return nil, fmt.Errorf("coding runtime: reconcile subagents: %w", err)
		}
		notificationLimits := configured.Subagent.Limits
		if notificationLimits == (subagent.Limits{}) {
			notificationLimits = subagent.DefaultLimits()
		}
		runtime.notifications, err = subagent.NewNotificationInbox(
			runtime.session,
			handle.Metadata().ID,
			notificationLimits.MaxResultBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("coding runtime: open Agent notification inbox: %w", err)
		}
	}
	treeSnapshot, err := runtime.session.Tree(harness.TreeLimits{})
	if err != nil {
		return nil, err
	}
	bootstrap, err := BootstrapState(BootstrapOptions{
		SessionID:           handle.Metadata().ID,
		Provider:            resolved.Ref.Provider,
		ModelID:             resolved.Ref.Model,
		ContextWindow:       resolved.Limits.ContextWindow,
		Mode:                options.Config.Mode,
		Path:                runtime.session.Path(),
		HasPendingToolCalls: len(pending) > 0,
		Tree:                treeSnapshot,
	})
	if err != nil {
		return nil, err
	}
	runtime.state = bootstrap.State
	runtime.recovery = bootstrap.Recovery
	runtime.publisher = newEventPublisher(runtime)
	runtime.children = make(map[string]*childProjection)
	runtime.childControls = newChildControlRegistry()
	if !openPolicy.teamWorker() {
		runtime.teamRecovery, runtime.teamRecoveryErr = runtime.discoverTeamRecovery(ctx)
	}

	runtime.controller, err = approval.New(
		options.Workspace,
		runtime.session,
		&runtime.resolver,
		&runtime.pending,
		policy,
		executor,
		tools.NewShellHandlerForSandbox(
			options.Config.Sandbox,
			options.Config.SandboxWorkspaceWrite.Network,
		),
	)
	if err != nil {
		return nil, err
	}
	runtime.controller.SetPendingResultHook(runtime.hookAfterPendingTool)
	runtime.questions, err = question.NewController(&runtime.resolver)
	if err != nil {
		return nil, err
	}
	runtime.planReviews, err = planreview.NewController(
		runtime.plans,
		runtime.planRef,
		&runtime.resolver,
	)
	if err != nil {
		return nil, err
	}
	if !openPolicy.teamWorker() {
		childCompactionSettings, compactionDisabled := effectiveCompactionSettings(
			runtime.config.Compaction,
			resolved,
		)
		var (
			childCompaction   *harness.CompactionSettings
			childSummaryModel ai.LanguageModel
		)
		if compactionDisabled == "" {
			childCompaction = &childCompactionSettings
			childSummaryModel = requestPolicyModel{
				LanguageModel: baseModel,
				apply:         requestPolicy,
			}
		}
		runtime.subagents, err = subagent.New(subagent.Config{
			Context:        ctx,
			Repository:     repository,
			Parent:         handle,
			Tree:           tree,
			Model:          baseModel,
			GenerationID:   integration.ID(),
			SummaryModel:   childSummaryModel,
			Compaction:     childCompaction,
			RequestPolicy:  requestPolicy,
			Lifecycle:      runtime.subagentHookLifecycle(),
			Options:        configured.Subagent,
			AgentObservers: []func(context.Context, agent.Event){runtime.observers.observe},
			EventObservers: []subagent.AgentEventObserver{runtime.observeChildAgentEvent},
		})
		if err != nil {
			return nil, err
		}
		stack.add(runtime.subagents.Close)
		if err := runtime.recoverAgentNotifications(ctx); err != nil {
			return nil, err
		}
	}

	runtime.observeSessionOpened(ctx, resumed)
	if err := runtime.runSessionStart(ctx, resumed); err != nil {
		return nil, err
	}
	runtime.recordHookDiagnostics(ctx, nil, pendingHookDiagnostics)
	if !openPolicy.teamWorker() {
		runtime.publishTeamRecoveryCandidates(ctx, runtime.teamRecovery)
	}
	runtime.recordOpenDiagnostics(ctx, integration.connectionsSnapshot())
	runtime.recordAgentPluginDiagnostics(ctx, integration.agentPluginSnapshot())
	if !openPolicy.teamWorker() {
		runtime.startNotificationCoordinator(ctx)
	}
	stack.values = nil

	return runtime, nil
}

func resolveOpenModel(
	options OpenOptions,
	catalog modelcatalog.Catalog,
) (modelcatalog.ResolvedModel, error) {
	if options.Resolved.Ref.String() != "" {
		if options.Resolved.Ref != options.Config.Model {
			return modelcatalog.ResolvedModel{}, fmt.Errorf(
				"%w: resolved model does not match configuration selection",
				ErrRuntimeInvalid,
			)
		}

		return options.Resolved.Clone(), nil
	}

	if catalog == nil {
		return modelcatalog.ResolvedModel{}, fmt.Errorf("%w: missing model catalog", ErrRuntimeInvalid)
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
	if options.HooksEnvironment == nil {
		options.HooksEnvironment = os.Environ()
	} else {
		options.HooksEnvironment = slices.Clone(options.HooksEnvironment)
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
	if options.AgentProfileLimits.MaxEntries == 0 {
		options.AgentProfileLimits = agentprofile.DefaultLimits()
	}
	if options.AgentPluginLimits.MaxPlugins == 0 {
		options.AgentPluginLimits = agentplugin.DefaultLimits()
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

func configuredSubagentOptions(
	configured config.SubagentConfig,
	override subagent.ExecutionOptions,
) subagent.ExecutionOptions {
	if configured == (config.SubagentConfig{}) {
		configured = config.DefaultSubagentConfig()
	}
	limits := subagent.DefaultLimits()
	limits.MaxTurns = configured.MaxTurns
	limits.FinalizationTurns = min(2, configured.MaxTurns-1)
	limits.MaxTokens = configured.MaxTokens
	limits.MaxToolCalls = configured.MaxToolCalls
	limits.MaxDuration = time.Duration(configured.MaxDurationMinutes) * time.Minute
	result := subagent.ExecutionOptions{
		Limits:                       limits,
		MaxConcurrent:                configured.MaxConcurrent,
		MaxSpawnedPerRootInteraction: configured.MaxSpawnedPerRootInteraction,
		MaxAutoFollowUps:             configured.MaxAutoFollowUps,
	}
	if override.Limits != (subagent.Limits{}) {
		result.Limits = override.Limits
	}
	if override.MaxConcurrent != 0 {
		result.MaxConcurrent = override.MaxConcurrent
	}
	if override.MaxSpawnedPerRootInteraction != 0 {
		result.MaxSpawnedPerRootInteraction = override.MaxSpawnedPerRootInteraction
	}
	if override.MaxAutoFollowUps != 0 {
		result.MaxAutoFollowUps = override.MaxAutoFollowUps
	}

	return result
}

func newRuntimeScratchRoot(productRoot string) (*execution.PrivateTempRoot, error) {
	root, err := execution.NewPrivateTempRootOutside(os.TempDir(), []string{productRoot})
	if err != nil {
		return nil, fmt.Errorf("coding runtime: allocate private scratch root: %w", err)
	}

	return root, nil
}

func ensurePrivateApplicationRoot(root string) error {
	if err := ensureOwnerOnlyDirectory(root); err != nil {
		return fmt.Errorf("%w: product root must be a private directory: %w", ErrRuntimeInvalid, err)
	}

	return nil
}

func ensurePrivateTempRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%w: temp root must be absolute", ErrRuntimeInvalid)
	}
	if err := ensureOwnerOnlyDirectory(root); err != nil {
		return fmt.Errorf("%w: temp root must be a private directory: %w", ErrRuntimeInvalid, err)
	}

	return nil
}

func ensureOwnerOnlyDirectory(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("directory must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}

	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("directory must be an owner-only non-symlink directory")
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
	policy runtimeOpenPolicy,
) (*session.Handle, bool, error) {
	if policy.teamWorker() {
		lineage := policy.worker.lineage
		if options.Session.ID == "" {
			handle, err := repository.Create(ctx, session.CreateOptions{
				WorkspaceID:   options.Workspace.Identity().Key(),
				WorkspacePath: options.Workspace.Root(),
				Kind:          session.KindTeamWorker,
				TeamWorker:    &lineage,
			})

			return handle, false, err
		}

		handle, err := repository.OpenTeamWorker(ctx, session.OpenTeamWorkerOptions{
			ID: options.Session.ID, WorkspaceID: options.Workspace.Identity().Key(),
			Lineage: lineage,
		})

		return handle, true, err
	}

	if options.Session.ID == "" {
		handle, err := repository.Create(ctx, session.CreateOptions{
			WorkspaceID:   options.Workspace.Identity().Key(),
			WorkspacePath: options.Workspace.Root(),
			RetainEmpty:   options.Session.RetainEmpty,
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
		Sandbox: options.Config.Sandbox, Network: options.Config.SandboxWorkspaceWrite.Network,
		Approval:      options.Config.Approval,
		SandboxSource: source, Protected: []string{options.Paths.Root()},
	})
}

func openMCP(
	ctx context.Context,
	options OpenOptions,
	configured ExecutionOptions,
	tree *workspace.Tree,
	permissions *codingmcp.Permissions,
	pluginDefinitions codingmcp.Definitions,
) (*codingmcp.Connections, error) {
	definitions, err := codingmcp.LoadDefinitions(ctx, codingmcp.LoadOptions{
		Paths: options.Paths, Tree: tree, ProjectTrusted: options.Trusted,
		Limits: configured.MCPLimits,
	})
	if err != nil {
		return nil, err
	}
	definitions, err = definitions.Merge(configured.MCPDefinitions, configured.MCPLimits.MaxServers)
	if err != nil {
		return nil, err
	}
	definitions, pluginDiagnostics, err := mergeAgentPluginDefinitions(
		definitions, pluginDefinitions, configured.MCPLimits.MaxServers,
	)
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
		Diagnostics:    pluginDiagnostics,
	})
}

func mergeAgentPluginDefinitions(
	base codingmcp.Definitions,
	plugins codingmcp.Definitions,
	maximum int,
) (codingmcp.Definitions, []codingmcp.ConnectionDiagnostic, error) {
	values := base.List()
	seen := make(map[string]struct{}, len(values))
	for _, definition := range values {
		seen[definition.ID] = struct{}{}
	}
	diagnostics := make([]codingmcp.ConnectionDiagnostic, 0)
	for _, definition := range plugins.List() {
		if _, duplicate := seen[definition.ID]; duplicate {
			diagnostics = append(diagnostics, codingmcp.ConnectionDiagnostic{
				ServerID: definition.ID, Stage: "configuration", Code: "definition_duplicate",
				Message: "Agent Plugin server conflicts with another configured server and was ignored",
			})

			continue
		}
		if len(values) >= maximum {
			diagnostics = append(diagnostics, codingmcp.ConnectionDiagnostic{
				ServerID: definition.ID, Stage: "configuration", Code: "definition_limit",
				Message: "Agent Plugin server exceeds the client-wide server limit and was ignored",
			})

			continue
		}
		seen[definition.ID] = struct{}{}
		values = append(values, definition)
	}

	merged, err := codingmcp.NewDefinitions(values, maximum)
	if err != nil {
		return codingmcp.Definitions{}, nil, err
	}

	return merged, diagnostics, nil
}

func openRuntimeMCP(
	ctx context.Context,
	options OpenOptions,
	configured ExecutionOptions,
	tree *workspace.Tree,
	permissions *codingmcp.Permissions,
	policy runtimeOpenPolicy,
	pluginDefinitions codingmcp.Definitions,
) (*codingmcp.Connections, error) {
	if !policy.teamWorker() {
		return openMCP(ctx, options, configured, tree, permissions, pluginDefinitions)
	}

	return codingmcp.OpenConnections(ctx, nil, codingmcp.ConnectionOptions{
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

func activateRuntimeResources(
	ctx context.Context,
	runtime *extension.Runtime,
	loaded resource.Result,
	compiled []extension.Extension,
	policy runtimeOpenPolicy,
) (*extension.Activation, error) {
	if policy.teamWorker() {
		return runtime.Activate(ctx)
	}

	return activateResources(ctx, runtime, loaded, compiled)
}

func (r *Runtime) recordOpenDiagnostics(
	ctx context.Context,
	connections *codingmcp.Connections,
) {
	for _, diagnostic := range connections.Diagnostics() {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: componentMCP, Code: diagnostic.Code, Message: diagnostic.Message, Disabled: true,
		})
	}
}

func (r *Runtime) recordAgentPluginDiagnostics(ctx context.Context, plugins agentplugin.Result) {
	for _, diagnostic := range plugins.Diagnostics() {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: componentAgentPlugin,
			Code:      diagnostic.Code,
			Message:   diagnostic.Plugin + ": " + diagnostic.Message,
		})
	}
}

func (r *Runtime) recordDiagnostic(ctx context.Context, diagnostic IntegrationDiagnostic) {
	emitter := newEventEmitter(ctx, r, nil, false)
	_ = emitter.emit("", "", EventIntegrationDiagnostic, diagnostic)
}

// ObserveEvents atomically returns the current State and a subscription that
// begins after that State. It is the preferred bootstrap and resubscribe API
// for interactive frontends.
func (r *Runtime) ObserveEvents() (EventObservation, error) {
	if r == nil || r.publisher == nil {
		return EventObservation{}, ErrRuntimeClosed
	}

	return r.publisher.observe()
}

// SubscribeEvents replays Runtime events after cursor when they remain in the
// bounded hub. Callers receiving [ErrEventGap] must use [Runtime.ObserveEvents].
func (r *Runtime) SubscribeEvents(afterCursor uint64) (*EventSubscription, error) {
	if r == nil || r.publisher == nil {
		return nil, ErrRuntimeClosed
	}

	subscription, _, err := r.publisher.hub.subscribe(afterCursor)

	return subscription, err
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

func (r *Runtime) currentOperatingMode() OperatingMode {
	if r == nil {
		return ModeAgent
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state.Mode
}

// ReplacementPreflight verifies that the Runtime can be replaced without
// interrupting an active interaction, approval, question, Plan review, or Team
// owner. It does not mutate Runtime state.
//
//nolint:gocyclo // Replacement safety keeps every lifecycle gate explicit.
func (r *Runtime) ReplacementPreflight(ctx context.Context) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return fmt.Errorf("%w: Team Worker resources are fixed", ErrRuntimeInvalid)
	}
	if r.teamGuard.active() {
		return fmt.Errorf("%w: Lead resources are fixed while a Team is active", ErrTeamActive)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return stateError("replace Runtime", r.state.Phase, ErrRuntimeClosed)
	}
	if r.active != nil || r.interaction != nil || r.recovery.PendingID != "" ||
		r.state.Phase != PhaseIdle || r.state.Compaction.Active ||
		r.state.Approval.Kind != ApprovalNone || r.state.Question.Required != nil ||
		r.state.PlanReview.Required != nil {
		return stateError("replace Runtime", r.state.Phase, ErrRuntimeBusy)
	}

	return nil
}

// SetMode changes the process-local capability policy at an idle boundary.
// Existing interactions retain the mode leased when they started.
//
//nolint:gocyclo // Runtime lifecycle and pending-state guards remain explicit.
func (r *Runtime) SetMode(ctx context.Context, mode OperatingMode) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return fmt.Errorf("%w: Team Worker mode is fixed", ErrRuntimeInvalid)
	}
	if r.teamGuard.active() {
		return fmt.Errorf("%w: Lead mode is fixed while a Team is active", ErrTeamActive)
	}
	if !validOperatingMode(mode) {
		return fmt.Errorf("%w: unsupported operating mode %q", ErrRuntimeInvalid, mode)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return stateError("set mode", phase, ErrRuntimeClosed)
	}
	if r.active != nil || r.interaction != nil || r.state.Phase != PhaseIdle ||
		r.state.Compaction.Active || r.state.Approval.Kind != ApprovalNone ||
		r.state.Question.Required != nil || r.state.PlanReview.Required != nil {
		phase := r.state.Phase
		r.mu.Unlock()

		return stateError("set mode", phase, ErrRuntimeBusy)
	}
	if r.state.Mode == mode {
		r.mu.Unlock()

		return nil
	}

	operationCtx, cancel := context.WithCancel(ctx)
	operation := &runtimeOperation{cancel: cancel, done: make(chan struct{})}
	r.active = operation
	r.mu.Unlock()
	defer r.endOperation(operation)

	emitter := newEventEmitter(operationCtx, r, nil, false)
	if err := emitter.emit("", "", EventModeChanged, ModeChanged{Mode: mode}); err != nil {
		return err
	}

	r.mu.Lock()
	r.config.Mode = mode
	r.mu.Unlock()

	return nil
}

// Prompt starts one new interaction. Iteration owns cancellation and cleanup.
func (r *Runtime) Prompt(ctx context.Context, messages ...ai.Message) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationPrompt, runtimeResolution{}, messages, nil)
}

// Continue reconciles a durable pending interaction after reopening a session.
func (r *Runtime) Continue(ctx context.Context) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationContinue, runtimeResolution{}, nil, nil)
}

// Resolve applies one explicit approval decision and continues only when the
// durable pending suffix becomes ready.
func (r *Runtime) Resolve(
	ctx context.Context,
	resolution approval.Resolution,
) iter.Seq2[Event, error] {
	return r.runSequence(ctx, operationResolve, runtimeResolution{approval: resolution}, nil, nil)
}

// ResolveQuestion durably records one exact structured response and resumes
// the paused interaction.
func (r *Runtime) ResolveQuestion(
	ctx context.Context,
	resolution question.Resolution,
) iter.Seq2[Event, error] {
	return r.runSequence(
		ctx,
		operationResolveQuestion,
		runtimeResolution{question: question.CloneResolution(resolution)},
		nil,
		nil,
	)
}

// RejectQuestion records an explicit cancellation and lets the Agent decide
// whether it can continue without the requested input.
func (r *Runtime) RejectQuestion(
	ctx context.Context,
	requestID string,
	schemaDigest string,
) iter.Seq2[Event, error] {
	return r.runSequence(
		ctx,
		operationRejectQuestion,
		runtimeResolution{rejection: &questionRejection{
			requestID: requestID, schemaDigest: schemaDigest,
		}},
		nil,
		nil,
	)
}

// ResolvePlanReview durably records one explicit Plan review decision and
// resumes the paused Plan interaction.
func (r *Runtime) ResolvePlanReview(
	ctx context.Context,
	resolution planreview.Resolution,
) iter.Seq2[Event, error] {
	return r.runSequence(
		ctx,
		operationResolvePlanReview,
		runtimeResolution{planReview: planreview.CloneResolution(resolution)},
		nil,
		nil,
	)
}

type runtimeResolution struct {
	approval   approval.Resolution
	question   question.Resolution
	planReview planreview.Resolution
	rejection  *questionRejection
}

type questionRejection struct {
	requestID    string
	schemaDigest string
}

type runtimeOperationKind string

const (
	operationPrompt            runtimeOperationKind = "prompt"
	operationDirectAgent       runtimeOperationKind = "run Agent"
	operationContinue          runtimeOperationKind = "continue"
	operationResolve           runtimeOperationKind = "resolve"
	operationResolveQuestion   runtimeOperationKind = "resolve question"
	operationRejectQuestion    runtimeOperationKind = "reject question"
	operationResolvePlanReview runtimeOperationKind = "resolve Plan review"
	operationPreview           runtimeOperationKind = "preview compaction"
	operationCompact           runtimeOperationKind = "compact"
	operationNavigate          runtimeOperationKind = "navigate"
	operationFork              runtimeOperationKind = "fork"
	operationAgentNotification runtimeOperationKind = "agent notification"
	operationTeamPropose       runtimeOperationKind = "propose Team"
	operationTeamProposalAgent runtimeOperationKind = "generate Team proposal"
	operationTeamConfirm       runtimeOperationKind = "confirm Team"
	operationTeamResume        runtimeOperationKind = "resume Team"
	operationTeamIntegration   runtimeOperationKind = "integrate Team results"
	operationTeamCleanup       runtimeOperationKind = "clean up Team resources"
)

func (r *Runtime) runSequence(
	ctx context.Context,
	kind runtimeOperationKind,
	resolution runtimeResolution,
	messages []ai.Message,
	notification *notificationOperation,
) iter.Seq2[Event, error] {
	cloned := cloneMessages(messages)

	return func(yield func(Event, error) bool) {
		if r == nil {
			yield(Event{}, ErrRuntimeClosed)
			return
		}

		r.run(ctx, kind, resolution, cloned, notification, yield)
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
func (r *Runtime) Reload(ctx context.Context) (returnErr error) {
	if r == nil {
		return ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return fmt.Errorf("%w: Team Worker resources are fixed", ErrRuntimeInvalid)
	}
	if r.teamGuard.active() {
		return fmt.Errorf("%w: Lead resources are fixed while a Team is active", ErrTeamActive)
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

	candidate, err := r.buildIntegrationCandidate(reloadCtx)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, candidate.abort(context.WithoutCancel(reloadCtx)))
	}()
	r.mu.Lock()
	if r.closed || r.closing || r.active != operation || r.interaction != nil {
		phase := r.state.Phase
		r.mu.Unlock()

		return stateError("reload", phase, ErrRuntimeBusy)
	}

	next, err := candidate.publish()
	if err != nil {
		r.mu.Unlock()

		return err
	}
	previous := r.integration
	r.integration = next
	r.generationID = next.ID()
	r.projectInstructions = next.projectInstructionsSnapshot()
	r.mu.Unlock()

	r.recordOpenDiagnostics(ctx, next.connectionsSnapshot())
	r.recordAgentPluginDiagnostics(ctx, next.agentPluginSnapshot())
	retireErr := previous.retire(context.WithoutCancel(ctx))
	if candidate.activationErr != nil {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: componentIntegration,
			Code:      "extension_retire_failed",
			Message:   "The new integration generation was published; retired Extension cleanup failed",
		})
	}
	if retireErr != nil {
		r.recordDiagnostic(ctx, IntegrationDiagnostic{
			Component: componentIntegration,
			Code:      "generation_retire_failed",
			Message:   "The new integration generation was published; retired generation cleanup failed",
		})
	}

	postPublicationErr := errors.Join(candidate.activationErr, retireErr)
	if postPublicationErr != nil {
		return &ReloadPublicationError{
			GenerationID: next.ID(),
			Err:          postPublicationErr,
		}
	}

	return nil
}
