package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const readToolName = "read"

// ExecutionOptions provides embedder/test overrides without adding a user
// configuration protocol in P1. A zero Limits value selects DefaultLimits.
type ExecutionOptions struct {
	Limits Limits
}

// Config contains the application-owned dependencies for one parent Session.
type Config struct {
	Repository     *session.Repository
	Parent         *session.Handle
	Tree           *workspace.Tree
	Model          ai.LanguageModel
	RequestPolicy  func(*ai.Request)
	Options        ExecutionOptions
	AgentObservers []func(context.Context, agent.Event)
}

// Manager owns at most one live child execution and all of its cleanup.
type Manager struct {
	mu        sync.Mutex
	journalMu sync.Mutex

	config       Config
	limits       Limits
	tools        []agent.Tool
	active       *Execution
	starting     bool
	startingDone chan struct{}
	closed       bool
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
}

// New constructs a current-parent child manager. Call Reconcile before New so
// interrupted child journals are repaired before the manager accepts work.
func New(config Config) (*Manager, error) {
	if err := validateManagerConfig(config); err != nil {
		return nil, err
	}

	limits := config.Options.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	readTools, err := buildReadTools(config)
	if err != nil {
		return nil, err
	}

	config.AgentObservers = slices.Clone(config.AgentObservers)

	return &Manager{config: config, limits: limits, tools: readTools}, nil
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

	names := []string{readToolName, "ls", "glob", "grep"}
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

// Start acquires the single execution slot and starts one owned goroutine.
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

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}

	if m.starting || m.active != nil {
		m.mu.Unlock()
		return nil, ErrBusy
	}

	m.starting = true
	m.startingDone = make(chan struct{})
	m.mu.Unlock()

	parentMeta := m.config.Parent.Metadata()

	parentRunID := ""
	if metadata, ok := agent.RunMetadataFromContext(ctx); ok {
		parentRunID = metadata.RunID
	}

	child, err := m.config.Repository.Create(ctx, session.CreateOptions{
		WorkspaceID:     parentMeta.WorkspaceID,
		Kind:            session.KindSubagent,
		ParentSessionID: parentMeta.ID,
		ParentRunID:     parentRunID,
		Agent:           string(request.Role),
	})
	if err != nil {
		m.clearStarting()
		return nil, err
	}

	created := record{
		Schema:          recordSchema,
		State:           StateCreated,
		Role:            request.Role,
		ChildSessionID:  child.Metadata().ID,
		ParentSessionID: parentMeta.ID,
		ParentRunID:     parentRunID,
		Model:           modelName(m.config.Model),
		Limits:          journalLimits(m.limits),
		TaskPreview:     preview(request.Task),
		Time:            time.Now().UTC(),
	}
	if err := m.appendMirrored(child.Session(), created); err != nil {
		m.clearStarting()
		return nil, errors.Join(err, child.Close())
	}

	runCtx, cancel := context.WithTimeout(ctx, m.limits.MaxDuration)
	tracker := newRunTracker(created.Time, m.limits.MaxToolCalls)
	execution := &Execution{
		manager: m,
		request: request,
		cancel:  cancel,
		done:    make(chan struct{}),
		child:   child,
		tracker: tracker,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()

		terminal := created
		terminal.State = StateCanceled
		terminal.Code = "manager_closed"
		terminal.Time = time.Now().UTC()
		persistErr := m.appendMirrored(child.Session(), terminal)
		closeErr := child.Close()

		m.clearStarting()

		return nil, errors.Join(ErrClosed, persistErr, closeErr)
	}

	m.active = execution
	m.finishStartingLocked()
	m.mu.Unlock()

	observerErr := emitObserver(runCtx, observer, eventFromRecord(created))
	if observerErr != nil {
		execution.Cancel()
	}

	go m.run(runCtx, execution, child, observer, created, observerErr)

	return execution, nil
}

func (m *Manager) clearStarting() {
	m.mu.Lock()
	m.finishStartingLocked()
	m.mu.Unlock()
}

func (m *Manager) finishStartingLocked() {
	if m.startingDone != nil {
		close(m.startingDone)
	}

	m.starting = false
	m.startingDone = nil
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

// Close stops admission, cancels the active child, and waits for its terminal
// persistence before returning.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}

	for {
		m.mu.Lock()
		m.closed = true
		active := m.active
		startingDone := m.startingDone
		m.mu.Unlock()

		if active != nil {
			active.Cancel()
			result, err := active.Wait(ctx)
			active.mu.Lock()
			cleanupErr := active.cleanupErr
			active.mu.Unlock()

			if cleanupErr != nil {
				return cleanupErr
			}

			if result.Outcome == OutcomeCanceled && ctx.Err() == nil {
				return nil
			}

			return err
		}

		if startingDone == nil {
			return nil
		}

		select {
		case <-startingDone:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type runTracker struct {
	mu sync.Mutex

	started   bool
	runID     string
	turns     int
	toolCalls int
	exhausted bool
	usage     ai.Usage
	value     any
	text      string
	err       error
	activity  Activity
	toolIndex map[string]int
	maxTools  int
}

type progressSnapshot struct {
	runID     string
	turns     int
	toolCalls int
	usage     ai.Usage
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
		m.observeChild(eventCtx, child, observer, created, tracker, event)
	}
	options := []agent.Option{
		agent.WithName("subagent/" + string(execution.request.Role)),
		agent.WithMaxTurns(m.limits.MaxTurns),
		agent.WithMaxTokens(m.limits.MaxTokens),
		agent.WithParallelTools(1),
		agent.WithBeforeTool(func(_ context.Context, _ agent.ToolCallInfo) agent.ToolDecision {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			if tracker.toolCalls >= m.limits.MaxToolCalls {
				tracker.exhausted = true
				return agent.DenyTool("subagent tool-call budget exhausted")
			}

			tracker.toolCalls++

			return agent.ToolDecision{}
		}),
		agent.WithStopWhen(func(agent.RunInfo) bool {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()

			return tracker.exhausted
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

func (m *Manager) observeChild(
	ctx context.Context,
	child *session.Handle,
	observer Observer,
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
	}, cloneActivity(t.activity)
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

	observerErr := emitObserver(context.WithoutCancel(ctx), observer, eventFromRecord(terminal))
	finalErr = errors.Join(finalErr, observerErr)
	cleanupErr = errors.Join(cleanupErr, observerErr)

	execution.mu.Lock()
	execution.result = result
	execution.err = finalErr
	execution.cleanupErr = cleanupErr
	execution.mu.Unlock()
	m.mu.Lock()
	if m.active == execution {
		m.active = nil
	}
	m.mu.Unlock()
	close(execution.done)
}

func (m *Manager) activeExecution(childSessionID string) *Execution {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active == nil || m.active.child == nil ||
		m.active.child.Metadata().ID != childSessionID {
		return nil
	}

	return m.active
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
		State:           value.State,
		Role:            value.Role,
		ChildSessionID:  value.ChildSessionID,
		ParentSessionID: value.ParentSessionID,
		ParentRunID:     value.ParentRunID,
		ChildRunID:      value.ChildRunID,
		Model:           value.Model,
		TaskPreview:     value.TaskPreview,
		Code:            value.Code,
		Stop:            agent.StopReason(value.Stop),
		Turns:           value.Turns,
		ToolCalls:       value.ToolCalls,
		Usage:           value.Usage,
		Duration:        time.Duration(value.DurationMillis) * time.Millisecond,
		Time:            value.Time,
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
