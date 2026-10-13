//nolint:gocyclo,funlen,wsl_v5 // Child control construction keeps all isolation boundaries adjacent.
package coding

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/hooks"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// childScopeFactory contains only application-owned, immutable-or-shared-safe
// dependencies needed to build a child control scope. In particular it has no
// parent approval controller, parent pending runner, parent harness, or parent
// interaction pointer.
type childScopeFactory struct {
	repository        *session.Repository
	fullCompaction    *compaction.Policy
	workspace         workspace.Workspace
	tree              *workspace.Tree
	toolLimits        tools.Limits
	policy            execution.Policy
	executor          *execution.Executor
	sandbox           config.SandboxMode
	network           config.SandboxNetworkMode
	requestPolicy     func(*ai.Request)
	toolTimeout       time.Duration
	inspector         *git.Inspector
	hooks             []hooks.Definition
	privateHooks      []hooks.Definition
	hookRunner        hooks.Runner
	onHookDiagnostics func(context.Context, []hooks.Diagnostic)
	model             ai.LanguageModel
	mode              OperatingMode
	// streamContinuation is the child model's own recovery opt-in, taken from
	// the binding for the model the child runs rather than from its parent's.
	streamContinuation bool
	// streamIdleTimeout is that model's streaming idle bound, which the child's
	// recovery episode is derived from. Zero leaves the transport default.
	streamIdleTimeout time.Duration
	mcpEntries        []catalog.Entry
	// backgroundTasks is the session's one background-task registry. A child
	// watch registers on it, so a monitor a child starts is the same kind of
	// session task the conversation's own watches are, and its events reach the
	// one conversation the Runtime owns.
	backgroundTasks *background.Registry
	// killBackground is the one control operation a background-task stop routes
	// through, shared with the conversation's own stop Tool, and
	// knownBackgroundIDs is the enumeration a not-found stop names.
	killBackground     tools.KillBackground
	knownBackgroundIDs tools.KnownBackgroundIDs
	// executorConfig and gitConfig let a child that runs outside the parent
	// workspace build its own process executor and Git inspector there.
	executorConfig execution.ExecutorConfig
	gitConfig      git.Config
	// worktrees provisions a preserved child Worktree on first use.
	worktrees *childWorktrees
	// messages is the tree's child messaging coordinator. It is nil while the
	// messaging gate is closed, which keeps the messaging Tool unoffered.
	messages *subagent.MessageCoordinator
	// wakeManager, wakeDispatcher, and wakeObserver let a child sender start
	// the continuation of a completed sibling. The woken child's lifecycle
	// belongs to the sender's own Run rather than the conversation's, so
	// wakeObserver projects it without entering the conversation timeline.
	// wakeDispatcher is a provider because the dispatcher is built from this
	// factory.
	wakeManager    *subagent.Manager
	wakeDispatcher func() subagent.Dispatcher
	wakeObserver   subagent.Observer
	// directoryResources is the child-scoped binding set the constructed scope
	// takes ownership of. It is nil for a child in the parent workspace.
	directoryResources *childDirectoryResources
	// toolSearchNameFor resolves the discovery tool name for the provider that
	// serves this child; nil uses catalog.DefaultToolSearchName.
	toolSearchNameFor func(ai.Provider) string
	controls          *childControlRegistry
	// onPauseChanged projects only the child Session phase transition. It is
	// deliberately separate from parent interaction state so a child waiting
	// for input can be surfaced and controlled without pausing its parent.
	onPauseChanged func(context.Context, string, bool) error
	// failOnInput is set only for a non-interactive direct invocation. The
	// child still records its own pending approval or question, but its runner
	// returns the corresponding fail-closed error instead of waiting forever
	// for a resolver that the caller cannot provide.
	failOnInput bool
	// onWorkspaceChanged only projects a child-owned, bounded change report.
	// It never routes parent controls or parent interaction state into the
	// child scope.
	onWorkspaceChanged func(context.Context, string, WorkspaceChanged) error
}

func (f childScopeFactory) clone() childScopeFactory {
	f.hooks = slices.Clone(f.hooks)
	f.privateHooks = slices.Clone(f.privateHooks)
	f.mcpEntries = cloneCatalogEntries(f.mcpEntries)
	if f.fullCompaction != nil {
		policy := *f.fullCompaction
		f.fullCompaction = &policy
	}

	return f
}

func cloneCatalogEntries(values []catalog.Entry) []catalog.Entry {
	cloned := slices.Clone(values)
	for index := range cloned {
		cloned[index].Tags = slices.Clone(cloned[index].Tags)
	}

	return cloned
}

// childControlRegistry is a bounded Runtime-local route table. It stores only
// child-owned control scopes and never falls back to a parent resolver.
type childControlRegistry struct {
	mu     sync.RWMutex
	values map[string]*childControlScope
}

func newChildControlRegistry() *childControlRegistry {
	return &childControlRegistry{values: make(map[string]*childControlScope)}
}

func (r *childControlRegistry) add(scope *childControlScope) error {
	if r == nil || scope == nil || scope.childSessionID == "" {
		return fmt.Errorf("%w: invalid child control scope", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.values[scope.childSessionID]; exists {
		return fmt.Errorf("%w: duplicate child control scope", ErrRuntimeInvalid)
	}
	r.values[scope.childSessionID] = scope

	return nil
}

func (r *childControlRegistry) remove(childSessionID string, scope *childControlScope) {
	if r == nil || childSessionID == "" {
		return
	}

	r.mu.Lock()
	if r.values[childSessionID] == scope {
		delete(r.values, childSessionID)
	}
	r.mu.Unlock()
}

func (r *childControlRegistry) get(childSessionID string) *childControlScope {
	if r == nil || childSessionID == "" {
		return nil
	}

	r.mu.RLock()
	scope := r.values[childSessionID]
	r.mu.RUnlock()

	return scope
}

// ChildPauseKind identifies the independently resolvable input owner of a
// live custom child execution.
type ChildPauseKind string

const (
	// ChildPauseNone means the child has no unresolved input boundary.
	ChildPauseNone ChildPauseKind = ""
	// ChildPauseApproval means the child is waiting for its own approval decision.
	ChildPauseApproval ChildPauseKind = "approval"
	// ChildPauseQuestion means the child is waiting for its own structured answer.
	ChildPauseQuestion ChildPauseKind = "question"
)

// ChildControlState is the bounded, targetable control projection for one
// running custom child. It intentionally excludes raw tool output and profile
// instructions; durable detail remains in the child session.
type ChildControlState struct {
	ChildSessionID string
	Pause          ChildPauseKind
	Approval       approval.State
	Question       *question.Request
}

// Clone returns a detached child-control projection.
func (s ChildControlState) Clone() ChildControlState {
	if s.Question != nil {
		value := question.CloneRequest(*s.Question)
		s.Question = &value
	}

	return s
}

// childControlScope is both a subagent.Runner and the owner of one isolated
// approval/question/pending/harness control plane.
type childControlScope struct {
	mu sync.Mutex

	childSessionID string
	plan           subagent.ExecutionPlan
	child          *harness.Session
	childHandle    *session.Handle
	historyAllowed bool
	fixedTokens    int
	fixedTokensErr error
	lifecycleDone  <-chan struct{}
	cancel         context.CancelFunc
	generation     *IntegrationGeneration
	factory        childScopeFactory
	// directoryResources owns the child-scoped tree and Git inspector when the
	// child runs outside the parent workspace. It is nil for a shared child.
	directoryResources *childDirectoryResources
	// directoryOverride is the child's effective working directory when it
	// differs from the parent workspace, and worktreePath is its preserved
	// Worktree when it has one.
	directoryOverride string
	worktreePath      string

	resolver    activeResolver
	pending     pendingRunner
	controller  *approval.Controller
	questions   *question.Controller
	harness     *harness.Harness
	changes     *interactionChangeTracker
	changesOnce sync.Once
	changesErr  error
	hooks       childHookScope
	guard       childToolGuard
	validator   subagent.OutputValidator

	pause       ChildPauseKind
	approval    approval.State
	question    *question.Request
	resume      chan struct{}
	outputValue any
	outputText  string
	closed      bool
	closeOnce   sync.Once
	closeErr    error

	// ownership is this child's own durable ownership. It binds the child's
	// messaging sender identity.
	ownership subagent.Ownership
	// inbound holds admitted messages that no live hand-off has consumed yet.
	// They are injected at this child's next turn boundary.
	inbound []inboundMessage
	// runActive reports that a prompt run is in flight, so an admitted message
	// can be handed to the harness immediately. runEnded reports that the run
	// has finished, so this scope accepts nothing further.
	runActive bool
	runEnded  bool
}

// inboundMessage is one admitted message waiting for this child's next turn
// boundary. release frees its tree-wide admission once the message is consumed
// or dropped.
type inboundMessage struct {
	id       string
	delivery subagent.MessageDelivery
	text     string
	release  func()
}

func newChildControlScope(
	ctx context.Context,
	factory childScopeFactory,
	generation *IntegrationGeneration,
	ambient []catalog.Descriptor,
	skills map[string]harness.Skill,
	input subagent.DispatchInput,
) (_ *childControlScope, returnErr error) {
	if input.Child == nil || input.Child.Session() == nil || input.OnEvent == nil ||
		generation == nil || factory.executor == nil || factory.tree == nil || factory.inspector == nil || factory.model == nil ||
		factory.controls == nil {
		return nil, fmt.Errorf("%w: incomplete child control dependencies", subagent.ErrInvalid)
	}
	if factory.fullCompaction != nil {
		if factory.repository == nil {
			return nil, fmt.Errorf("%w: child compaction repository is unavailable", subagent.ErrInvalid)
		}
		if _, err := factory.fullCompaction.Threshold(); err != nil {
			return nil, fmt.Errorf("coding child: invalid compaction policy for selected model: %w", err)
		}
	}
	if err := subagent.ValidateExecutionPlan(input.Plan); err != nil {
		return nil, err
	}
	if err := generation.acquire(); err != nil {
		return nil, err
	}
	acquired := true
	defer func() {
		if returnErr != nil && acquired {
			returnErr = errors.Join(returnErr, generation.release(context.WithoutCancel(ctx)))
		}
	}()

	lifecycle, cancel := context.WithCancel(context.WithoutCancel(ctx))
	scope := &childControlScope{
		childSessionID: input.Child.Metadata().ID,
		plan:           input.Plan.Clone(),
		child:          input.Child.Session(),
		childHandle:    input.Child,
		lifecycleDone:  lifecycle.Done(),
		cancel:         cancel,
		generation:     generation,
		factory:        factory.clone(),
		resume:         make(chan struct{}, 1),

		directoryResources: factory.directoryResources,
		directoryOverride:  strings.TrimSpace(input.Directory),
		worktreePath:       input.WorktreePath,
		ownership:          input.Request.Ownership,
	}
	scope.hooks = newChildHookScope(
		factory.hooks, factory.privateHooks, factory.hookRunner,
		scope.childSessionID, factory.workspace.Root(), factory.onHookDiagnostics,
	)
	validator, err := subagent.NewOutputValidator(scope.plan.Output, scope.plan.Limits)
	if err != nil {
		cancel()
		return nil, err
	}
	scope.validator = validator

	handlers := []approval.Handler{tools.NewShellHandlerForSandbox(factory.sandbox, factory.network)}
	if factory.backgroundTasks != nil {
		handlers = append(handlers, tools.NewMonitorHandlerForSandbox(
			factory.backgroundTasks,
			factory.sandbox,
			factory.network,
		))
	}
	controller, err := approval.New(
		factory.workspace,
		scope.child,
		&scope.resolver,
		&scope.pending,
		factory.policy,
		factory.executor,
		handlers...,
	)
	if err != nil {
		cancel()
		return nil, err
	}
	scope.controller = controller
	scope.controller.SetPendingResultHook(scope.afterPendingTool)
	questions, err := question.NewController(&scope.resolver)
	if err != nil {
		cancel()
		return nil, err
	}
	scope.questions = questions

	childCatalog, skillCatalog, descriptors, err := scope.buildCatalog(ctx, ambient, skills)
	if err != nil {
		cancel()
		return nil, err
	}
	scope.guard.risks = childToolRisks(descriptors)
	scope.changes = newInteractionChangeTracker(factory.inspector, descriptors)
	policy := catalog.AllowAll(factory.workspace.Identity().Key(), catalog.RiskPrivileged)
	searchOptions := catalog.ToolSearchOptions{Enabled: scope.plan.ToolSearch}
	if factory.toolSearchNameFor != nil {
		searchOptions.Name = factory.toolSearchNameFor(factory.model.Provider())
	}
	search, err := catalog.NewToolSearch(childCatalog, policy, searchOptions)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: tool search: %w", ErrRuntimeInvalid, err)
	}
	searchName := ""
	if scope.plan.ToolSearch {
		searchName = search.Name()
	}
	executableTools, err := search.ExecutableTools(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	allTools, err := childCatalog.Snapshot(ctx, policy)
	if err != nil {
		cancel()
		return nil, err
	}
	allTools = appendUniqueTools(allTools, executableTools...)

	preloaded, err := activateExplicitSkills(skillCatalog, scope.plan.PreloadedSkills)
	if err != nil {
		cancel()
		return nil, err
	}
	composed := extension.ComposeHooks(
		extension.Hooks{BeforeTool: scope.guard.before(scope.plan.Limits)},
		extension.Hooks{BeforeTool: scope.hooks.beforeTool},
		extension.Hooks{BeforeTool: leasedToolGuard(scope.factory.mode, descriptors, searchName, allTools)},
		extension.Hooks{BeforeTool: scope.questions.BeforeTool},
		extension.Hooks{BeforeTool: scope.changes.beforeTool},
		extension.Hooks{BeforeTool: scope.controller.BeforeTool},
		extension.Hooks{PrepareTurn: search.PrepareTurn},
		// Harness has already flushed TurnCompleted before this hook. Search
		// updates remain authoritative; compaction replaces messages only.
		extension.Hooks{PrepareTurn: scope.prepareCompaction},
		extension.Hooks{AfterTool: scope.hooks.afterTool},
	)
	failureGuard := newToolFailureGuard()
	composed.BeforeTool = failureGuard.wrapBeforeTool(composed.BeforeTool)
	composed.AfterTool = failureGuard.wrapAfterTool(composed.AfterTool)
	if err := scope.pending.set(allTools, composed, factory.toolTimeout); err != nil {
		cancel()
		return nil, err
	}

	options := append(
		composed.AgentOptions(),
		agent.WithName("subagent/"+scope.plan.Identity.ID),
		agent.WithMaxTurns(scope.plan.Limits.MaxTurns),
		agent.WithMaxTokens(scope.plan.Limits.MaxTokens),
		streamRecoveryOption(factory.streamContinuation, factory.streamIdleTimeout),
		agent.WithParallelTools(1),
		agent.WithStopWhen(scope.guard.stopWhen),
		agent.WithToolTimeout(factory.toolTimeout),
		agent.WithRequest(scope.requestPolicy),
		agent.WithOutputGuardrail("subagent_result", scope.validateOutput),
	)
	systemSuffix := strings.TrimSpace(input.SystemSuffix)
	if preloaded != "" {
		if systemSuffix != "" {
			systemSuffix += "\n"
		}
		systemSuffix += preloaded
	}
	h, err := harness.New(
		compaction.GuardModel(factory.model, factory.fullCompaction),
		scope.child,
		harness.WithSystem(scope.plan.Instructions),
		harness.WithSystemSuffix(systemSuffix),
		harness.WithTools(executableTools...),
		harness.WithSkillCatalog(skillCatalog),
		harness.WithOnEvent(func(eventCtx context.Context, event agent.Event) {
			if _, ok := event.Payload().(agent.RunCompleted); ok {
				search.Forget(event.RunID)
			}
			input.OnEvent(eventCtx, event)
		}),
		harness.WithAgentOptions(options...),
	)
	if err != nil {
		cancel()
		return nil, err
	}
	scope.harness = h
	scope.resolver.set(h)
	if err := factory.controls.add(scope); err != nil {
		cancel()
		return nil, err
	}
	acquired = false

	return scope, nil
}

func (s *childControlScope) buildCatalog(
	ctx context.Context,
	ambient []catalog.Descriptor,
	skills map[string]harness.Skill,
) (*catalog.Catalog, *harness.SkillCatalog, []catalog.Descriptor, error) {
	if s == nil || s.controller == nil || s.questions == nil {
		return nil, nil, nil, fmt.Errorf("%w: child scope is incomplete", subagent.ErrInvalid)
	}
	if err := validateChildPlanCapabilities(s.plan, ambient); err != nil {
		return nil, nil, nil, err
	}

	shell, found := s.controller.Tool("shell")
	if !found {
		return nil, nil, nil, fmt.Errorf("%w: child controlled shell is unavailable", subagent.ErrInvalid)
	}
	localOptions := []tools.CatalogOption{tools.WithControlledShell(shell)}
	// A child keeps the watch surface exactly when its capability mode does, and
	// every watch it starts registers on the session's own registry, so its
	// events land in the one conversation this Runtime owns rather than opening
	// a second delivery queue.
	if monitor, ok := s.controller.Tool(tools.MonitorName); ok {
		localOptions = append(localOptions, tools.WithControlledMonitor(monitor))
		if s.factory.killBackground != nil {
			localOptions = append(localOptions, tools.WithKillBackground(s.factory.killBackground))
		}
		if s.factory.knownBackgroundIDs != nil {
			localOptions = append(
				localOptions,
				tools.WithKnownBackgroundIDs(s.factory.knownBackgroundIDs),
			)
		}
	}
	local, err := tools.NewCatalog(s.factory.tree, s.factory.toolLimits, localOptions...)
	if err != nil {
		return nil, nil, nil, err
	}
	questionCatalog, err := s.questions.Catalog()
	if err != nil {
		return nil, nil, nil, err
	}

	selectedSkills := make([]harness.Skill, 0, len(s.plan.Skills))
	for _, name := range s.plan.Skills {
		skill, exists := skills[name]
		if !exists {
			return nil, nil, nil, fmt.Errorf("%w: planned Skill %q is unavailable", subagent.ErrInvalid, name)
		}
		selectedSkills = append(selectedSkills, skill)
	}
	skillCatalog, err := harness.NewSkillCatalog(selectedSkills...)
	if err != nil {
		return nil, nil, nil, err
	}

	entries := make([]catalog.Entry, 0, len(s.plan.Capabilities))
	appendSelected := func(value *catalog.Catalog) error {
		if value == nil {
			return nil
		}
		selected, selectErr := filterCatalog(ctx, value, func(descriptor catalog.Descriptor) bool {
			return planSelectsDescriptor(s.plan, descriptor)
		})
		if selectErr != nil {
			return selectErr
		}
		selectedEntries, snapshotErr := selected.Snapshot(
			ctx,
			catalog.AllowAll(s.factory.workspace.Identity().Key(), catalog.RiskPrivileged),
		)
		if snapshotErr != nil {
			return snapshotErr
		}
		for _, tool := range selectedEntries {
			descriptor, lookupErr := catalogDescriptor(value, tool.Decl().Name)
			if lookupErr != nil {
				return lookupErr
			}
			entries = append(entries, catalog.Entry{
				Tool: tool, Source: descriptor.Source, Risk: descriptor.Risk, Tags: slices.Clone(descriptor.Tags),
			})
		}

		return nil
	}
	if err := appendSelected(local); err != nil {
		return nil, nil, nil, err
	}
	if err := appendSelected(questionCatalog); err != nil {
		return nil, nil, nil, err
	}
	historyCatalog, err := s.historyCatalog(ambient)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := appendSelected(historyCatalog); err != nil {
		return nil, nil, nil, err
	}
	if planSelectsDescriptor(s.plan, catalog.Descriptor{
		Name:   harness.SkillToolName,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.skills"},
		Risk:   catalog.RiskRead,
	}) {
		skillTool, skillErr := harness.NewSkillTool(skillCatalog)
		if skillErr != nil {
			return nil, nil, nil, skillErr
		}
		entries = append(entries, catalog.Local("coding.skills", catalog.RiskRead, skillTool)...)
	}
	entries = append(entries, s.messageCatalogEntries()...)
	for _, entry := range s.factory.mcpEntries {
		// Never reuse an ambient history implementation, even if accidentally
		// supplied in this integration snapshot. Its binding may belong to a parent.
		if entry.Tool.Decl().Name == tools.HistoryName {
			continue
		}
		descriptor := catalog.Descriptor{
			Name: entry.Tool.Decl().Name, Source: entry.Source, Risk: entry.Risk, Tags: slices.Clone(entry.Tags),
		}
		if planSelectsDescriptor(s.plan, descriptor) {
			entries = append(entries, catalog.Entry{
				Tool: entry.Tool, Source: entry.Source, Risk: entry.Risk, Tags: slices.Clone(entry.Tags),
			})
		}
	}
	// Depth is capped at one: a child never receives a delegation tool, so
	// recursion cannot start below the root conversation.
	childCatalog, err := catalog.New(entries...)
	if err != nil {
		return nil, nil, nil, err
	}
	descriptors, err := childCatalog.Search(
		ctx,
		catalog.AllowAll(s.factory.workspace.Identity().Key(), catalog.RiskPrivileged),
		"",
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateChildPlanCapabilities(s.plan, descriptors); err != nil {
		return nil, nil, nil, err
	}
	s.historyAllowed = slices.ContainsFunc(descriptors, childHistoryDescriptor)

	return childCatalog, skillCatalog, descriptors, nil
}

// messageCatalogEntries returns this child's messaging Tool when its plan
// selects one and the messaging gate is open. The child's own binding carries
// its sender identity, so a message it sends is attributed to this child.
func (s *childControlScope) messageCatalogEntries() []catalog.Entry {
	if s == nil || s.plan.Identity.ID == "" || s.factory.messages == nil {
		return nil
	}
	descriptor := catalog.Descriptor{
		Name:   SendMessageToolName,
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: messageCatalogID},
		Risk:   catalog.RiskRead,
	}
	if !planSelectsDescriptor(s.plan, descriptor) {
		return nil
	}
	// A child-initiated wake is attributed to this child's own Tool call, not to
	// the conversation's Run, so the woken child is projected without entering
	// the conversation timeline.
	identity := subagent.MessageSenderIdentity{
		SessionID: s.childSessionID,
		AttemptID: s.childSessionID,
		Child:     true,
		Ownership: s.ownership,
		AgentID:   s.plan.Identity.ID,
	}
	tool := newSendMessageTool(
		s.factory.messages,
		subagent.NewMessageBinding(identity, s.childWakePort(identity)),
	)
	if tool == nil {
		return nil
	}

	return catalog.Local(messageCatalogID, catalog.RiskRead, tool)
}

// childWakePort starts the continuation of a completed sibling on behalf of
// this child. A wake's lifecycle must belong to an active Tool call of the Run
// that asked for it, and a child's own Run is not the conversation's Run, so
// the woken child is attributed to this child's Tool call and only its own
// projection is opened. A factory with no wake route keeps the port closed,
// which refuses the wake rather than dropping it.
func (s *childControlScope) childWakePort(sender subagent.MessageSenderIdentity) subagent.WakePort {
	if s == nil || s.factory.wakeManager == nil || s.factory.wakeObserver == nil ||
		s.factory.wakeDispatcher == nil {
		return nil
	}

	return func(ctx context.Context, target subagent.WakeTarget) error {
		return startSubagentWake(
			ctx, s.factory.wakeManager, s.factory.wakeDispatcher(), s.factory.wakeObserver,
			subagent.Ownership{
				ParentSessionID:     sender.Ownership.ParentSessionID,
				ParentInteractionID: sender.SessionID,
				ParentRunID:         target.Call.RunID,
				ParentToolCallID:    target.Call.ID,
				RootInteractionID:   sender.Ownership.RootInteractionID,
			},
			target,
		)
	}
}

func childHistoryDescriptor(descriptor catalog.Descriptor) bool {
	return descriptor.Name == tools.HistoryName && descriptor.Source.Kind == catalog.SourceLocal &&
		descriptor.Source.ID == "coding.history" && descriptor.Risk == catalog.RiskRead
}

func (s *childControlScope) historyCatalog(ambient []catalog.Descriptor) (*catalog.Catalog, error) {
	for _, descriptor := range ambient {
		if !childHistoryDescriptor(descriptor) || !planSelectsDescriptor(s.plan, descriptor) {
			continue
		}
		if s.factory.repository == nil || s.childHandle == nil {
			return nil, fmt.Errorf("%w: child history repository is unavailable", subagent.ErrInvalid)
		}
		store, err := s.factory.repository.Archives(s.childHandle)
		if err != nil {
			return nil, err
		}
		history := tools.NewHistoryTool(func(ctx context.Context) (*session.ArchiveReader, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			snapshot, err := s.child.SnapshotContext()
			if err != nil {
				return nil, err
			}
			return store.Bind(compaction.ArchiveIDs(snapshot.Entries))
		})
		return catalog.New(catalog.Entry{
			Tool: history, Source: descriptor.Source, Risk: descriptor.Risk, Tags: slices.Clone(descriptor.Tags),
		})
	}
	return nil, nil
}

func catalogDescriptor(value *catalog.Catalog, name string) (catalog.Descriptor, error) {
	descriptors, err := value.Search(context.Background(), catalog.AllowAll("coding-child-descriptor", catalog.RiskPrivileged), "")
	if err != nil {
		return catalog.Descriptor{}, err
	}
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, nil
		}
	}

	return catalog.Descriptor{}, fmt.Errorf("%w: selected tool descriptor disappeared", subagent.ErrInvalid)
}

func validateChildPlanCapabilities(plan subagent.ExecutionPlan, ambient []catalog.Descriptor) error {
	for _, capability := range plan.Capabilities {
		found := false
		for _, descriptor := range ambient {
			if descriptor.Name == capability.WireName &&
				capabilitySource(descriptor.Source) == capability.Source &&
				capabilityRisk(descriptor.Risk) == capability.Risk &&
				delegableDescriptor(descriptor) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: plan capability %q is not in the captured ambient set", subagent.ErrInvalid, capability.WireName)
		}
	}

	return nil
}

func planSelectsDescriptor(plan subagent.ExecutionPlan, descriptor catalog.Descriptor) bool {
	for _, capability := range plan.Capabilities {
		if capability.WireName == descriptor.Name &&
			capability.Source == capabilitySource(descriptor.Source) &&
			capability.Risk == capabilityRisk(descriptor.Risk) {
			return true
		}
	}

	return false
}

func (s *childControlScope) Run(ctx context.Context, task string) (subagent.RunnerResult, error) {
	if s == nil || s.harness == nil {
		return subagent.RunnerResult{}, fmt.Errorf("%w: child control scope is unavailable", subagent.ErrClosed)
	}
	messages, err := s.initialMessages(task)
	if err != nil {
		return s.finishRun(ctx, subagent.RunnerResult{}, err)
	}
	s.beginRun()
	defer s.markRunEnded()
	for {
		// A message admitted before this run started, or while it was paused,
		// is injected at the next turn boundary instead of being lost.
		messages = append(messages, s.drainPending()...)
		result, err := s.harness.PromptMessagesRun(ctx, messages...)
		messages = nil
		if err != nil {
			return s.finishRun(ctx, subagent.RunnerResult{Run: result, ToolCalls: s.guard.count()}, err)
		}
		if result == nil || result.Stop != agent.StopPaused {
			// Closing the run and taking its last pending set happen under one
			// critical section, so a concurrent admission either lands in this
			// set and gets its turn, or sees the ended run and is refused. A
			// message is therefore never accepted into a run that will not
			// continue.
			if pending := s.endRunAndDrain(); len(pending) > 0 {
				s.beginRun()
				messages = pending

				continue
			}
			value, text := s.output()

			return s.finishRun(ctx, subagent.RunnerResult{
				Run: result, Value: value, Text: text, ToolCalls: s.guard.count(),
			}, nil)
		}
		if err := s.reconcile(ctx); err != nil {
			return s.finishRun(ctx, subagent.RunnerResult{Run: result, ToolCalls: s.guard.count()}, err)
		}
		if !s.paused() {
			continue
		}
		if err := s.waitForResume(ctx); err != nil {
			return s.finishRun(ctx, subagent.RunnerResult{Run: result, ToolCalls: s.guard.count()}, err)
		}
	}
}

// AdmitMessage implements subagent.MessageSink. It is the single entry point
// for a message into this child. The message is queued here and injected at the
// child's next turn boundary, or handed to the live run immediately.
func (s *childControlScope) AdmitMessage(
	ctx context.Context,
	delivery subagent.MessageDelivery,
	messageID string,
	text string,
	onRelease func(),
) error {
	if s == nil {
		return subagent.ErrMessageClosed
	}
	release := releaseOnce(onRelease)
	if err := ctx.Err(); err != nil {
		release()

		return err
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		release()

		return subagent.ErrMessageClosed
	case s.runEnded:
		s.mu.Unlock()
		release()

		return subagent.ErrMessageFinalizing
	case len(s.inbound) >= subagent.MaxChildMessageAdmissions:
		s.mu.Unlock()
		release()

		return subagent.ErrMessageSaturated
	}
	s.inbound = append(s.inbound, inboundMessage{
		id: messageID, delivery: delivery, text: text, release: release,
	})
	live := s.runActive
	s.mu.Unlock()

	if live {
		s.forwardPending()
	}

	return nil
}

// beginRun records that this child's prompt run is in flight.
func (s *childControlScope) beginRun() {
	s.mu.Lock()
	s.runActive = true
	s.runEnded = false
	s.mu.Unlock()
}

// endRunAndDrain closes this child's run and takes the messages no live
// hand-off consumed. Closing and draining under one critical section is what
// keeps an admission from being accepted into a run that has already stopped.
func (s *childControlScope) endRunAndDrain() []ai.Message {
	s.mu.Lock()
	s.runActive = false
	s.runEnded = true
	pending := s.inbound
	s.inbound = nil
	s.mu.Unlock()

	return inboundMessages(pending)
}

// markRunEnded records that this child's run has finished, so the scope
// accepts no further message.
func (s *childControlScope) markRunEnded() {
	s.mu.Lock()
	s.runActive = false
	s.runEnded = true
	s.mu.Unlock()
}

// forwardPending hands every pending message to the live run, oldest first. A
// message the run cannot take yet stays pending for the next turn boundary.
func (s *childControlScope) forwardPending() {
	for {
		s.mu.Lock()
		if len(s.inbound) == 0 {
			s.mu.Unlock()

			return
		}
		item := s.inbound[0]
		s.inbound = s.inbound[1:]
		s.mu.Unlock()

		if err := s.injectMessage(item); err != nil {
			s.requeueMessage(item)

			return
		}
		item.release()
	}
}

// injectMessage hands one message to the running harness under its delivery
// verb. The harness reports ErrIdle when no run is active.
func (s *childControlScope) injectMessage(item inboundMessage) error {
	message := ai.UserText(item.text)
	switch item.delivery {
	case subagent.MessageDeliveryQueue:
		return s.harness.FollowUp(message)
	case subagent.MessageDeliveryInterject:
		return s.harness.Interject(message)
	default:
		return s.harness.Steer(message)
	}
}

// requeueMessage returns one message to the head of the pending set.
func (s *childControlScope) requeueMessage(item inboundMessage) {
	s.mu.Lock()
	s.inbound = append([]inboundMessage{item}, s.inbound...)
	s.mu.Unlock()
}

// drainPending removes and returns the pending messages as user turns, oldest
// first, releasing each admission as it is consumed.
func (s *childControlScope) drainPending() []ai.Message {
	s.mu.Lock()
	pending := s.inbound
	s.inbound = nil
	s.mu.Unlock()

	return inboundMessages(pending)
}

// inboundMessages renders admitted messages as user turns, releasing each
// admission as it is consumed.
func inboundMessages(pending []inboundMessage) []ai.Message {
	if len(pending) == 0 {
		return nil
	}
	messages := make([]ai.Message, 0, len(pending))
	for _, item := range pending {
		messages = append(messages, ai.UserText(item.text))
		item.release()
	}

	return messages
}

// releaseOnce bounds one admitted message's release to a single call.
func releaseOnce(release func()) func() {
	if release == nil {
		return func() {}
	}
	var once sync.Once

	return func() { once.Do(release) }
}

const (
	personaReminderPrefix = "<system-reminder>\n"
	personaReminderSuffix = "\n</system-reminder>"
)

func personaReminderText(instructions string) string {
	return personaReminderPrefix + instructions + personaReminderSuffix
}

// initialMessages builds the child's first prompt. A persona contributes one
// synthetic user item inserted before the task prompt at the inherited-prefix
// boundary. It is persisted with the run, it is not re-inserted when the
// transcript already carries it, and it is never injected into a resumed
// child, whose forked transcript already carries whatever its source had.
func (s *childControlScope) initialMessages(task string) ([]ai.Message, error) {
	messages := make([]ai.Message, 0, 2)
	reminder := s.plan.PersonaReminder
	if reminder != "" && s.plan.ResumeFrom == "" {
		present, err := s.transcriptCarriesReminder(reminder)
		if err != nil {
			return nil, err
		}
		if !present {
			messages = append(messages, ai.UserText(personaReminderText(reminder)))
		}
	}

	return append(messages, ai.UserText(task)), nil
}

func (s *childControlScope) transcriptCarriesReminder(reminder string) (bool, error) {
	conversation, err := s.child.Context()
	if err != nil {
		return false, err
	}

	return messagesCarryReminder(conversation.Messages, reminder), nil
}

// messagesCarryReminder reports whether a persisted conversation already holds
// this exact persona reminder. It is what keeps a resumed child from
// re-inserting the reminder it already carries.
func messagesCarryReminder(messages []ai.Message, reminder string) bool {
	expected := personaReminderText(reminder)
	for _, message := range messages {
		if childMessageText(message) == expected {
			return true
		}
	}

	return false
}

func (s *childControlScope) finishRun(
	ctx context.Context,
	result subagent.RunnerResult,
	runErr error,
) (subagent.RunnerResult, error) {
	s.markRunEnded()
	result.Cwd = s.directoryOverride
	result.WorktreePath = s.worktreePath
	result.StopCause = s.guard.stopCause()

	return result, errors.Join(runErr, s.finishChanges(context.WithoutCancel(ctx)))
}

// finishChanges publishes and persists only the child-owned report. The
// parent timeline remains a lifecycle-only projection; its reducer never sees
// this EventWorkspaceChanged payload.
func (s *childControlScope) finishChanges(ctx context.Context) error {
	if s == nil || s.changes == nil {
		return nil
	}
	s.changesOnce.Do(func() {
		report, hasReport := s.changes.finish(ctx)
		if !hasReport {
			return
		}
		projected := workspaceChanged(report)
		if err := appendChildWorkspaceChangeAudit(s.child, projected); err != nil {
			s.changesErr = err

			return
		}
		if s.factory.onWorkspaceChanged != nil {
			// Parent-facing projection is deliberately best-effort. The audit was
			// already committed to the child session above; a closing parent UI
			// must not retroactively turn a successful child task into failure.
			_ = s.factory.onWorkspaceChanged(ctx, s.childSessionID, projected)
		}
	})

	return s.changesErr
}

func (s *childControlScope) reconcile(ctx context.Context) error {
	pending, err := s.child.Pending()
	if err != nil {
		return err
	}
	request, err := s.questions.Reconcile(pending)
	if err != nil {
		return err
	}
	if request != nil {
		return s.setQuestionPause(ctx, *request)
	}

	state, err := s.controller.Reconcile(ctx, nil)
	if err != nil {
		return err
	}
	ready, err := s.applyApprovalState(ctx, state)
	if err != nil {
		return err
	}
	if ready {
		return s.clearPause(ctx)
	}

	// A review or unknown state is the intended pause boundary. It is resolved
	// by this child scope's targetable control route; retrying the unchanged
	// journal state here would only spin until the user supplies input.
	return nil
}

func (s *childControlScope) applyApprovalState(ctx context.Context, state approval.State) (bool, error) {
	s.mu.Lock()
	s.approval = state
	s.mu.Unlock()
	switch state.Kind {
	case approval.StateReady:
		return true, nil
	case approval.StateUnknown:
		if err := s.setApprovalPause(ctx, state); err != nil {
			return false, err
		}

		return false, nil
	case approval.StateReview:
		if state.Review == nil {
			return false, approval.ErrJournalCorrupt
		}
		outcome, err := s.hooks.permissionRequest(ctx, *state.Review)
		if err != nil {
			return false, err
		}
		if outcome.Blocked || outcome.Allowed {
			resolution := approval.Resolution{RequestID: state.Review.RequestID, Choice: approval.ChoiceAllowOnce}
			if outcome.Blocked {
				resolution.Choice = approval.ChoiceDeny
				resolution.Reason = outcome.Reason
			}
			next, resolveErr := s.controller.Resolve(ctx, resolution, nil)
			if resolveErr != nil {
				return false, resolveErr
			}

			return s.applyApprovalState(ctx, next)
		}
		if err := s.setApprovalPause(ctx, state); err != nil {
			return false, err
		}

		return false, nil
	default:
		return false, approval.ErrJournalCorrupt
	}
}

func (s *childControlScope) ResolveApproval(
	ctx context.Context,
	resolution approval.Resolution,
) (ChildControlState, error) {
	if s == nil {
		return ChildControlState{}, ErrRuntimeClosed
	}
	state, err := s.controller.Resolve(ctx, resolution, nil)
	if err != nil {
		return ChildControlState{}, err
	}
	ready, err := s.applyApprovalState(ctx, state)
	if err != nil {
		return ChildControlState{}, err
	}
	if ready {
		if err := s.reconcile(ctx); err != nil {
			return ChildControlState{}, err
		}
		if !s.paused() {
			s.signalResume()
		}
	}

	return s.State(), nil
}

func (s *childControlScope) ResolveQuestion(
	ctx context.Context,
	resolution question.Resolution,
) (ChildControlState, error) {
	if s == nil {
		return ChildControlState{}, ErrRuntimeClosed
	}
	if err := s.questions.Resolve(resolution); err != nil {
		return ChildControlState{}, err
	}
	if err := s.reconcile(ctx); err != nil {
		return ChildControlState{}, err
	}
	if !s.paused() {
		s.signalResume()
	}

	return s.State(), nil
}

func (s *childControlScope) RejectQuestion(
	ctx context.Context,
	requestID string,
	schemaDigest string,
) (ChildControlState, error) {
	if s == nil {
		return ChildControlState{}, ErrRuntimeClosed
	}
	if err := s.questions.Reject(requestID, schemaDigest); err != nil {
		return ChildControlState{}, err
	}
	if err := s.reconcile(ctx); err != nil {
		return ChildControlState{}, err
	}
	if !s.paused() {
		s.signalResume()
	}

	return s.State(), nil
}

func (s *childControlScope) State() ChildControlState {
	if s == nil {
		return ChildControlState{}
	}

	s.mu.Lock()
	state := ChildControlState{
		ChildSessionID: s.childSessionID,
		Pause:          s.pause,
		Approval:       s.approval,
	}
	if s.question != nil {
		value := question.CloneRequest(*s.question)
		state.Question = &value
	}
	s.mu.Unlock()

	return state.Clone()
}

func (s *childControlScope) setApprovalPause(ctx context.Context, state approval.State) error {
	s.mu.Lock()
	wasPaused := s.pause != ChildPauseNone
	s.pause = ChildPauseApproval
	s.approval = state
	s.question = nil
	s.mu.Unlock()

	if err := s.notifyPause(ctx, wasPaused, true); err != nil {
		return err
	}
	if s.factory.failOnInput {
		return state.NonInteractiveError()
	}

	return nil
}

func (s *childControlScope) setQuestionPause(ctx context.Context, value question.Request) error {
	cloned := question.CloneRequest(value)
	s.mu.Lock()
	wasPaused := s.pause != ChildPauseNone
	s.pause = ChildPauseQuestion
	s.question = &cloned
	s.approval = approval.State{Kind: approval.StateReady}
	s.mu.Unlock()

	if err := s.notifyPause(ctx, wasPaused, true); err != nil {
		return err
	}
	if s.factory.failOnInput {
		return ErrInputRequired
	}

	return nil
}

func (s *childControlScope) clearPause(ctx context.Context) error {
	s.mu.Lock()
	wasPaused := s.pause != ChildPauseNone
	s.pause = ChildPauseNone
	s.question = nil
	s.approval = approval.State{Kind: approval.StateReady}
	s.mu.Unlock()

	return s.notifyPause(ctx, wasPaused, false)
}

// notifyPause publishes only an edge. Reconciliation may observe the same
// pending state again, but must not append a second child phase transition.
func (s *childControlScope) notifyPause(ctx context.Context, wasPaused, paused bool) error {
	if s.factory.onPauseChanged == nil || wasPaused == paused {
		return nil
	}

	return s.factory.onPauseChanged(ctx, s.childSessionID, paused)
}

func (s *childControlScope) paused() bool {
	s.mu.Lock()
	paused := s.pause != ChildPauseNone
	s.mu.Unlock()

	return paused
}

func (s *childControlScope) waitForResume(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.lifecycleDone:
		return context.Canceled
	case <-s.resume:
		return nil
	}
}

func (s *childControlScope) signalResume() {
	select {
	case s.resume <- struct{}{}:
	default:
	}
}

func (s *childControlScope) validateOutput(
	_ context.Context,
	info agent.OutputGuardrailInfo,
) error {
	if s == nil || s.validator == nil {
		return subagent.ErrInvalidResult
	}
	text := childMessageText(info.Message)
	value, err := s.validator.Validate(text)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.outputValue = value
	s.outputText = text
	s.mu.Unlock()

	return nil
}

func (s *childControlScope) output() (any, string) {
	s.mu.Lock()
	value, text := s.outputValue, s.outputText
	s.mu.Unlock()

	return value, text
}

func (s *childControlScope) requestPolicy(request *ai.Request) {
	if s.factory.requestPolicy != nil {
		s.factory.requestPolicy(request)
	}
	if request.MaxTokens == nil || *request.MaxTokens > s.plan.Limits.MaxOutputTokens {
		request.MaxTokens = ai.Ptr(s.plan.Limits.MaxOutputTokens)
	}
	request.ResponseFormat = nil
	if s.plan.Output.Format == subagent.OutputFormatJSONSchema &&
		s.factory.model.Capabilities().StructuredOutput {
		request.ResponseFormat = &ai.ResponseFormat{
			Name:   s.plan.Output.Name,
			Schema: &ai.Schema{RawJSON: slices.Clone(s.plan.Output.Schema)},
			Strict: true,
		}
	}
	s.applyReasoningEffort(request)
	if s.factory.fullCompaction != nil {
		fixed, err := compaction.RequestOverheadTokens(*request)
		s.mu.Lock()
		s.fixedTokens, s.fixedTokensErr = fixed, err
		s.mu.Unlock()
	}
}

// applyReasoningEffort applies the resolved role or persona reasoning level.
// It is applied only for a model that exposes reasoning controls, so a level
// declared for a non-reasoning model cannot reach the wire.
func (s *childControlScope) applyReasoningEffort(request *ai.Request) {
	if s.plan.ReasoningEffort == "" {
		return
	}
	capabilities := s.factory.model.Capabilities()
	if !capabilities.Reasoning {
		return
	}
	if request.Reasoning == nil {
		request.Reasoning = &ai.ReasoningConfig{}
	}
	request.Reasoning.Effort = ai.ReasoningEffort(s.plan.ReasoningEffort)
}

// prepareCompaction runs after ToolSearch and the durable TurnCompleted save
// point. It never restores or replaces Tools, mutates limits, or consumes a
// parent archive binding. A failed rewrite terminates this run before model I/O.
func (s *childControlScope) prepareCompaction(ctx context.Context, info agent.RunInfo) agent.TurnUpdate {
	if s.factory.fullCompaction == nil || info.Response == nil || len(info.Response.ToolCalls()) == 0 {
		return agent.TurnUpdate{}
	}
	if err := ctx.Err(); err != nil {
		return agent.TurnUpdate{Err: err}
	}
	s.mu.Lock()
	fixed, fixedErr, closed := s.fixedTokens, s.fixedTokensErr, s.closed
	s.mu.Unlock()
	if closed {
		return agent.TurnUpdate{Err: subagent.ErrClosed}
	}
	if fixedErr != nil {
		return agent.TurnUpdate{Err: fixedErr}
	}
	plan, err := compaction.Capture(s.child, &fixed, nil)
	if err != nil {
		return agent.TurnUpdate{Err: fmt.Errorf("coding child: capture compaction: %w", err)}
	}
	policy := *s.factory.fullCompaction
	threshold, err := policy.Threshold()
	if err != nil {
		return agent.TurnUpdate{Err: err}
	}
	if plan.TokensBefore < threshold {
		return agent.TurnUpdate{}
	}
	if !s.historyAllowed {
		return agent.TurnUpdate{Err: fmt.Errorf("%w: child compaction requires profile permission for session_history", compaction.ErrBudget)}
	}
	state, err := json.Marshal(struct {
		SessionID    string                         `json:"child_session_id"`
		AgentID      string                         `json:"agent_id"`
		Instructions string                         `json:"current_assignment_instructions"`
		Capabilities []subagent.EffectiveCapability `json:"current_capabilities"`
		Limits       subagent.Limits                `json:"current_limits"`
	}{s.childSessionID, s.plan.Identity.ID, s.plan.Instructions, s.plan.Capabilities, s.plan.Limits})
	if err != nil {
		return agent.TurnUpdate{Err: err}
	}
	// Summary requests use the resolved model's policy, not the child's
	// final-response JSON schema or its ordinary output-token clamp.
	_, err = compaction.Execute(ctx, s.factory.repository, s.childHandle, s.factory.model,
		s.factory.requestPolicy, plan, policy, "", string(state))
	if err != nil {
		return agent.TurnUpdate{Err: fmt.Errorf("coding child: compact context: %w", err)}
	}
	rebuilt, err := s.child.Context()
	if err != nil {
		return agent.TurnUpdate{Err: err}
	}
	return agent.TurnUpdate{ReplaceMessages: rebuilt.Messages}
}

func (s *childControlScope) afterPendingTool(
	ctx context.Context,
	call agent.ToolCall,
	parts []ai.Part,
	isError bool,
) ([]ai.Part, bool) {
	if s == nil {
		return parts, isError
	}
	override := s.hooks.afterTool(ctx, agent.ToolResultInfo{
		ToolCall: call,
		Result: ai.ToolResultPart{
			ToolCallID: call.ID, Name: call.Name, Content: slices.Clone(parts), IsError: isError,
		},
	})
	if override == nil {
		return parts, isError
	}
	if override.Content != nil {
		parts = slices.Clone(override.Content)
	}
	if override.IsError != nil {
		isError = *override.IsError
	}

	return parts, isError
}

func (s *childControlScope) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		undelivered := s.inbound
		s.inbound = nil
		s.mu.Unlock()
		for _, item := range undelivered {
			// A message this child never consumed still frees its tree-wide
			// admission.
			item.release()
		}
		s.cancel()
		s.signalResume()
		s.pending.clear()
		s.resolver.set(nil)
		s.factory.controls.remove(s.childSessionID, s)
		s.closeErr = errors.Join(
			s.closeErr,
			s.generation.release(ctx),
			s.directoryResources.Close(),
		)
	})

	s.mu.Lock()
	err := s.closeErr
	s.mu.Unlock()

	return err
}

type childToolGuard struct {
	mu         sync.Mutex
	toolCalls  int
	exhausted  bool
	noProgress bool
	last       [sha256.Size]byte
	repeats    int
	// problematic reports that every call in the current identical run is
	// read risk, the reference's ToolKind::Read | ToolKind::Plan tier. A mixed
	// run falls back to the looser thresholds.
	problematic bool
	// risks maps one Tool's wire name to its catalog risk, so the guard can
	// classify a run without re-reading the child catalog.
	risks map[string]catalog.Risk
}

func (g *childToolGuard) before(limits subagent.Limits) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		g.mu.Lock()
		defer g.mu.Unlock()
		if limits.MaxToolCalls > 0 && g.toolCalls >= limits.MaxToolCalls {
			g.exhausted = true

			return agent.DenyTool("subagent tool-call budget exhausted")
		}
		g.toolCalls++
		fingerprint := childToolFingerprint(info.ToolCall)
		if fingerprint == g.last {
			g.repeats++
			g.problematic = g.problematic && g.readRisk(info.Name)
		} else {
			g.last = fingerprint
			g.repeats = 1
			g.problematic = g.readRisk(info.Name)
		}
		nudge, stop := g.thresholds(limits)
		if g.repeats >= stop {
			g.noProgress = true
		}
		if g.repeats >= nudge {
			return agent.DenyTool("identical tool call repeated; use existing evidence or choose a different action")
		}

		return agent.ToolDecision{}
	}
}

// thresholds returns the nudge and hard-stop counts for the current run's tier.
func (g *childToolGuard) thresholds(limits subagent.Limits) (int, int) {
	if g.problematic {
		return limits.RepeatedProblematicToolCallLimit, limits.RepeatedProblematicToolCallStopLimit
	}

	return limits.RepeatedToolCallLimit, limits.RepeatedToolCallStopLimit
}

func (g *childToolGuard) readRisk(name string) bool {
	risk, known := g.risks[name]

	return known && risk == catalog.RiskRead
}

func (g *childToolGuard) stopWhen(_ agent.RunInfo) bool {
	g.mu.Lock()
	stop := g.exhausted || g.noProgress
	g.mu.Unlock()

	return stop
}

// stopCause names a stop the repeat guard raised, so the Manager can classify
// it as no_progress rather than an unexpected stop.
func (g *childToolGuard) stopCause() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.noProgress {
		return subagent.StopCauseNoProgress
	}

	return ""
}

func (g *childToolGuard) count() int {
	g.mu.Lock()
	count := g.toolCalls
	g.mu.Unlock()

	return count
}

// childToolRisks maps each child Tool's wire name to its catalog risk, so the
// repeat guard can tell a read/plan run from a mixed one.
func childToolRisks(descriptors []catalog.Descriptor) map[string]catalog.Risk {
	risks := make(map[string]catalog.Risk, len(descriptors))
	for _, descriptor := range descriptors {
		risks[descriptor.Name] = descriptor.Risk
	}

	return risks
}

func childToolFingerprint(call agent.ToolCall) [sha256.Size]byte {
	arguments := call.Args
	var decoded any
	if json.Unmarshal(call.Args, &decoded) == nil {
		if canonical, err := json.Marshal(decoded); err == nil {
			arguments = canonical
		}
	}
	buffer := make([]byte, 0, len(call.Name)+1+len(arguments))
	buffer = append(buffer, call.Name...)
	buffer = append(buffer, 0)
	buffer = append(buffer, arguments...)

	return sha256.Sum256(buffer)
}

func childMessageText(message ai.Message) string {
	var value strings.Builder
	parts, err := ai.MessageParts(message)
	if err != nil {
		return ""
	}

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			value.WriteString(text.Text)
		}
	}

	return value.String()
}
