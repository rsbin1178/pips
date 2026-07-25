//nolint:containedctx,wsl_v5 // Manager owns a Runtime lifecycle context and structured children.
package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	MaxConcurrent                int
	MaxSpawnedPerRootInteraction int
	MaxAutoFollowUps             int
}

// Config contains the application-owned dependencies for one parent Session.
type Config struct {
	Context        context.Context
	Repository     *session.Repository
	Parent         *session.Handle
	Tree           *workspace.Tree
	Model          ai.LanguageModel
	RequestPolicy  func(*ai.Request)
	Options        ExecutionOptions
	AgentObservers []func(context.Context, agent.Event)
	EventObservers []AgentEventObserver
}

// Manager owns a bounded set of live child executions and all of their cleanup.
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
	spawned          map[string]int
	maxConcurrent    int
	maxSpawned       int
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
	manager *Manager
	request Request
	cancel  context.CancelFunc
	done    chan struct{}
	child   *session.Handle
	tracker *runTracker

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

	maxConcurrent := config.Options.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = 4
	}
	maxSpawned := config.Options.MaxSpawnedPerRootInteraction
	if maxSpawned == 0 {
		maxSpawned = 8
	}
	maxAutoFollowUps := config.Options.MaxAutoFollowUps
	if maxAutoFollowUps == 0 {
		maxAutoFollowUps = 4
	}
	if maxConcurrent < 1 || maxConcurrent > 32 || maxSpawned < 1 || maxSpawned > 128 ||
		maxAutoFollowUps < 1 || maxAutoFollowUps > 32 {
		return nil, fmt.Errorf("%w: invalid manager concurrency limits", ErrInvalid)
	}
	lifecycleBase := config.Context
	if lifecycleBase == nil {
		lifecycleBase = context.Background()
	}
	lifecycle, cancel := context.WithCancel(context.WithoutCancel(lifecycleBase))

	return &Manager{
		config: config, limits: limits, tools: readTools,
		lifecycle: lifecycle, cancel: cancel,
		active: make(map[string]*Execution), spawned: make(map[string]int),
		maxConcurrent: maxConcurrent, maxSpawned: maxSpawned,
		maxAutoFollowUps: maxAutoFollowUps,
		waitDone:         make(chan struct{}),
	}, nil
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

	if config.Parent.Metadata().Kind != session.KindConversation {
		return fmt.Errorf("%w: parent must be a conversation", ErrInvalid)
	}

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

// Start reserves one bounded execution slot and starts one owned goroutine.
func (m *Manager) Start(
	ctx context.Context,
	request Request,
	observer Observer,
) (*Execution, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil manager", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := validateRequest(request, m.limits); err != nil {
		return nil, err
	}
	request = m.normalizeRequest(ctx, request)
	if err := m.reserveStart(request); err != nil {
		return nil, err
	}
	startSucceeded := false
	defer func() { m.finishStarting(request, startSucceeded) }()

	startCtx, cancelStart := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(m.lifecycle, cancelStart)
	defer func() {
		stopLifecycle()
		cancelStart()
	}()

	parentMeta := m.config.Parent.Metadata()

	child, err := m.config.Repository.Create(startCtx, session.CreateOptions{
		WorkspaceID:     parentMeta.WorkspaceID,
		Kind:            session.KindSubagent,
		ParentSessionID: parentMeta.ID,
		ParentRunID:     request.Ownership.ParentRunID,
		Agent:           string(request.Role),
	})
	if err != nil {
		return nil, err
	}

	created := record{
		Schema:              recordSchema,
		State:               StateCreated,
		Role:                request.Role,
		ChildSessionID:      child.Metadata().ID,
		ParentSessionID:     parentMeta.ID,
		ParentInteractionID: request.Ownership.ParentInteractionID,
		ParentRunID:         request.Ownership.ParentRunID,
		ParentToolCallID:    request.Ownership.ParentToolCallID,
		RootInteractionID:   request.Ownership.RootInteractionID,
		Delivery:            request.Delivery,
		Model:               modelName(m.config.Model),
		Limits:              journalLimits(m.limits),
		TaskPreview:         preview(request.Task),
		Time:                time.Now().UTC(),
	}
	if err := m.appendMirrored(child.Session(), created); err != nil {
		return nil, errors.Join(err, child.Close())
	}
	startSucceeded = true

	runCtx, cancel := context.WithTimeout(m.lifecycle, m.limits.MaxDuration)
	var stopParent func() bool
	if request.Delivery == DeliveryForeground {
		stopParent = context.AfterFunc(ctx, cancel)
	}
	tracker := newRunTracker(created.Time, m.limits.MaxToolCalls)
	execution := &Execution{
		manager:    m,
		request:    request,
		cancel:     cancel,
		done:       make(chan struct{}),
		child:      child,
		tracker:    tracker,
		stopParent: stopParent,
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
		closeErr := child.Close()

		return nil, errors.Join(ErrClosed, persistErr, closeErr)
	}

	m.active[created.ChildSessionID] = execution
	m.runs.Add(1)
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

func (m *Manager) reserveStart(request Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if len(m.active)+m.starting >= m.maxConcurrent {
		if m.maxConcurrent == 1 {
			return ErrBusy
		}

		return ErrCapacity
	}
	if request.Delivery == DeliveryBackground {
		root := request.Ownership.RootInteractionID
		if root == "" || m.spawned[root] >= m.maxSpawned {
			return ErrSpawnLimit
		}
		m.spawned[root]++
	}

	if m.starting == 0 {
		m.startingDone = make(chan struct{})
	}
	m.starting++

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

func (m *Manager) finishStarting(request Request, succeeded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !succeeded && request.Delivery == DeliveryBackground {
		root := request.Ownership.RootInteractionID
		if m.spawned[root] > 1 {
			m.spawned[root]--
		} else {
			delete(m.spawned, root)
		}
	}
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
		Role: value.Role, ChildSessionID: value.ChildSessionID,
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
	exhausted            bool
	finalizing           bool
	finalizationInjected bool
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

	spec, specErr := specFor(execution.request.Role)
	if specErr != nil {
		m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, nil, specErr)
		return
	}

	useNativeResponseFormat := m.config.Model.Capabilities().StructuredOutput

	instructions, instructionsErr := spec.instructionsFor(useNativeResponseFormat)
	if instructionsErr != nil {
		m.finishExecution(
			ctx, execution, child, observer, created, tracker, startedAt, nil, instructionsErr,
		)

		return
	}

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
	options := m.agentOptions(execution.request.Role, child, tracker, spec, useNativeResponseFormat)

	childHarness, err := harness.New(
		m.config.Model,
		child.Session(),
		harness.WithSystem(instructions),
		harness.WithTools(m.tools...),
		harness.WithOnEvent(onEvent),
		harness.WithAgentOptions(options...),
	)
	if err == nil {
		var result *agent.RunResult

		result, err = childHarness.Prompt(ctx, execution.request.Task)
		m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, result, err)

		return
	}

	m.finishExecution(ctx, execution, child, observer, created, tracker, startedAt, nil, err)
}

//nolint:gocyclo,wsl_v5 // Each option keeps its execution boundary beside the policy it enforces.
func (m *Manager) agentOptions(
	role Role,
	child *session.Handle,
	tracker *runTracker,
	spec roleSpec,
	useNativeResponseFormat bool,
) []agent.Option {
	return []agent.Option{
		agent.WithName("subagent/" + string(role)),
		agent.WithMaxTurns(m.limits.MaxTurns),
		agent.WithMaxTokens(m.limits.MaxTokens),
		agent.WithParallelTools(1),
		agent.WithBeforeTool(func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			if tracker.toolCalls >= m.limits.MaxToolCalls {
				tracker.exhausted = true
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
				tracker.finalizing = true

				return agent.DenyTool(
					"identical tool call repeated after a no-progress warning; finalize from existing evidence",
				)
			}

			return agent.ToolDecision{}
		}),
		agent.WithStopWhen(func(agent.RunInfo) bool {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			return tracker.exhausted
		}),
		agent.WithPrepareTurn(func(_ context.Context, info agent.RunInfo) agent.TurnUpdate {
			tracker.mu.Lock()
			workingTurns := m.limits.MaxTurns - m.limits.FinalizationTurns
			shouldFinalize := tracker.finalizing || info.Turns >= workingTurns
			alreadyFinalizing := tracker.finalizing && tracker.finalizationInjected
			if shouldFinalize {
				tracker.finalizing = true
			}
			tracker.mu.Unlock()

			if !shouldFinalize || alreadyFinalizing {
				return agent.TurnUpdate{}
			}

			contextSnapshot, err := child.Session().Context()
			if err != nil {
				tracker.mu.Lock()
				tracker.err = errors.Join(tracker.err, err)
				tracker.mu.Unlock()

				return agent.TurnUpdate{}
			}

			messages := append(
				slices.Clone(contextSnapshot.Messages),
				ai.UserText(finalizationInstruction),
			)

			tracker.mu.Lock()
			tracker.finalizationInjected = true
			tracker.mu.Unlock()

			return agent.TurnUpdate{
				ReplaceMessages: messages,
				Tools:           []agent.Tool{},
			}
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
	case agent.EventDelta:
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
	if event.Call == nil || t.upsertToolLocked(event, ToolStatusRunning) < 0 {
		return false
	}

	t.activitySummary = summarizeToolActivity(*event.Call)
	t.activity.Phase = ActivityPhaseWorking

	return true
}

func (t *runTracker) trackToolUpdateLocked(event agent.Event) bool {
	if event.Call == nil {
		return false
	}

	index := t.upsertToolLocked(event, ToolStatusRunning)
	if index < 0 {
		return false
	}

	t.activity.Tools[index].Update = cloneTranscriptMessage(ai.Message{
		Role: ai.RoleTool, Parts: event.Update,
	})
	t.activity.Phase = ActivityPhaseWorking

	return true
}

func (t *runTracker) trackToolEndLocked(event agent.Event) bool {
	if event.Call == nil || event.Result == nil {
		return false
	}

	index := t.upsertToolLocked(event, ToolStatusCompleted)
	if index < 0 {
		return false
	}

	t.activity.Tools[index].Result = cloneTranscriptMessage(ai.Message{
		Role: ai.RoleTool, Parts: []ai.Part{*event.Result},
	})
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
	exhausted := tracker.exhausted
	activity := tracker.activitySummary
	tracker.mu.Unlock()

	if trackedErr != nil {
		runErr = errors.Join(runErr, trackedErr)
	}

	result := Result{
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

	tokenExceeded := result.Usage.InputTokens+result.Usage.OutputTokens > m.limits.MaxTokens

	result.Outcome, result.Code = classifyOutcome(
		runErr,
		runResult,
		value,
		exhausted,
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
	closeErr := child.Close()
	cleanupErr := errors.Join(trackedErr, persistErr, closeErr)

	finalErr := errors.Join(runErr, persistErr, closeErr)
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
	close(execution.done)
}

func (m *Manager) activeExecution(childSessionID string) *Execution {
	m.mu.Lock()
	defer m.mu.Unlock()

	active := m.active[childSessionID]
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
	exhausted bool,
	tokenExceeded bool,
) (Outcome, string) {
	if outcome, code, canceled := canceledOutcome(runErr); canceled {
		return outcome, code
	}

	if errors.Is(runErr, ErrInvalidResult) || errors.Is(runErr, agent.ErrGuardrail) {
		return OutcomeFailed, "invalid_result"
	}

	if runErr != nil {
		return OutcomeFailed, "execution_failed"
	}

	if exhausted {
		return OutcomeFailed, "max_tool_calls"
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

	value, err := decodeClonedValue(result.Role, data)
	if err != nil {
		result.Value = nil
		return result
	}

	result.Value = value

	return result
}

func decodeClonedValue(role Role, data []byte) (any, error) {
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
