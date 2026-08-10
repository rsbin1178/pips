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

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes/git"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
)

// childScopeFactory contains only application-owned, immutable-or-shared-safe
// dependencies needed to build a child control scope. In particular it has no
// parent approval controller, parent pending runner, parent harness, or parent
// interaction pointer.
type childScopeFactory struct {
	workspace            workspace.Workspace
	tree                 *workspace.Tree
	toolLimits           tools.Limits
	policy               execution.Policy
	executor             *execution.Executor
	sandbox              config.SandboxMode
	network              config.SandboxNetworkMode
	requestPolicy        func(*ai.Request)
	toolTimeout          time.Duration
	inspector            *git.Inspector
	hooks                []hooks.Definition
	privateHooks         []hooks.Definition
	hookRunner           hooks.Runner
	onHookDiagnostics    func(context.Context, []hooks.Diagnostic)
	model                ai.LanguageModel
	mode                 OperatingMode
	mcpEntries           []catalog.Entry
	controls             *childControlRegistry
	subagents            *subagent.Manager
	delegationDispatcher subagent.Dispatcher
	delegationOwner      subagent.Ownership
	delegationObserver   subagent.Observer
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
	lifecycleDone  <-chan struct{}
	cancel         context.CancelFunc
	generation     *IntegrationGeneration
	factory        childScopeFactory

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
	nested      *subagent.Manager

	pause       ChildPauseKind
	approval    approval.State
	question    *question.Request
	resume      chan struct{}
	outputValue any
	outputText  string
	closed      bool
	closeOnce   sync.Once
	closeErr    error
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
	if err := subagent.ValidateExecutionPlan(input.Plan); err != nil {
		return nil, err
	}
	if err := generation.acquire(); err != nil {
		return nil, err
	}
	acquired := true
	var nested *subagent.Manager
	defer func() {
		if returnErr != nil && nested != nil {
			returnErr = errors.Join(returnErr, nested.Close(context.WithoutCancel(ctx)))
		}
		if returnErr != nil && acquired {
			returnErr = errors.Join(returnErr, generation.release(context.WithoutCancel(ctx)))
		}
	}()

	lifecycle, cancel := context.WithCancel(context.WithoutCancel(ctx))
	scope := &childControlScope{
		childSessionID: input.Child.Metadata().ID,
		plan:           input.Plan.Clone(),
		child:          input.Child.Session(),
		lifecycleDone:  lifecycle.Done(),
		cancel:         cancel,
		generation:     generation,
		factory:        factory.clone(),
		resume:         make(chan struct{}, 1),
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
	if factory.delegationDispatcher != nil {
		if factory.subagents == nil || len(scope.plan.DelegationTargets) == 0 {
			cancel()
			return nil, fmt.Errorf("%w: incomplete recursive delegation dependencies", subagent.ErrInvalid)
		}
		nested, err = subagent.New(subagent.Config{
			Context: lifecycle, Parent: input.Child, Model: factory.model,
			RequestPolicy: factory.requestPolicy, Share: factory.subagents,
			DelegationDepth: scope.plan.DelegationDepth + 1,
		})
		if err != nil {
			cancel()
			return nil, err
		}
		scope.nested = nested
	}

	controller, err := approval.New(
		factory.workspace,
		scope.child,
		&scope.resolver,
		&scope.pending,
		factory.policy,
		factory.executor,
		tools.NewShellHandlerForSandbox(factory.sandbox, factory.network),
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
	scope.changes = newInteractionChangeTracker(factory.inspector, descriptors)
	policy := catalog.AllowAll(factory.workspace.Identity().Key(), catalog.RiskPrivileged)
	search, err := catalog.NewToolSearch(childCatalog, policy, catalog.ToolSearchOptions{
		Enabled: scope.plan.ToolSearch,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	visibleTools, err := search.Tools(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	allTools, err := childCatalog.Snapshot(ctx, policy)
	if err != nil {
		cancel()
		return nil, err
	}
	allTools = appendUniqueTools(allTools, visibleTools...)

	preloaded, err := activateExplicitSkills(skillCatalog, scope.plan.PreloadedSkills)
	if err != nil {
		cancel()
		return nil, err
	}
	stateful := newStatefulToolBatchGuard(descriptors)
	composed := extension.ComposeHooks(
		extension.Hooks{BeforeTool: scope.guard.before(scope.plan.Limits)},
		extension.Hooks{BeforeTool: scope.hooks.beforeTool},
		extension.Hooks{BeforeTool: leasedToolGuard(scope.factory.mode, descriptors, scope.plan.ToolSearch)},
		extension.Hooks{BeforeTool: stateful.beforeTool},
		extension.Hooks{BeforeTool: scope.questions.BeforeTool},
		extension.Hooks{BeforeTool: scope.changes.beforeTool},
		extension.Hooks{BeforeTool: scope.controller.BeforeTool},
		extension.Hooks{PrepareTurn: search.PrepareTurn},
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
		agent.WithParallelTools(1),
		agent.WithStopWhen(scope.guard.stopWhen),
		agent.WithToolTimeout(factory.toolTimeout),
		agent.WithRequest(scope.requestPolicy),
		agent.WithOutputGuardrail("subagent_result", scope.validateOutput),
	)
	h, err := harness.New(
		factory.model,
		scope.child,
		harness.WithSystem(scope.plan.Instructions),
		harness.WithSystemSuffix(preloaded),
		harness.WithTools(visibleTools...),
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
	local, err := tools.NewCatalog(s.factory.tree, s.factory.toolLimits, tools.WithControlledShell(shell))
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
	for _, entry := range s.factory.mcpEntries {
		descriptor := catalog.Descriptor{
			Name: entry.Tool.Decl().Name, Source: entry.Source, Risk: entry.Risk, Tags: slices.Clone(entry.Tags),
		}
		if planSelectsDescriptor(s.plan, descriptor) {
			entries = append(entries, catalog.Entry{
				Tool: entry.Tool, Source: entry.Source, Risk: entry.Risk, Tags: slices.Clone(entry.Tags),
			})
		}
	}
	if s.nested != nil {
		delegationTool := s.nested.ToolForTargets(
			s.factory.delegationOwner,
			s.factory.delegationObserver,
			s.factory.delegationDispatcher,
			s.plan.DelegationTargets,
		)
		entries = append(entries, catalog.Local("coding.subagent", catalog.RiskRead, delegationTool)...)
	}
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

	return childCatalog, skillCatalog, descriptors, nil
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
	messages := []ai.Message{ai.UserText(task)}
	for {
		result, err := s.harness.PromptMessages(ctx, messages...)
		messages = nil
		if err != nil {
			return s.finishRun(ctx, subagent.RunnerResult{Run: result, ToolCalls: s.guard.count()}, err)
		}
		if result == nil || result.Stop != agent.StopPaused {
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

func (s *childControlScope) finishRun(
	ctx context.Context,
	result subagent.RunnerResult,
	runErr error,
) (subagent.RunnerResult, error) {
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
		s.mu.Unlock()
		s.cancel()
		s.signalResume()
		s.pending.clear()
		s.resolver.set(nil)
		s.factory.controls.remove(s.childSessionID, s)
		if s.nested != nil {
			s.closeErr = errors.Join(s.closeErr, s.nested.Close(ctx))
		}
		s.closeErr = errors.Join(s.closeErr, s.generation.release(ctx))
	})

	s.mu.Lock()
	err := s.closeErr
	s.mu.Unlock()

	return err
}

type childToolGuard struct {
	mu        sync.Mutex
	toolCalls int
	exhausted bool
	last      [sha256.Size]byte
	repeats   int
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
		} else {
			g.last = fingerprint
			g.repeats = 1
		}
		if g.repeats >= limits.RepeatedToolCallLimit {
			return agent.DenyTool("identical tool call repeated; use existing evidence or choose a different action")
		}

		return agent.ToolDecision{}
	}
}

func (g *childToolGuard) stopWhen(_ agent.RunInfo) bool {
	g.mu.Lock()
	stop := g.exhausted
	g.mu.Unlock()

	return stop
}

func (g *childToolGuard) count() int {
	g.mu.Lock()
	count := g.toolCalls
	g.mu.Unlock()

	return count
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
	for _, part := range message.Parts {
		if text, ok := part.(ai.TextPart); ok {
			value.WriteString(text.Text)
		}
	}

	return value.String()
}
