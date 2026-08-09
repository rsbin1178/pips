//nolint:containedctx,wsl_v5,gocyclo,funlen // Manager owns a Runtime lifecycle context and structured children.
package subagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	readToolName   = "read"
	listToolName   = "ls"
	globToolName   = "glob"
	searchToolName = "grep"
)

// ExecutionOptions provides embedder/test overrides without adding a user
// configuration protocol in P1. A zero Limits value selects DefaultLimits.
type ExecutionOptions struct {
	Limits                       Limits
	MaxDepth                     int
	MaxConcurrent                int
	MaxSpawnedPerRootInteraction int
	MaxAutoFollowUps             int
}

type sharedManagerState struct {
	mu sync.Mutex

	admission *admissionBudget
	active    map[string]*Execution
	maxDepth  int
}

// Config contains the application-owned dependencies for one parent Session.
type Config struct {
	Context    context.Context
	Repository *session.Repository
	Parent     *session.Handle
	Tree       *workspace.Tree
	Model      ai.LanguageModel
	// GenerationID identifies the immutable Coding integration snapshot that
	// compiled this manager's builtin execution plan. Custom dispatch will
	// acquire a per-child generation lease in the next slice.
	GenerationID   uint64
	SummaryModel   ai.LanguageModel
	Compaction     *harness.CompactionSettings
	RequestPolicy  func(*ai.Request)
	Lifecycle      Lifecycle
	Options        ExecutionOptions
	AgentObservers []func(context.Context, agent.Event)
	EventObservers []AgentEventObserver
	// AdmissionObserver receives content-free shared-budget decisions. It is
	// observational only and cannot veto or alter a reservation.
	AdmissionObserver AdmissionObserver
	// Share joins this Manager to an existing root execution tree. It is
	// required when Parent is a subagent Session.
	Share *Manager
	// DelegationDepth is the depth expected for children created by this
	// manager. Root conversation Managers use zero.
	DelegationDepth int
}

// Manager owns a bounded set of live child executions and all of their cleanup.
// Each admitted child runs until natural completion, cancellation, an explicit
// positive execution limit, or a structural safety policy stops it.
type Manager struct {
	mu        sync.Mutex
	journalMu sync.Mutex

	config           Config
	limits           Limits
	tools            []agent.Tool
	lifecycle        context.Context
	cancel           context.CancelFunc
	active           map[string]*Execution
	starting         int
	startingDone     chan struct{}
	admission        *admissionBudget
	shared           *sharedManagerState
	delegationDepth  int
	maxAutoFollowUps int
	runs             sync.WaitGroup
	waitOnce         sync.Once
	waitDone         chan struct{}
	cleanupErr       error
	closed           bool
}

// Execution is one cancelable, waitable child lifetime. It intentionally does
// not retain a context.Context.
type Execution struct {
	manager     *Manager
	request     Request
	plan        ExecutionPlan
	runner      Runner
	cancel      context.CancelFunc
	done        chan struct{}
	child       *session.Handle
	tracker     *runTracker
	hookContext string
	permit      *admissionPermit

	cancelOnce sync.Once
	mu         sync.Mutex
	result     Result
	err        error
	cleanupErr error
	stopParent func() bool
}

// New constructs a current-parent child manager. Call Reconcile before New so
// interrupted child journals are repaired before the manager accepts work.
//
//nolint:gocyclo // Construction validates all concurrency and execution bounds in one place.
func New(config Config) (*Manager, error) {
	config = inheritManagerConfig(config)
	if err := validateManagerConfig(config); err != nil {
		return nil, err
	}

	limits := config.Options.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}

	limits = normalizeLimits(limits)

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	readTools, err := buildReadTools(config)
	if err != nil {
		return nil, err
	}

	config.AgentObservers = slices.Clone(config.AgentObservers)
	config.EventObservers = slices.Clone(config.EventObservers)
	if config.Compaction != nil {
		settings := *config.Compaction
		config.Compaction = &settings
	}

	maxConcurrent := config.Options.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	maxSpawned := config.Options.MaxSpawnedPerRootInteraction
	if maxSpawned == 0 {
		maxSpawned = defaultMaxSpawned
	}
	maxAutoFollowUps := config.Options.MaxAutoFollowUps
	if maxAutoFollowUps == 0 {
		maxAutoFollowUps = 4
	}
	if maxAutoFollowUps < 1 || maxAutoFollowUps > 32 {
		return nil, fmt.Errorf("%w: invalid manager concurrency limits", ErrInvalid)
	}
	if config.Options.MaxDepth < 0 || config.Options.MaxDepth > MaxDelegationDepth {
		return nil, fmt.Errorf("%w: invalid manager delegation depth", ErrInvalid)
	}
	var shared *sharedManagerState
	if config.Share != nil {
		shared = config.Share.shared
		if shared == nil {
			return nil, fmt.Errorf("%w: unavailable shared manager state", ErrInvalid)
		}
		if shared.maxDepth != config.Options.MaxDepth {
			return nil, fmt.Errorf("%w: shared manager delegation depth mismatch", ErrInvalid)
		}
	} else {
		admission, err := newAdmissionBudget(maxConcurrent, maxSpawned)
		if err != nil {
			return nil, err
		}
		shared = &sharedManagerState{
			admission: admission,
			active:    make(map[string]*Execution),
			maxDepth:  config.Options.MaxDepth,
		}
	}
	lifecycleBase := config.Context
	if lifecycleBase == nil {
		lifecycleBase = context.Background()
	}
	lifecycle, cancel := context.WithCancel(context.WithoutCancel(lifecycleBase))

	return &Manager{
		config: config, limits: limits, tools: readTools,
		lifecycle: lifecycle, cancel: cancel,
		active: make(map[string]*Execution), admission: shared.admission, shared: shared,
		delegationDepth:  config.DelegationDepth,
		maxAutoFollowUps: maxAutoFollowUps,
		waitDone:         make(chan struct{}),
	}, nil
}

func inheritManagerConfig(config Config) Config {
	if config.Share == nil {
		return config
	}
	root := config.Share.config
	config.Repository = root.Repository
	config.Tree = root.Tree
	config.GenerationID = root.GenerationID
	config.SummaryModel = root.SummaryModel
	config.Compaction = root.Compaction
	config.Lifecycle = root.Lifecycle
	config.Options = root.Options
	config.AgentObservers = root.AgentObservers
	config.EventObservers = root.EventObservers
	config.AdmissionObserver = root.AdmissionObserver

	return config
}

// MaxAutoFollowUps returns the validated Runtime coordination bound.
func (m *Manager) MaxAutoFollowUps() int {
	if m == nil {
		return 0
	}

	return m.maxAutoFollowUps
}

func validateManagerConfig(config Config) error {
	if config.Repository == nil || config.Parent == nil || config.Parent.Session() == nil ||
		config.Tree == nil || config.Model == nil {
		return fmt.Errorf("%w: incomplete manager dependencies", ErrInvalid)
	}

	if !validModelIdentity(string(config.Model.Provider())) ||
		!validModelIdentity(config.Model.ModelID()) {
		return fmt.Errorf("%w: invalid model identity", ErrInvalid)
	}

	parentKind := config.Parent.Metadata().Kind
	if parentKind != session.KindConversation && parentKind != session.KindSubagent {
		return fmt.Errorf("%w: parent must be a conversation or subagent", ErrInvalid)
	}
	if parentKind == session.KindSubagent && config.Share == nil {
		return fmt.Errorf("%w: subagent parent requires shared tree state", ErrInvalid)
	}
	if parentKind == session.KindConversation && config.Share != nil {
		return fmt.Errorf("%w: conversation parent cannot join another tree", ErrInvalid)
	}
	if parentKind == session.KindConversation && config.DelegationDepth != 0 {
		return fmt.Errorf("%w: manager delegation depth does not match parent kind", ErrInvalid)
	}
	if parentKind == session.KindSubagent && config.DelegationDepth == 0 {
		return fmt.Errorf("%w: manager delegation depth does not match parent kind", ErrInvalid)
	}
	if config.DelegationDepth < 0 || config.DelegationDepth > MaxDelegationDepth {
		return fmt.Errorf("%w: invalid manager delegation depth", ErrInvalid)
	}

	if err := validateManagerObservers(config); err != nil {
		return err
	}
	if err := validateCompactionDependencies(config); err != nil {
		return err
	}

	return nil
}

func validateManagerObservers(config Config) error {
	for _, observer := range config.AgentObservers {
		if observer == nil {
			return fmt.Errorf("%w: nil agent observer", ErrInvalid)
		}
	}
	for _, observer := range config.EventObservers {
		if observer == nil {
			return fmt.Errorf("%w: nil child event observer", ErrInvalid)
		}
	}

	return nil
}

func validateCompactionDependencies(config Config) error {
	if config.Compaction == nil {
		if config.SummaryModel != nil {
			return fmt.Errorf("%w: summary model requires child compaction", ErrInvalid)
		}

		return nil
	}
	if config.SummaryModel == nil ||
		!validModelIdentity(string(config.SummaryModel.Provider())) ||
		!validModelIdentity(config.SummaryModel.ModelID()) {
		return fmt.Errorf("%w: child compaction requires a valid summary model", ErrInvalid)
	}

	settings := *config.Compaction
	usable := settings.ContextTokens - settings.ReserveTokens
	if settings.ContextTokens <= 0 || settings.ReserveTokens <= 0 || usable <= 0 ||
		settings.KeepRecentTokens <= 0 || settings.KeepRecentTokens > usable ||
		settings.SummaryTokens <= 0 {
		return fmt.Errorf("%w: invalid child compaction settings", ErrInvalid)
	}

	return nil
}

func validModelIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n")
}

func buildReadTools(config Config) ([]agent.Tool, error) {
	local, err := tools.NewCatalog(config.Tree, tools.DefaultLimits())
	if err != nil {
		return nil, err
	}

	names := []string{readToolName, listToolName, globToolName, searchToolName}
	policy := catalog.Policy{
		TenantID:  config.Parent.Metadata().WorkspaceID,
		Allowlist: slices.Clone(names),
		MaxRisk:   catalog.RiskRead,
	}

	readTools, err := local.Tools(context.TODO(), policy, names...)
	if err != nil {
		return nil, err
	}

	for index, name := range names {
		if readTools[index].Decl().Name != name {
			return nil, fmt.Errorf("%w: read-only catalog mismatch", ErrInvalid)
		}
	}

	return readTools, nil
}

func (m *Manager) builtinExecutionPlan(request Request) (ExecutionPlan, error) {
	identity, err := BuiltinIdentity(request.Role)
	if err != nil {
		return ExecutionPlan{}, err
	}
	spec, err := specFor(request.Role)
	if err != nil {
		return ExecutionPlan{}, err
	}
	nativeStructuredOutput := m.config.Model.Capabilities().StructuredOutput
	instructions, err := spec.instructionsFor(nativeStructuredOutput)
	if err != nil {
		return ExecutionPlan{}, err
	}
	schema, err := json.Marshal(spec.schema)
	if err != nil {
		return ExecutionPlan{}, fmt.Errorf("coding subagent: encode builtin output schema: %w", err)
	}
	output := OutputContract{
		Format: OutputFormatBuiltin,
		Name:   spec.name,
		Schema: schema,
	}
	output.Digest = digestOutputContract(output)

	capabilities := make([]EffectiveCapability, 0, len(m.tools))
	for _, tool := range m.tools {
		if tool == nil {
			return ExecutionPlan{}, fmt.Errorf("%w: nil builtin tool", ErrInvalid)
		}
		capabilities = append(capabilities, EffectiveCapability{
			WireName: tool.Decl().Name,
			Source:   "builtin:workspace",
			Risk:     readToolName,
		})
	}
	plan := ExecutionPlan{
		Schema:             ExecutionPlanSchema,
		Identity:           identity,
		GenerationID:       m.config.GenerationID,
		Delivery:           request.Delivery,
		Model:              modelName(m.config.Model),
		Instructions:       instructions,
		InstructionsDigest: digestText(instructions),
		Capabilities:       capabilities,
		Skills:             []string{},
		Limits:             m.limits,
		Output:             output,
	}
	if err := validateExecutionPlan(plan, false); err != nil {
		return ExecutionPlan{}, err
	}

	return plan, nil
}

// Start reserves one execution slot and starts one owned goroutine.
func (m *Manager) Start(
	ctx context.Context,
	request Request,
	observer Observer,
) (*Execution, error) {
	return m.start(ctx, request, observer, nil)
}

// StartWithDispatcher admits either a builtin compatibility request or a
// Runtime-compiled custom execution. Dispatcher is interaction-scoped: it
// owns registry visibility and the child-specific control binding, while this
// Manager retains quotas, durable lineage, lifecycle, and terminal cleanup.
func (m *Manager) StartWithDispatcher(
	ctx context.Context,
	request Request,
	observer Observer,
	dispatcher Dispatcher,
) (*Execution, error) {
	return m.start(ctx, request, observer, dispatcher)
}

func (m *Manager) start(
	ctx context.Context,
	request Request,
	observer Observer,
	dispatcher Dispatcher,
) (*Execution, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil manager", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var err error
	request, builtin, err := normalizeDispatchRequest(request)
	if err != nil {
		return nil, err
	}
	if err := validateDispatchRequest(request, m.limits); err != nil {
		return nil, err
	}
	request = m.normalizeRequest(ctx, request)
	var plan ExecutionPlan
	if builtin {
		if m.config.Parent.Metadata().Kind == session.KindSubagent {
			return nil, fmt.Errorf("%w: recursive builtin agents are unavailable", ErrInvalid)
		}
		if err := validateRequest(request, m.limits); err != nil {
			return nil, err
		}
		plan, err = m.builtinExecutionPlan(request)
	} else {
		if dispatcher == nil {
			return nil, fmt.Errorf("%w: custom agent dispatcher is unavailable", ErrInvalid)
		}
		plan, err = dispatcher.Compile(ctx, request)
		if err == nil {
			err = validateDispatchedPlan(request, plan, m.limits)
		}
		if err == nil {
			err = m.validateDelegationPlan(plan)
		}
	}
	if err != nil {
		return nil, err
	}
	permit, err := m.reserveStart(ctx, request)
	if err != nil {
		return nil, err
	}
	ownershipTransferred := false
	defer func() { m.finishStarting(permit, ownershipTransferred) }()

	startCtx, cancelStart := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(m.lifecycle, cancelStart)
	defer func() {
		stopLifecycle()
		cancelStart()
	}()

	parentMeta := m.config.Parent.Metadata()
	planDigest, err := plan.Digest()
	if err != nil {
		return nil, err
	}
	childIdentity := session.SubagentIdentity{
		Schema:           plan.Identity.Schema,
		AgentID:          plan.Identity.ID,
		Kind:             string(plan.Identity.Kind),
		Name:             plan.Identity.Name,
		DefinitionSchema: plan.Identity.DefinitionSchema,
		DefinitionDigest: plan.Identity.DefinitionDigest,
		DefinitionSource: plan.Identity.DefinitionSource,
		GenerationID:     plan.GenerationID,
		PlanDigest:       planDigest,
	}

	child, err := m.config.Repository.Create(startCtx, session.CreateOptions{
		WorkspaceID:      parentMeta.WorkspaceID,
		WorkspacePath:    parentMeta.WorkspacePath,
		Kind:             session.KindSubagent,
		ParentSessionID:  parentMeta.ID,
		ParentRunID:      request.Ownership.ParentRunID,
		Agent:            request.AgentID,
		SubagentIdentity: &childIdentity,
	})
	if err != nil {
		return nil, err
	}

	created := record{
		Schema:              recordSchema,
		State:               StateCreated,
		Identity:            plan.Identity,
		Role:                request.Role,
		Plan:                plan,
		ChildSessionID:      child.Metadata().ID,
		ParentSessionID:     parentMeta.ID,
		ParentInteractionID: request.Ownership.ParentInteractionID,
		ParentRunID:         request.Ownership.ParentRunID,
		ParentToolCallID:    request.Ownership.ParentToolCallID,
		RootInteractionID:   request.Ownership.RootInteractionID,
		Delivery:            request.Delivery,
		Model:               plan.Model,
		Limits:              journalLimits(plan.Limits),
		TaskPreview:         preview(request.Task),
		Time:                time.Now().UTC(),
	}
	var (
		runCtx context.Context
		cancel context.CancelFunc
	)
	if plan.Limits.MaxDuration > 0 {
		runCtx, cancel = context.WithTimeout(m.lifecycle, plan.Limits.MaxDuration)
	} else {
		runCtx, cancel = context.WithCancel(m.lifecycle)
	}
	var stopParent func() bool
	if request.Delivery == DeliveryForeground {
		stopParent = context.AfterFunc(ctx, cancel)
	}
	tracker := newRunTracker(created.Time, plan.Limits.MaxActivityTools)
	execution := &Execution{
		manager:    m,
		request:    request,
		plan:       plan,
		cancel:     cancel,
		done:       make(chan struct{}),
		child:      child,
		tracker:    tracker,
		stopParent: stopParent,
		permit:     permit,
	}
	if err := m.appendMirrored(child.Session(), created); err != nil {
		cancel()
		if stopParent != nil {
			stopParent()
		}

		return nil, errors.Join(err, child.Close())
	}
	if !builtin {
		runner, openErr := dispatcher.Open(runCtx, DispatchInput{
			Plan: plan, Request: request, Child: child,
			Observer: observer,
			OnEvent: func(eventCtx context.Context, event agent.Event) {
				m.observeChild(eventCtx, child, observer, request, created, tracker, event)
			},
		})
		if openErr != nil {
			cancel()
			if stopParent != nil {
				stopParent()
			}
			terminal := created
			terminal.State = StateFailed
			terminal.Code = "binding_failed"
			terminal.Time = time.Now().UTC()
			persistErr := m.appendMirrored(child.Session(), terminal)
			// A custom binding can fail after the durable child record exists
			// (for example a required capability disappears). Project both
			// lifecycle edges so parent Runs views do not retain a phantom
			// created child. The durable journal remains authoritative if an
			// observer is already detached.
			createdObserverErr := emitObserver(runCtx, observer, eventFromRecord(created))
			terminalObserverErr := emitObserver(runCtx, observer, eventFromRecord(terminal))

			return nil, errors.Join(
				openErr,
				persistErr,
				createdObserverErr,
				terminalObserverErr,
				child.Close(),
			)
		}
		execution.runner = runner
	}
	if m.config.Lifecycle.BeforeStart != nil {
		execution.hookContext = m.config.Lifecycle.BeforeStart(runCtx, LifecycleStart{
			ChildSessionID: child.Metadata().ID,
			Identity:       plan.Identity,
			Role:           request.Role,
			Task:           request.Task,
			Ownership:      request.Ownership,
		})
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		if stopParent != nil {
			stopParent()
		}

		terminal := created
		terminal.State = StateCanceled
		terminal.Code = "manager_closed"
		terminal.Time = time.Now().UTC()
		persistErr := m.appendMirrored(child.Session(), terminal)
		runnerCloseErr := error(nil)
		if execution.runner != nil {
			runnerCloseErr = execution.runner.Close(context.WithoutCancel(startCtx))
		}
		closeErr := child.Close()

		return nil, errors.Join(ErrClosed, persistErr, runnerCloseErr, closeErr)
	}

	if err := m.registerExecution(created.ChildSessionID, execution); err != nil {
		m.mu.Unlock()
		cancel()
		if stopParent != nil {
			stopParent()
		}

		return nil, errors.Join(err, closeRunner(context.WithoutCancel(startCtx), execution.runner), child.Close())
	}
	m.runs.Add(1)
	permit.commit()
	ownershipTransferred = true
	m.mu.Unlock()

	observerErr := emitObserver(runCtx, observer, eventFromRecord(created))
	if observerErr != nil {
		execution.Cancel()
	}

	go func() {
		defer m.runs.Done()
		m.run(runCtx, execution, child, observer, created, observerErr)
	}()

	return execution, nil
}

func closeRunner(ctx context.Context, runner Runner) error {
	if runner == nil {
		return nil
	}

	return runner.Close(ctx)
}

func (m *Manager) reserveStart(
	ctx context.Context,
	request Request,
) (*admissionPermit, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.observeAdmission(ctx, request, ErrClosed)

		return nil, ErrClosed
	}
	permit, err := m.admission.reserve(
		request.Delivery,
		request.Ownership.RootInteractionID,
		m.config.Parent.Metadata().Kind == session.KindSubagent,
	)
	if err != nil {
		m.mu.Unlock()
		m.observeAdmission(ctx, request, err)

		return nil, err
	}

	if m.starting == 0 {
		m.startingDone = make(chan struct{})
	}
	m.starting++
	m.mu.Unlock()
	m.observeAdmission(ctx, request, nil)

	return permit, nil
}

func (m *Manager) observeAdmission(ctx context.Context, request Request, admissionErr error) {
	if m == nil || m.config.AdmissionObserver == nil {
		return
	}

	event := AdmissionEvent{
		Outcome: AdmissionOutcomeAccepted, Delivery: request.Delivery,
		DelegationDepth: m.delegationDepth,
	}
	if admissionErr != nil {
		event.Outcome = AdmissionOutcomeRejected
		event.Reason = admissionReason(admissionErr)
	}

	func() {
		defer func() { _ = recover() }()
		m.config.AdmissionObserver(ctx, event)
	}()
}

func admissionReason(err error) AdmissionReason {
	switch {
	case errors.Is(err, ErrBusy):
		return AdmissionReasonBusy
	case errors.Is(err, ErrCapacity):
		return AdmissionReasonCapacity
	case errors.Is(err, ErrSpawnLimit):
		return AdmissionReasonSpawnLimit
	case errors.Is(err, ErrClosed):
		return AdmissionReasonClosed
	default:
		return AdmissionReasonInvalid
	}
}

func (m *Manager) validateDelegationPlan(plan ExecutionPlan) error {
	if len(plan.Ancestry) == 0 {
		if m.config.Parent.Metadata().Kind == session.KindSubagent {
			return fmt.Errorf("%w: nested dispatch requires delegation lineage", ErrInvalid)
		}

		return nil
	}
	if m.shared == nil || plan.DelegationDepth != m.delegationDepth ||
		plan.MaxDelegationDepth != m.shared.maxDepth {
		return fmt.Errorf("%w: execution plan delegation budget mismatch", ErrInvalid)
	}

	return nil
}

func (m *Manager) registerExecution(childSessionID string, execution *Execution) error {
	if m.shared == nil {
		return fmt.Errorf("%w: unavailable shared manager state", ErrInvalid)
	}
	m.shared.mu.Lock()
	defer m.shared.mu.Unlock()
	if _, exists := m.shared.active[childSessionID]; exists {
		return fmt.Errorf("%w: duplicate child execution", ErrInvalid)
	}
	m.active[childSessionID] = execution
	m.shared.active[childSessionID] = execution

	return nil
}

func (m *Manager) normalizeRequest(ctx context.Context, request Request) Request {
	request.Ownership.ParentSessionID = m.config.Parent.Metadata().ID
	if metadata, ok := agent.RunMetadataFromContext(ctx); ok {
		request.Ownership.ParentRunID = metadata.RunID
	}
	if request.Ownership.RootInteractionID == "" {
		request.Ownership.RootInteractionID = request.Ownership.ParentInteractionID
	}
	if request.Delivery == "" {
		request.Delivery = DeliveryForeground
	}

	return request
}

func (m *Manager) finishStarting(permit *admissionPermit, ownershipTransferred bool) {
	if !ownershipTransferred {
		permit.release()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.starting > 0 {
		m.starting--
	}
	if m.starting == 0 && m.startingDone != nil {
		close(m.startingDone)
		m.startingDone = nil
	}
}

// Wait waits for terminal persistence or for only this wait context to end.
func (e *Execution) Wait(ctx context.Context) (Result, error) {
	if e == nil {
		return Result{}, fmt.Errorf("%w: nil execution", ErrInvalid)
	}

	select {
	case <-e.done:
		e.mu.Lock()
		result, err := cloneResult(e.result), e.err
		e.mu.Unlock()

		return result, err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Cancel requests child cancellation. It is safe to call repeatedly.
func (e *Execution) Cancel() {
	if e == nil {
		return
	}

	e.cancelOnce.Do(e.cancel)
}

// Wait waits for one owned child to become terminal. A child that already
// completed is reconstructed from its durable journal.
func (m *Manager) Wait(ctx context.Context, childSessionID string) (Result, error) {
	if m == nil {
		return Result{}, ErrClosed
	}
	if err := session.ValidateID(childSessionID); err != nil {
		return Result{}, fmt.Errorf("%w: invalid child session id", ErrInvalid)
	}
	if active := m.activeExecution(childSessionID); active != nil {
		return active.Wait(ctx)
	}

	detail, err := m.Inspect(ctx, childSessionID)
	if err != nil {
		return Result{}, err
	}
	result := resultFromDetail(detail)
	if result.Outcome == OutcomeSucceeded {
		return result, nil
	}

	return result, fmt.Errorf("coding subagent: %s (%s)", result.Outcome, result.Code)
}

// Cancel cancels one running child. Repeating cancellation, including after
// the child is terminal, is safe.
func (m *Manager) Cancel(ctx context.Context, childSessionID string) error {
	if m == nil {
		return ErrClosed
	}
	if err := session.ValidateID(childSessionID); err != nil {
		return fmt.Errorf("%w: invalid child session id", ErrInvalid)
	}
	if active := m.activeExecution(childSessionID); active != nil {
		active.Cancel()

		return nil
	}

	_, err := m.findSummary(ctx, childSessionID)

	return err
}

// CancelRoot cancels every running background child created by one root
// interaction and returns the number of cancellation requests issued.
func (m *Manager) CancelRoot(rootInteractionID string) int {
	if m == nil || rootInteractionID == "" {
		return 0
	}

	m.mu.Lock()
	values := make([]*Execution, 0, len(m.active))
	for _, execution := range m.active {
		if execution.request.Delivery == DeliveryBackground &&
			execution.request.Ownership.RootInteractionID == rootInteractionID {
			values = append(values, execution)
		}
	}
	m.mu.Unlock()

	for _, execution := range values {
		execution.Cancel()
	}

	return len(values)
}

func resultFromDetail(detail Detail) Result {
	value := detail.Summary
	result := Result{
		Identity: value.Identity, Role: value.Role, ChildSessionID: value.ChildSessionID,
		Code: value.Code, Turns: value.Turns, ToolCalls: value.ToolCalls,
		Usage: value.Usage, Duration: value.Duration, Value: detail.Result,
	}
	switch value.State {
	case StateSucceeded:
		result.Outcome = OutcomeSucceeded
	case StateCanceled:
		result.Outcome = OutcomeCanceled
	case StateInterrupted:
		result.Outcome = OutcomeInterrupted
	case StateCreated, StateRunning, StateFailed:
		result.Outcome = OutcomeFailed
	}

	return result
}

// Close stops admission, cancels every child, and waits for terminal
// persistence before returning.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	m.closed = true
	m.cancel()
	startingDone := m.startingDone
	m.mu.Unlock()
	if startingDone != nil {
		select {
		case <-startingDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	m.waitOnce.Do(func() {
		go func() {
			m.runs.Wait()
			close(m.waitDone)
		}()
	})
	select {
	case <-m.waitDone:
		m.mu.Lock()
		err := m.cleanupErr
		m.mu.Unlock()

		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type runTracker struct {
	mu sync.Mutex

	started              bool
	runID                string
	turns                int
	toolCalls            int
	stopCause            runStopCause
	finalizationCause    finalizationCause
	finalizationInjected bool
	stopAfterTurn        int
	lastToolFingerprint  [sha256.Size]byte
	repeatedToolCalls    int
	repeatWarningIssued  bool
	usage                ai.Usage
	value                any
	text                 string
	err                  error
	activity             Activity
	activitySummary      ActivitySummary
	toolIndex            map[string]int
	maxTools             int
}

type runStopCause uint8

const (
	runStopNone runStopCause = iota
	runStopMaxToolCalls
	runStopNoProgress
)

type finalizationCause uint8

const (
	finalizationNone finalizationCause = iota
	finalizationTurnBudget
	finalizationNoProgress
)

type progressSnapshot struct {
	runID     string
	turns     int
	toolCalls int
	usage     ai.Usage
	activity  ActivitySummary
}

func newRunTracker(startedAt time.Time, maxTools int) *runTracker {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	return &runTracker{
		activity: Activity{
			Revision: 1, Phase: ActivityPhaseStarting,
			StartedAt: startedAt, UpdatedAt: startedAt,
			Tools: []ToolActivity{},
		},
		toolIndex: make(map[string]int),
		maxTools:  maxTools,
	}
}

func (m *Manager) run(
	ctx context.Context,
	execution *Execution,
	child *session.Handle,
	observer Observer,
	created record,
	observerErr error,
) {
	tracker := execution.tracker
	tracker.mu.Lock()
	tracker.err = observerErr
	startedAt := tracker.activity.StartedAt
	tracker.mu.Unlock()
	if execution.runner != nil {
		output, runErr := execution.runner.Run(ctx, execution.request.Task)
		tracker.mu.Lock()
		tracker.value = output.Value
		tracker.text = output.Text
		tracker.toolCalls = output.ToolCalls
		tracker.mu.Unlock()
		m.finishExecution(
			ctx,
			execution,
			child,
			observer,
			created,
			tracker,
			startedAt,
			output.Run,
			runErr,
		)

		return
	}

	spec, specErr := specFor(execution.request.Role)
	if specErr != nil {
		m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, nil, specErr)
		return
	}

	useNativeResponseFormat := m.config.Model.Capabilities().StructuredOutput
	instructions := execution.plan.Instructions

	onEvent := func(eventCtx context.Context, event agent.Event) {
		m.observeChild(
			eventCtx,
			child,
			observer,
			execution.request,
			created,
			tracker,
			event,
		)
	}
	options := m.agentOptions(execution.plan.Identity, child, tracker, spec, useNativeResponseFormat)

	childHarness, err := harness.New(
		m.config.Model,
		child.Session(),
		harness.WithSystem(instructions),
		harness.WithSystemSuffix(execution.hookContext),
		harness.WithTools(m.tools...),
		harness.WithOnEvent(onEvent),
		harness.WithAgentOptions(options...),
	)
	if err == nil {
		var result *agent.RunResult

		result, err = childHarness.Prompt(ctx, execution.request.Task)
		stopHookActive := false
		for err == nil && result != nil && m.config.Lifecycle.BeforeStop != nil {
			decision := m.config.Lifecycle.BeforeStop(ctx, LifecycleStop{
				ChildSessionID:       child.Metadata().ID,
				Identity:             execution.plan.Identity,
				Role:                 execution.request.Role,
				Ownership:            execution.request.Ownership,
				StopHookActive:       stopHookActive,
				LastAssistantMessage: result.Text(),
			})
			reason := strings.TrimSpace(decision.Reason)
			if !decision.Continue || reason == "" {
				break
			}

			stopHookActive = true
			result, err = childHarness.Prompt(ctx, reason)
		}
		m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, result, err)

		return
	}

	m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, nil, err)
}

//nolint:gocyclo,wsl_v5 // Each option keeps its execution boundary beside the policy it enforces.
func (m *Manager) agentOptions(
	identity AgentIdentity,
	child *session.Handle,
	tracker *runTracker,
	spec roleSpec,
	useNativeResponseFormat bool,
) []agent.Option {
	return []agent.Option{
		agent.WithName("subagent/" + identity.ID),
		agent.WithMaxTurns(m.limits.MaxTurns),
		agent.WithMaxTokens(m.limits.MaxTokens),
		agent.WithParallelTools(1),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			if m.limits.MaxToolCalls > 0 && tracker.toolCalls >= m.limits.MaxToolCalls {
				tracker.stopCause = runStopMaxToolCalls
				return agent.DenyTool("subagent tool-call budget exhausted")
			}

			tracker.toolCalls++
			switch tracker.trackToolFingerprintLocked(
				info.ToolCall,
				m.limits.RepeatedToolCallLimit,
			) {
			case repeatedToolContinue:
			case repeatedToolWarn:
				return agent.DenyTool(
					"identical tool call repeated; choose a different action or finalize from existing evidence",
				)
			case repeatedToolFinalize:
				if tracker.finalizationCause == finalizationNone {
					tracker.finalizationCause = finalizationNoProgress
				}

				return agent.DenyTool(
					"identical tool call repeated after a no-progress warning; finalize from existing evidence",
				)
			}

			return agent.ToolDecision{}
		}),
		agent.WithStopWhen(func(info agent.RunInfo) bool {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			if tracker.stopCause == runStopMaxToolCalls {
				return true
			}
			if tracker.finalizationCause == finalizationNoProgress &&
				tracker.finalizationInjected && info.Turns >= tracker.stopAfterTurn {
				tracker.stopCause = runStopNoProgress

				return true
			}

			return false
		}),
		agent.WithPrepareTurn(func(ctx context.Context, info agent.RunInfo) agent.TurnUpdate {
			return m.prepareChildTurn(ctx, info, child, tracker)
		}),
		agent.WithOutputGuardrail("subagent_result", func(
			_ context.Context,
			info agent.OutputGuardrailInfo,
		) error {
			text := messageText(info.Message)

			value, err := spec.decodeResult(text, m.limits, !useNativeResponseFormat)
			if err != nil {
				return err
			}

			tracker.mu.Lock()
			tracker.value = value
			tracker.text = text
			tracker.mu.Unlock()

			return nil
		}),
		agent.WithRequest(func(request *ai.Request) {
			if m.config.RequestPolicy != nil {
				m.config.RequestPolicy(request)
			}

			if request.MaxTokens == nil || *request.MaxTokens > m.limits.MaxOutputTokens {
				request.MaxTokens = ai.Ptr(m.limits.MaxOutputTokens)
			}

			request.ResponseFormat = nil
			if useNativeResponseFormat {
				request.ResponseFormat = &ai.ResponseFormat{
					Name: spec.name, Schema: spec.schema, Strict: true,
				}
			}
		}),
	}
}

func (m *Manager) prepareChildTurn(
	ctx context.Context,
	info agent.RunInfo,
	child *session.Handle,
	tracker *runTracker,
) agent.TurnUpdate {
	finalization, alreadyInjected, skip := m.childPreparationState(info, tracker)
	if skip {
		return agent.TurnUpdate{}
	}

	// The convergence turn is deliberately terminal. Compacting after it would
	// add model I/O without giving the child another productive turn.
	if finalization == finalizationNoProgress && alreadyInjected {
		return agent.TurnUpdate{}
	}

	messages, err := m.compactChildAfterToolTurn(ctx, info, child)
	if err != nil {
		return agent.TurnUpdate{Err: err}
	}

	shouldFinalize, alreadyInjected := m.markChildFinalization(info.Turns, tracker)

	if !shouldFinalize || alreadyInjected {
		if messages == nil {
			return agent.TurnUpdate{}
		}

		return agent.TurnUpdate{ReplaceMessages: messages}
	}

	return childFinalizationUpdate(child, messages)
}

func (m *Manager) childPreparationState(
	info agent.RunInfo,
	tracker *runTracker,
) (finalizationCause, bool, bool) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	stopForTokens := m.limits.MaxTokens > 0 &&
		info.Usage.InputTokens+info.Usage.OutputTokens >= m.limits.MaxTokens
	skip := tracker.stopCause != runStopNone || stopForTokens

	return tracker.finalizationCause, tracker.finalizationInjected, skip
}

func (m *Manager) markChildFinalization(turns int, tracker *runTracker) (bool, bool) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if tracker.finalizationCause == finalizationNone && m.limits.MaxTurns > 0 &&
		m.limits.FinalizationTurns > 0 &&
		turns >= m.limits.MaxTurns-m.limits.FinalizationTurns {
		tracker.finalizationCause = finalizationTurnBudget
	}
	shouldFinalize := tracker.finalizationCause != finalizationNone
	alreadyInjected := tracker.finalizationInjected
	if shouldFinalize && !alreadyInjected {
		tracker.finalizationInjected = true
		if tracker.finalizationCause == finalizationNoProgress {
			tracker.stopAfterTurn = turns + 1
		}
	}

	return shouldFinalize, alreadyInjected
}

func childFinalizationUpdate(
	child *session.Handle,
	messages []ai.Message,
) agent.TurnUpdate {
	if messages == nil {
		contextSnapshot, err := child.Session().Context()
		if err != nil {
			return agent.TurnUpdate{
				Err: fmt.Errorf("coding subagent: load finalization context: %w", err),
			}
		}
		messages = slices.Clone(contextSnapshot.Messages)
	}
	messages = append(messages, ai.UserText(finalizationInstruction))

	return agent.TurnUpdate{
		ReplaceMessages: messages,
		Tools:           []agent.Tool{},
	}
}

func (m *Manager) compactChildAfterToolTurn(
	ctx context.Context,
	info agent.RunInfo,
	child *session.Handle,
) ([]ai.Message, error) {
	if m.config.Compaction == nil || info.Response == nil ||
		len(info.Response.ToolCalls()) == 0 {
		return nil, nil
	}

	settings := *m.config.Compaction
	path := child.Session().Path()
	if !harness.ShouldCompact(harness.EstimateContext(path), settings) ||
		harness.PlanCompaction(path, settings) == nil {
		return nil, nil
	}

	compactor, err := harness.New(
		m.config.Model,
		child.Session(),
		harness.WithCompaction(settings),
		harness.WithSummaryModel(m.config.SummaryModel),
	)
	if err == nil {
		err = compactor.Compact(ctx, "")
	}
	if err != nil {
		return nil, fmt.Errorf("coding subagent: compact child context: %w", err)
	}

	contextSnapshot, err := child.Session().Context()
	if err != nil {
		return nil, fmt.Errorf("coding subagent: reload compacted child context: %w", err)
	}

	return slices.Clone(contextSnapshot.Messages), nil
}

const finalizationInstruction = `[System] The evidence-gathering phase is complete and tools are now disabled.
Return the required final response now using only evidence already collected.
Do not request more tools or describe unfinished work.`

type repeatedToolAction uint8

const (
	repeatedToolContinue repeatedToolAction = iota
	repeatedToolWarn
	repeatedToolFinalize
)

func (t *runTracker) trackToolFingerprintLocked(call agent.ToolCall, limit int) repeatedToolAction {
	fingerprint := toolFingerprint(call)
	if fingerprint == t.lastToolFingerprint {
		t.repeatedToolCalls++
	} else {
		t.lastToolFingerprint = fingerprint
		t.repeatedToolCalls = 1
		t.repeatWarningIssued = false
	}

	if t.repeatedToolCalls < limit {
		return repeatedToolContinue
	}

	if !t.repeatWarningIssued {
		t.repeatWarningIssued = true

		return repeatedToolWarn
	}

	return repeatedToolFinalize
}

func toolFingerprint(call agent.ToolCall) [sha256.Size]byte {
	arguments := call.Args

	var decoded any
	if err := json.Unmarshal(call.Args, &decoded); err == nil {
		if canonical, marshalErr := json.Marshal(decoded); marshalErr == nil {
			arguments = canonical
		}
	}

	input := make([]byte, 0, len(call.Name)+1+len(arguments))
	input = append(input, call.Name...)
	input = append(input, 0)
	input = append(input, arguments...)

	return sha256.Sum256(input)
}

func (m *Manager) observeChild(
	ctx context.Context,
	child *session.Handle,
	observer Observer,
	request Request,
	created record,
	tracker *runTracker,
	event agent.Event,
) {
	snapshot, needsStart, visibleChange := trackChildEvent(tracker, event)

	if needsStart {
		m.recordChildStart(ctx, child, observer, created, tracker, event)
	}

	if visibleChange && event.Type != agent.EventRunStart {
		m.emitChildProgress(ctx, child, observer, created, tracker, event, snapshot)
	}

	for _, rawObserver := range m.config.AgentObservers {
		emitRawObserver(ctx, rawObserver, event)
	}
	for _, childObserver := range m.config.EventObservers {
		if err := emitChildEventObserver(ctx, childObserver, AgentEvent{
			ChildSessionID: child.Metadata().ID,
			Ownership:      executionOwnership(created),
			Task:           request.Task,
			Event:          event,
		}); err != nil {
			m.failChildObservation(child.Metadata().ID, tracker, err)
		}
	}
}

func trackChildEvent(
	tracker *runTracker,
	event agent.Event,
) (progressSnapshot, bool, bool) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if event.Turn > tracker.turns {
		tracker.turns = event.Turn
	}

	if event.Type == agent.EventTurnEnd || event.Type == agent.EventRunEnd {
		tracker.usage = event.Usage
	}

	needsStart := event.Type == agent.EventRunStart && !tracker.started
	if needsStart {
		tracker.started = true
		tracker.runID = event.RunID
	}

	visibleChange := tracker.trackActivityLocked(event)

	return progressSnapshot{
		runID: tracker.runID, turns: tracker.turns,
		toolCalls: tracker.toolCalls, usage: tracker.usage,
		activity: tracker.activitySummary,
	}, needsStart, visibleChange
}

func (t *runTracker) trackActivityLocked(event agent.Event) bool {
	changed := false

	switch event.Type {
	case agent.EventRunStart:
		t.activity.RunID = event.RunID
		changed = true
	case agent.EventTurnStart:
		t.activity.Phase = ActivityPhaseThinking
		changed = true
	case agent.EventMessage:
		changed = t.trackMessageActivityLocked(event)
	case agent.EventToolStart:
		changed = t.trackToolStartLocked(event)
	case agent.EventToolUpdate:
		changed = t.trackToolUpdateLocked(event)
	case agent.EventToolEnd:
		changed = t.trackToolEndLocked(event)
	case agent.EventTurnEnd:
		t.activity.Phase = ActivityPhaseThinking
		changed = true
	case agent.EventRunEnd:
		t.activity.Phase = ActivityPhaseFinalizing
		changed = true
	case agent.EventDelta, agent.EventCandidateDiscard:
	}

	if event.Turn > t.activity.Turn {
		t.activity.Turn = event.Turn
	}

	if !changed {
		return false
	}

	t.activity.Revision++
	t.activity.UpdatedAt = activityEventTime(event.Time)

	return true
}

func (t *runTracker) trackMessageActivityLocked(event agent.Event) bool {
	if event.Message == nil || event.Message.Role != ai.RoleAssistant ||
		messageHasToolCall(*event.Message) {
		return false
	}

	t.activity.Phase = ActivityPhaseFinalizing

	return true
}

func (t *runTracker) trackToolStartLocked(event agent.Event) bool {
	if event.Call == nil {
		return false
	}

	t.upsertToolLocked(event, ToolStatusRunning)
	t.activitySummary = summarizeToolActivity(*event.Call)
	t.activity.Phase = ActivityPhaseWorking

	return true
}

func (t *runTracker) trackToolUpdateLocked(event agent.Event) bool {
	if event.Call == nil {
		return false
	}

	index := t.upsertToolLocked(event, ToolStatusRunning)
	if index >= 0 {
		t.activity.Tools[index].Update = cloneTranscriptMessage(ai.Message{
			Role: ai.RoleTool, Parts: event.Update,
		})
	}
	t.activity.Phase = ActivityPhaseWorking

	return true
}

func (t *runTracker) trackToolEndLocked(event agent.Event) bool {
	if event.Call == nil || event.Result == nil {
		return false
	}

	index := t.upsertToolLocked(event, ToolStatusCompleted)
	if index >= 0 {
		t.activity.Tools[index].Result = cloneTranscriptMessage(ai.Message{
			Role: ai.RoleTool, Parts: []ai.Part{*event.Result},
		})
	}
	t.activity.Phase = ActivityPhaseThinking

	return true
}

func (t *runTracker) upsertToolLocked(event agent.Event, status ToolStatus) int {
	call := *event.Call
	if call.ID == "" || call.Name == "" {
		return -1
	}

	call.Args = slices.Clone(call.Args)
	if index, ok := t.toolIndex[call.ID]; ok {
		t.activity.Tools[index].RunID = event.RunID
		t.activity.Tools[index].Turn = event.Turn
		t.activity.Tools[index].Call = call
		t.activity.Tools[index].Status = status

		return index
	}

	index := len(t.activity.Tools)
	if index >= t.maxTools {
		return -1
	}

	t.toolIndex[call.ID] = index
	t.activity.Tools = append(t.activity.Tools, ToolActivity{
		RunID: event.RunID, Turn: event.Turn, Call: call, Status: status,
	})

	return index
}

func activityEventTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}

	return value
}

func messageHasToolCall(message ai.Message) bool {
	for _, part := range message.Parts {
		if _, ok := part.(ai.ToolCallPart); ok {
			return true
		}
	}

	return false
}

func (t *runTracker) activitySnapshot() Activity {
	t.mu.Lock()
	defer t.mu.Unlock()

	return cloneActivity(t.activity)
}

func (t *runTracker) snapshot() (progressSnapshot, Activity) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return progressSnapshot{
		runID: t.runID, turns: t.turns, toolCalls: t.toolCalls, usage: t.usage,
		activity: t.activitySummary,
	}, cloneActivity(t.activity)
}

//nolint:wsl_v5 // Each known Tool keeps decode, semantic mapping, and scope assembly together.
func summarizeToolActivity(call ai.ToolCallPart) ActivitySummary {
	var arguments struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(call.Args, &arguments); err != nil {
		return ActivitySummary{}
	}

	summary := ActivitySummary{}
	switch call.Name {
	case readToolName:
		summary.Action = ActivityActionRead
		summary.Target = safeActivityPath(arguments.Path)
	case searchToolName:
		summary.Action = ActivityActionSearch
		summary.Target = arguments.Pattern
		if scope := safeActivityPath(arguments.Path); scope != "" {
			summary.Target += " in " + scope
		}
	case globToolName:
		summary.Action = ActivityActionGlob
		summary.Target = arguments.Pattern
		if scope := safeActivityPath(arguments.Path); scope != "" {
			summary.Target += " in " + scope
		}
	case listToolName:
		summary.Action = ActivityActionList
		summary.Target = safeActivityPath(arguments.Path)
		if summary.Target == "" {
			summary.Target = "."
		}
	default:
		return ActivitySummary{}
	}

	summary.Target = boundedActivityTarget(summary.Target)
	if summary.Target == "" {
		return ActivitySummary{}
	}

	return summary
}

//nolint:wsl_v5 // Normalization and rejection form one disclosure boundary.
func safeActivityPath(value string) string {
	value = boundedActivityTarget(value)
	normalized := strings.ReplaceAll(value, "\\", "/")
	cleaned := path.Clean(normalized)
	if strings.HasPrefix(normalized, "/") || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") ||
		len(normalized) >= 2 && normalized[1] == ':' {
		return ""
	}

	return value
}

//nolint:wsl_v5 // Sanitization and both bounds are one disclosure operation.
func boundedActivityTarget(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}

		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	const (
		maximumRunes = 160
		maximumBytes = 512
	)
	if utf8.RuneCountInString(value) <= maximumRunes && len(value) <= maximumBytes {
		return value
	}

	var bounded strings.Builder
	runeCount := 0
	for _, character := range value {
		if runeCount >= maximumRunes-1 ||
			bounded.Len()+utf8.RuneLen(character)+len("…") > maximumBytes {
			break
		}
		bounded.WriteRune(character)
		runeCount++
	}
	bounded.WriteString("…")

	return bounded.String()
}

func cloneActivity(value Activity) Activity {
	value.Tools = slices.Clone(value.Tools)
	for index := range value.Tools {
		value.Tools[index].Call.Args = slices.Clone(value.Tools[index].Call.Args)
		value.Tools[index].Update = cloneTranscriptMessage(value.Tools[index].Update)
		value.Tools[index].Result = cloneTranscriptMessage(value.Tools[index].Result)
	}

	return value
}

func (m *Manager) recordChildStart(
	ctx context.Context,
	child *session.Handle,
	observer Observer,
	created record,
	tracker *runTracker,
	event agent.Event,
) {
	started := created
	started.State = StateRunning
	started.ChildRunID = event.RunID
	started.Time = event.Time

	if err := m.appendMirrored(child.Session(), started); err != nil {
		m.failChildObservation(child.Metadata().ID, tracker, err)
	}

	if err := emitObserver(ctx, observer, eventFromRecord(started)); err != nil {
		m.failChildObservation(child.Metadata().ID, tracker, err)
	}
}

func (m *Manager) emitChildProgress(
	ctx context.Context,
	child *session.Handle,
	observer Observer,
	created record,
	tracker *runTracker,
	event agent.Event,
	snapshot progressSnapshot,
) {
	progress := eventFromRecord(created)
	progress.Progress = true
	progress.State = StateRunning
	progress.ChildRunID = snapshot.runID
	progress.Turns = snapshot.turns
	progress.ToolCalls = snapshot.toolCalls
	progress.Usage = snapshot.usage
	progress.Activity = snapshot.activity
	progress.Time = event.Time

	if err := emitObserver(ctx, observer, progress); err != nil {
		m.failChildObservation(child.Metadata().ID, tracker, err)
	}
}

func (m *Manager) failChildObservation(childSessionID string, tracker *runTracker, err error) {
	tracker.mu.Lock()
	tracker.err = errors.Join(tracker.err, err)
	tracker.mu.Unlock()

	if active := m.activeExecution(childSessionID); active != nil {
		active.Cancel()
	}
}

func (m *Manager) finishExecution(
	ctx context.Context,
	execution *Execution,
	child *session.Handle,
	observer Observer,
	created record,
	tracker *runTracker,
	startedAt time.Time,
	runResult *agent.RunResult,
	runErr error,
) {
	execution.Cancel()
	tracker.mu.Lock()
	trackedErr := tracker.err
	value := tracker.value
	text := tracker.text
	runID := tracker.runID
	turns := tracker.turns
	toolCalls := tracker.toolCalls
	usage := tracker.usage
	stopCause := tracker.stopCause
	convergenceActive := tracker.finalizationCause == finalizationNoProgress &&
		tracker.finalizationInjected
	activity := tracker.activitySummary
	tracker.mu.Unlock()

	if trackedErr != nil {
		runErr = errors.Join(runErr, trackedErr)
	}

	result := Result{
		Identity:       execution.plan.Identity,
		Role:           execution.request.Role,
		ChildSessionID: child.Metadata().ID,
		ChildRunID:     runID,
		Turns:          turns,
		ToolCalls:      toolCalls,
		Usage:          usage,
		Duration:       time.Since(startedAt),
		Value:          value,
	}
	if runResult != nil {
		result.Stop = runResult.Stop
		result.Turns = runResult.Turns
		result.Usage = runResult.Usage
	}

	tokenExceeded := execution.plan.Limits.MaxTokens > 0 &&
		result.Usage.InputTokens+result.Usage.OutputTokens > execution.plan.Limits.MaxTokens

	result.Outcome, result.Code = classifyOutcome(
		runErr,
		runResult,
		value,
		stopCause,
		convergenceActive,
		tokenExceeded,
	)
	if result.Outcome != OutcomeSucceeded {
		result.Value = nil
	}

	terminal := created
	terminal.State = stateForOutcome(result.Outcome)
	terminal.ChildRunID = result.ChildRunID
	terminal.Code = result.Code
	terminal.Stop = string(result.Stop)
	terminal.Turns = result.Turns
	terminal.ToolCalls = result.ToolCalls
	terminal.Usage = result.Usage
	terminal.DurationMillis = result.Duration.Milliseconds()
	terminal.ResultBytes = len(text)
	terminal.Time = time.Now().UTC()
	persistErr := m.appendMirrored(child.Session(), terminal)
	runnerCloseErr := error(nil)
	if execution.runner != nil {
		runnerCloseErr = execution.runner.Close(context.WithoutCancel(ctx))
	}
	closeErr := child.Close()
	execution.permit.release()
	cleanupErr := errors.Join(trackedErr, persistErr, runnerCloseErr, closeErr)

	finalErr := errors.Join(runErr, persistErr, runnerCloseErr, closeErr)
	if result.Outcome == OutcomeSucceeded && finalErr == nil {
		finalErr = nil
	} else if finalErr == nil {
		finalErr = fmt.Errorf("coding subagent: %s (%s)", result.Outcome, result.Code)
	}

	terminalEvent := eventFromRecord(terminal)
	terminalEvent.Activity = activity
	terminalResult := cloneResult(result)
	terminalEvent.Result = &terminalResult
	observerErr := emitObserver(context.WithoutCancel(ctx), observer, terminalEvent)
	finalErr = errors.Join(finalErr, observerErr)
	cleanupErr = errors.Join(cleanupErr, observerErr)

	execution.mu.Lock()
	execution.result = result
	execution.err = finalErr
	execution.cleanupErr = cleanupErr
	execution.mu.Unlock()
	if execution.stopParent != nil {
		execution.stopParent()
	}
	m.mu.Lock()
	if m.active[terminal.ChildSessionID] == execution {
		delete(m.active, terminal.ChildSessionID)
	}
	m.cleanupErr = errors.Join(m.cleanupErr, cleanupErr)
	m.mu.Unlock()
	m.shared.mu.Lock()
	if m.shared.active[terminal.ChildSessionID] == execution {
		delete(m.shared.active, terminal.ChildSessionID)
	}
	m.shared.mu.Unlock()
	close(execution.done)
}

func (m *Manager) activeExecution(childSessionID string) *Execution {
	m.shared.mu.Lock()
	defer m.shared.mu.Unlock()

	active := m.shared.active[childSessionID]
	if active == nil || active.child == nil ||
		active.child.Metadata().ID != childSessionID {
		return nil
	}

	return active
}

func classifyOutcome(
	runErr error,
	runResult *agent.RunResult,
	value any,
	stopCause runStopCause,
	convergenceActive bool,
	tokenExceeded bool,
) (Outcome, string) {
	if outcome, code, canceled := canceledOutcome(runErr); canceled {
		return outcome, code
	}

	if errors.Is(runErr, ErrInvalidResult) || errors.Is(runErr, agent.ErrGuardrail) {
		if convergenceActive {
			return OutcomeFailed, "no_progress"
		}

		return OutcomeFailed, "invalid_result"
	}

	if runErr != nil {
		return OutcomeFailed, "execution_failed"
	}

	if stopCause == runStopMaxToolCalls {
		return OutcomeFailed, "max_tool_calls"
	}
	if stopCause == runStopNoProgress {
		return OutcomeFailed, "no_progress"
	}

	if tokenExceeded {
		return OutcomeFailed, "max_tokens"
	}

	if runResult == nil {
		return OutcomeFailed, "missing_result"
	}

	return outcomeForStop(runResult.Stop, value)
}

func canceledOutcome(err error) (Outcome, string, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeCanceled, "deadline_exceeded", true
	}

	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled, "canceled", true
	}

	return "", "", false
}

func outcomeForStop(stop agent.StopReason, value any) (Outcome, string) {
	switch stop {
	case agent.StopEndTurn:
		if value == nil {
			return OutcomeFailed, "invalid_result"
		}

		return OutcomeSucceeded, "ok"
	case agent.StopMaxTurns:
		return OutcomeFailed, "max_turns"
	case agent.StopBudget:
		return OutcomeFailed, "max_tokens"
	default:
		return OutcomeFailed, "unexpected_stop"
	}
}

func stateForOutcome(outcome Outcome) State {
	switch outcome {
	case OutcomeSucceeded:
		return StateSucceeded
	case OutcomeCanceled:
		return StateCanceled
	case OutcomeInterrupted:
		return StateInterrupted
	default:
		return StateFailed
	}
}

func eventFromRecord(value record) Event {
	return Event{
		State:               value.State,
		Identity:            value.Identity,
		Role:                value.Role,
		ChildSessionID:      value.ChildSessionID,
		ParentSessionID:     value.ParentSessionID,
		ParentInteractionID: value.ParentInteractionID,
		ParentRunID:         value.ParentRunID,
		ParentToolCallID:    value.ParentToolCallID,
		RootInteractionID:   value.RootInteractionID,
		Delivery:            value.Delivery,
		ChildRunID:          value.ChildRunID,
		Model:               value.Model,
		TaskPreview:         value.TaskPreview,
		Code:                value.Code,
		Stop:                agent.StopReason(value.Stop),
		Turns:               value.Turns,
		ToolCalls:           value.ToolCalls,
		Usage:               value.Usage,
		Duration:            time.Duration(value.DurationMillis) * time.Millisecond,
		Time:                value.Time,
	}
}

func emitObserver(ctx context.Context, observer Observer, event Event) (err error) {
	if observer == nil {
		return nil
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("coding subagent: lifecycle observer panicked: %v", recovered)
		}
	}()

	return observer(ctx, event)
}

func emitChildEventObserver(
	ctx context.Context,
	observer AgentEventObserver,
	event AgentEvent,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("coding subagent: child event observer panicked: %v", recovered)
		}
	}()

	return observer(ctx, event)
}

func executionOwnership(value record) Ownership {
	return Ownership{
		ParentSessionID:     value.ParentSessionID,
		ParentInteractionID: value.ParentInteractionID,
		ParentRunID:         value.ParentRunID,
		ParentToolCallID:    value.ParentToolCallID,
		RootInteractionID:   value.RootInteractionID,
	}
}

func emitRawObserver(ctx context.Context, observer func(context.Context, agent.Event), event agent.Event) {
	defer func() { _ = recover() }()

	observer(ctx, event)
}

func messageText(message ai.Message) string {
	var result strings.Builder

	for _, part := range message.Parts {
		if text, ok := part.(ai.TextPart); ok {
			result.WriteString(text.Text)
		}
	}

	return result.String()
}

func modelName(model ai.LanguageModel) string {
	return string(model.Provider()) + "/" + model.ModelID()
}

func cloneResult(result Result) Result {
	if result.Value == nil {
		return result
	}

	data, err := json.Marshal(result.Value)
	if err != nil {
		result.Value = nil
		return result
	}

	value, err := decodeClonedValue(result.Identity, result.Role, data)
	if err != nil {
		result.Value = nil
		return result
	}

	result.Value = value

	return result
}

func decodeClonedValue(identity AgentIdentity, role Role, data []byte) (any, error) {
	if identity.Kind == AgentKindCustom || identity.Kind == AgentKindEphemeral {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, errors.New("trailing custom result JSON")
		}

		return value, nil
	}

	switch role {
	case RoleExplore:
		var value ExploreResult
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}

		return value, nil
	case RolePlan:
		var value PlanResult
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}

		return value, nil
	case RoleReview:
		var value ReviewResult
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}

		return value, nil
	default:
		return nil, fmt.Errorf("%w: invalid result role", ErrInvalid)
	}
}
