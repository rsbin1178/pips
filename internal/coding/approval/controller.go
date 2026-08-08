package approval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const maxVolatileOutcomes = 1024

// PendingResultHook observes and may replace the result of a controlled tool
// call after it resumes from the approval boundary. It runs only for calls
// executed by executePending; direct calls are handled by the agent's normal
// after-tool hook chain.
type PendingResultHook func(context.Context, agent.ToolCall, []ai.Part, bool) ([]ai.Part, bool)

// Controller owns every registerable wrapper for approval-controlled tools.
type Controller struct {
	mutex             sync.Mutex
	workspace         workspace.Workspace
	journal           Journal
	resolver          Resolver
	pending           PendingRunner
	policy            execution.Policy
	executor          controllerExecutor
	handlers          map[string]Handler
	tools             map[string]agent.Tool
	permits           map[string]execution.Fingerprint
	volatile          map[string]struct{}
	pendingResultHook PendingResultHook
}

// New constructs a controller for one durable session and workspace.
func New(
	ws workspace.Workspace,
	journal Journal,
	resolver Resolver,
	pending PendingRunner,
	policy execution.Policy,
	executor *execution.Executor,
	handlers ...Handler,
) (*Controller, error) {
	if ws.Root() == "" || ws.Identity().Key() == "" || journal == nil || resolver == nil ||
		pending == nil || executor == nil {
		return nil, errors.New("coding approval: incomplete controller dependencies")
	}

	return newController(
		ws,
		journal,
		resolver,
		pending,
		policy,
		executorAdapter{executor: executor},
		handlers...,
	)
}

type controllerExecutor interface {
	Prepare(context.Context, execution.Operation, execution.Authorization) (preparedExecution, error)
}

// preparedExecution owns one single-use execution plan. Run consumes and
// closes it; Close abandons it before Run.
type preparedExecution interface {
	Run(context.Context, execution.Sink) (execution.Result, error)
	Close() error
}

type executorAdapter struct {
	executor *execution.Executor
}

func (a executorAdapter) Prepare(
	ctx context.Context,
	operation execution.Operation,
	authorization execution.Authorization,
) (preparedExecution, error) {
	plan, err := a.executor.Plan(ctx, operation, authorization)
	if err != nil {
		return nil, err
	}

	return &executorPlan{executor: a.executor, plan: plan}, nil
}

type executorPlan struct {
	executor *execution.Executor
	plan     *execution.Plan
}

func (p *executorPlan) Run(ctx context.Context, sink execution.Sink) (execution.Result, error) {
	return p.executor.Run(ctx, p.plan, sink)
}

func (p *executorPlan) Close() error { return p.plan.Close() }

func newController(
	ws workspace.Workspace,
	journal Journal,
	resolver Resolver,
	pending PendingRunner,
	policy execution.Policy,
	executor controllerExecutor,
	handlers ...Handler,
) (*Controller, error) {
	if executor == nil {
		return nil, errors.New("coding approval: nil controller executor")
	}

	if len(handlers) == 0 {
		return nil, errors.New("coding approval: at least one handler is required")
	}

	controller := &Controller{
		workspace: ws,
		journal:   journal,
		resolver:  resolver,
		pending:   pending,
		policy:    policy,
		executor:  executor,
		handlers:  make(map[string]Handler, len(handlers)),
		tools:     make(map[string]agent.Tool, len(handlers)),
		permits:   make(map[string]execution.Fingerprint),
		volatile:  make(map[string]struct{}),
	}

	for _, handler := range handlers {
		if handler == nil {
			return nil, errors.New("coding approval: nil handler")
		}

		declaration := handler.Decl()
		if !validToolName(declaration.Name) {
			return nil, fmt.Errorf("coding approval: invalid handler name %q", declaration.Name)
		}

		if _, exists := controller.handlers[declaration.Name]; exists {
			return nil, fmt.Errorf("coding approval: duplicate handler %q", declaration.Name)
		}

		controller.handlers[declaration.Name] = handler
		controller.tools[declaration.Name] = &controlledTool{
			controller:  controller,
			handler:     handler,
			declaration: declaration,
		}
	}

	return controller, nil
}

// Tool returns the only registerable wrapper for a controlled handler.
func (c *Controller) Tool(name string) (agent.Tool, bool) {
	if c == nil {
		return nil, false
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	tool, ok := c.tools[name]

	return tool, ok
}

// SetPendingResultHook installs the hook applied after an approval-paused
// controlled call executes. Call it during runtime setup, before the
// controller is used. Passing nil clears the hook.
func (c *Controller) SetPendingResultHook(hook PendingResultHook) {
	if c == nil {
		return
	}

	c.mutex.Lock()
	c.pendingResultHook = hook
	c.mutex.Unlock()
}

// BeforeTool is the agent.WithBeforeTool gate for controlled calls.
func (c *Controller) BeforeTool(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
	if c == nil {
		return agent.DenyTool("approval controller unavailable")
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	replay := replayJournal(c.journal.Path())
	if replay.tainted || c.firstUnknown(replay, nil, false) != nil {
		return agent.ToolDecision{Action: agent.ToolDecisionPause}
	}

	handler, controlled := c.handlers[info.Name]
	if !controlled {
		return agent.ToolDecision{Action: agent.ToolDecisionAllow}
	}

	call := info.ToolCall

	operation, err := c.prepare(ctx, handler, call)
	if err != nil {
		return agent.DenyTool(renderPreparationDenial(handler, err))
	}

	decision := c.policy.Evaluate(operation, replay.grants...)
	switch decision.Verdict() {
	case execution.VerdictAllow:
		if !c.issuePermit(call, operation.Fingerprint()) {
			return agent.DenyTool("approval controller cannot issue an execution permit")
		}

		return agent.ToolDecision{Action: agent.ToolDecisionAllow}
	case execution.VerdictDeny:
		return agent.DenyTool(wrapError(ErrDenied, decision.Reason()).Error())
	case execution.VerdictReview:
		lifecycle := findLifecycle(replay, call, operation.Fingerprint())
		if lifecycle == nil {
			if _, err := c.appendRequest(call, operation); err != nil {
				return agent.DenyTool("approval journal unavailable")
			}
		}

		return agent.ToolDecision{Action: agent.ToolDecisionPause}
	default:
		return agent.DenyTool("approval policy returned an invalid verdict")
	}
}

func renderPreparationDenial(handler Handler, err error) string {
	if handler == nil {
		return "controlled operation arguments are invalid"
	}

	_, renderErr := handler.Render(execution.Result{}, err)
	if renderErr == nil {
		return "controlled operation arguments are invalid"
	}

	return renderErr.Error()
}

// Reconcile repairs durable completions and processes pending calls until the
// first review or unknown barrier.
func (c *Controller) Reconcile(ctx context.Context, sink execution.Sink) (State, error) {
	if c == nil {
		return State{}, errors.New("coding approval: nil controller")
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.reconcileLocked(ctx, sink)
}

// Resolve applies a user decision to the currently displayed durable state.
func (c *Controller) Resolve(
	ctx context.Context,
	resolution Resolution,
	sink execution.Sink,
) (State, error) {
	if c == nil {
		return State{}, errors.New("coding approval: nil controller")
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	invalidReasonChoice := resolution.Reason != "" && resolution.Choice != ChoiceDeny
	invalidReason := !validOptionalJournalText(resolution.Reason, maxDecisionReasonBytes)
	if resolution.RequestID == "" || invalidReasonChoice || invalidReason {
		return State{}, ErrInvalidResolution
	}

	state, err := c.currentStateLocked(ctx)
	if err != nil {
		return State{}, err
	}

	switch state.Kind {
	case StateReview:
		if state.Review == nil || state.Review.RequestID != resolution.RequestID {
			return State{}, ErrInvalidResolution
		}

		return c.resolveReviewLocked(ctx, *state.Review, resolution.Choice, resolution.Reason, sink)
	case StateUnknown:
		if state.Unknown == nil || state.Unknown.RequestID != resolution.RequestID {
			return State{}, ErrInvalidResolution
		}

		return c.resolveUnknownLocked(ctx, *state.Unknown, resolution.Choice, sink)
	default:
		return State{}, ErrInvalidResolution
	}
}

type controlledTool struct {
	controller  *Controller
	handler     Handler
	declaration ai.Tool
}

func (t *controlledTool) Decl() ai.Tool { return t.declaration }

func (t *controlledTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	return t.controller.executeDirect(ctx, t.handler, call)
}

func (c *Controller) executeDirect(
	ctx context.Context,
	handler Handler,
	call agent.ToolCall,
) ([]ai.Part, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	permittedFingerprint, permitted := c.permits[call.ID]
	delete(c.permits, call.ID)

	if !permitted {
		return nil, wrapError(ErrDenied, "before-tool permit missing")
	}

	operation, err := c.prepare(ctx, handler, call)
	if err != nil {
		return nil, err
	}

	if operation.Fingerprint() != permittedFingerprint {
		return nil, wrapError(ErrDenied, "operation changed after gate")
	}

	replay := replayJournal(c.journal.Path())
	if replay.tainted || c.firstUnknown(replay, nil, true) != nil || len(c.volatile) >= maxVolatileOutcomes {
		return nil, ErrOutcomeUnknown
	}

	authorization, verdictErr := c.authorization(operation, nil, replay)
	if verdictErr != nil {
		return nil, verdictErr
	}

	prepared, err := c.executor.Prepare(ctx, operation, authorization)
	if err != nil {
		return handler.Render(execution.Result{}, err)
	}

	lifecycle, err := c.appendRequest(call, operation)
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}

	if err := appendReceipt(c.journal, receiptFor(lifecycle, eventStarted, "", 1, "")); err != nil {
		return nil, errors.Join(err, prepared.Close())
	}

	result, runErr := prepared.Run(ctx, &agentProgressSink{})
	c.volatile[lifecycle.receipt.RequestID] = struct{}{}

	return handler.Render(result, runErr)
}

func (c *Controller) issuePermit(call agent.ToolCall, fingerprint execution.Fingerprint) bool {
	if !validJournalText(call.ID, maxCallIDBytes) || len(c.permits) >= maxVolatileOutcomes {
		return false
	}

	if _, duplicate := c.permits[call.ID]; duplicate {
		return false
	}

	c.permits[call.ID] = fingerprint

	return true
}

func (c *Controller) reconcileLocked(ctx context.Context, sink execution.Sink) (State, error) {
	for {
		if err := ctx.Err(); err != nil {
			return State{}, err
		}

		replay := replayJournal(c.journal.Path())
		if replay.tainted {
			return taintedState(replay), nil
		}

		if repaired, err := c.repairCompletion(replay); err != nil {
			return State{}, err
		} else if repaired {
			continue
		}

		pending, err := c.journal.Pending()
		if err != nil {
			return State{}, fmt.Errorf("coding approval: list pending calls: %w", err)
		}

		pendingByID := indexPending(pending)

		if unknown := c.firstUnknown(replay, pendingByID, false); unknown != nil {
			return State{Kind: StateUnknown, Unknown: unknown}, nil
		}

		if len(pending) == 0 {
			return State{Kind: StateReady}, nil
		}

		barrier, err := c.reconcilePending(ctx, pending, replay, sink)
		if err != nil {
			return State{}, err
		}

		if barrier != nil {
			return *barrier, nil
		}
	}
}

//nolint:gocyclo // Pending suffix routing is one ordered fail-closed state machine.
func (c *Controller) reconcilePending(
	ctx context.Context,
	pending []ai.ToolCallPart,
	replay replayState,
	sink execution.Sink,
) (*State, error) {
	for _, pendingCall := range pending {
		call := agent.ToolCall{ID: pendingCall.ID, Name: pendingCall.Name, Args: slices.Clone(pendingCall.Args)}

		handler, controlled := c.handlers[call.Name]
		if !controlled {
			parts, runErr := c.pending.RunPending(ctx, call, sink)
			if err := c.resolveCall(call, parts, runErr != nil, runErr); err != nil {
				return nil, err
			}

			continue
		}

		operation, err := c.prepare(ctx, handler, call)
		if err != nil {
			if resolveErr := c.resolveRendered(call, handler, execution.Result{}, err); resolveErr != nil {
				return nil, resolveErr
			}

			continue
		}

		lifecycle := findLifecycle(replay, call, operation.Fingerprint())
		if lifecycle != nil && lifecycle.denied {
			if err := c.finishDenial(call, handler, lifecycle, lifecycle.denialReason); err != nil {
				return nil, err
			}

			continue
		}

		authorization, authErr := c.authorization(operation, lifecycle, replay)
		if errors.Is(authErr, ErrApprovalRequired) {
			if lifecycle == nil {
				lifecycle, err = c.appendRequest(call, operation)
				if err != nil {
					return nil, err
				}
			}

			state := reviewState(lifecycle, call, operation)

			return &state, nil
		}

		if authErr != nil {
			if err := c.resolveRendered(call, handler, execution.Result{}, authErr); err != nil {
				return nil, err
			}

			continue
		}

		if lifecycle == nil {
			lifecycle, err = c.appendRequest(call, operation)
			if err != nil {
				return nil, err
			}
		}

		if err := c.executePending(ctx, call, handler, operation, authorization, lifecycle, 1, sink); err != nil {
			return nil, err
		}

		replay = replayJournal(c.journal.Path())
	}

	return nil, nil
}

func (c *Controller) executePending(
	ctx context.Context,
	call agent.ToolCall,
	handler Handler,
	operation execution.Operation,
	authorization execution.Authorization,
	lifecycle *lifecycle,
	attempt int,
	sink execution.Sink,
) error {
	prepared, err := c.executor.Prepare(ctx, operation, authorization)
	if err != nil {
		return err
	}

	if err := appendReceipt(c.journal, receiptFor(lifecycle, eventStarted, "", attempt, "")); err != nil {
		return errors.Join(err, prepared.Close())
	}

	result, runErr := prepared.Run(ctx, sink)

	parts, isError := renderResult(handler, result, runErr)
	if c.pendingResultHook != nil {
		parts, isError = c.pendingResultHook(ctx, call, slices.Clone(parts), isError)
	}
	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: call.ID,
		Content:    parts,
		IsError:    isError,
	}); err != nil {
		return fmt.Errorf("coding approval: persist tool result: %w", err)
	}

	resultName := journalResultSuccess
	if isError {
		resultName = journalResultError
	}

	if err := appendReceipt(c.journal, receiptFor(lifecycle, eventCompleted, "", attempt, resultName)); err != nil {
		return err
	}

	delete(c.volatile, lifecycle.receipt.RequestID)

	return nil
}

//nolint:gocyclo // Review choices intentionally keep durable ordering visible in one state transition.
func (c *Controller) resolveReviewLocked(
	ctx context.Context,
	review Review,
	choice Choice,
	denialReason string,
	sink execution.Sink,
) (State, error) {
	replay := replayJournal(c.journal.Path())

	lifecycle := replay.byRequest[review.RequestID]
	if lifecycle == nil || lifecycle.started > 0 || lifecycle.completed || lifecycle.acknowledged {
		return State{}, ErrInvalidResolution
	}

	pending, err := c.journal.Pending()
	if err != nil {
		return State{}, err
	}

	call, ok := pendingCall(pending, lifecycle.receipt.CallID)
	if !ok {
		return State{}, ErrInvalidResolution
	}

	handler := c.handlers[call.Name]

	operation, err := c.prepare(ctx, handler, call)
	if err != nil || operation.Fingerprint() != lifecycle.fingerprint {
		return State{}, wrapError(ErrInvalidResolution, "operation changed")
	}

	switch choice {
	case ChoiceDeny:
		decision := receiptFor(lifecycle, eventDecided, ChoiceDeny, 0, "")
		decision.Reason = denialReason
		if err := appendReceipt(c.journal, decision); err != nil {
			return State{}, err
		}

		if err := c.finishDenial(call, handler, lifecycle, denialReason); err != nil {
			return State{}, err
		}
	case ChoiceAllowOnce, ChoiceAllowSession:
		if err := appendReceipt(c.journal, receiptFor(lifecycle, eventDecided, choice, 0, "")); err != nil {
			return State{}, err
		}

		authorization, err := c.policy.Approve(operation)
		if err != nil {
			return State{}, err
		}

		if err := c.executePending(ctx, call, handler, operation, authorization, lifecycle, 1, sink); err != nil {
			return State{}, err
		}
	default:
		return State{}, ErrInvalidResolution
	}

	return c.reconcileLocked(ctx, sink)
}

//nolint:gocyclo // Unknown recovery has deliberately explicit, non-overlapping crash transitions.
func (c *Controller) resolveUnknownLocked(
	ctx context.Context,
	unknown Unknown,
	choice Choice,
	sink execution.Sink,
) (State, error) {
	if !unknown.Recoverable {
		return State{}, ErrJournalCorrupt
	}

	replay := replayJournal(c.journal.Path())

	lifecycle := replay.byRequest[unknown.RequestID]
	if lifecycle == nil || lifecycle.started != unknown.Attempt || lifecycle.completed || lifecycle.acknowledged {
		return State{}, ErrInvalidResolution
	}

	pending, err := c.journal.Pending()
	if err != nil {
		return State{}, err
	}

	call, hasPending := pendingCall(pending, lifecycle.receipt.CallID)

	switch choice {
	case ChoiceAcknowledge:
		if hasPending {
			return State{}, ErrInvalidResolution
		}

		if err := appendReceipt(c.journal, receiptFor(
			lifecycle,
			eventAcknowledged,
			ChoiceMarkFailed,
			lifecycle.started,
			"",
		)); err != nil {
			return State{}, err
		}
	case ChoiceMarkFailed:
		if !hasPending {
			return State{}, ErrInvalidResolution
		}

		handler := c.handlers[call.Name]
		if err := c.resolveRendered(call, handler, execution.Result{}, ErrOutcomeUnknown); err != nil {
			return State{}, err
		}

		if err := appendReceipt(c.journal, receiptFor(
			lifecycle,
			eventAcknowledged,
			ChoiceMarkFailed,
			lifecycle.started,
			"",
		)); err != nil {
			return State{}, err
		}
	case ChoiceRetry:
		if !hasPending {
			return State{}, ErrInvalidResolution
		}

		handler := c.handlers[call.Name]

		operation, err := c.prepare(ctx, handler, call)
		if err != nil || operation.Fingerprint() != lifecycle.fingerprint {
			return State{}, wrapError(ErrInvalidResolution, "operation changed")
		}

		nextAttempt := lifecycle.started + 1
		if lifecycle.retryAttempt == 0 {
			if err := appendReceipt(c.journal, receiptFor(
				lifecycle,
				eventDecided,
				ChoiceRetry,
				nextAttempt,
				"",
			)); err != nil {
				return State{}, err
			}
		} else if lifecycle.retryAttempt != nextAttempt {
			return State{}, ErrJournalCorrupt
		}

		authorization, err := c.policy.Approve(operation)
		if err != nil {
			return State{}, err
		}

		if err := c.executePending(
			ctx,
			call,
			handler,
			operation,
			authorization,
			lifecycle,
			nextAttempt,
			sink,
		); err != nil {
			return State{}, err
		}
	default:
		return State{}, ErrInvalidResolution
	}

	return c.reconcileLocked(ctx, sink)
}

func (c *Controller) currentStateLocked(ctx context.Context) (State, error) {
	replay := replayJournal(c.journal.Path())
	if replay.tainted {
		return taintedState(replay), nil
	}

	pending, err := c.journal.Pending()
	if err != nil {
		return State{}, err
	}

	pendingByID := indexPending(pending)
	if unknown := c.firstUnknown(replay, pendingByID, false); unknown != nil {
		return State{Kind: StateUnknown, Unknown: unknown}, nil
	}

	for _, pendingCall := range pending {
		handler, ok := c.handlers[pendingCall.Name]
		if !ok {
			continue
		}

		call := agent.ToolCall{ID: pendingCall.ID, Name: pendingCall.Name, Args: slices.Clone(pendingCall.Args)}

		operation, err := c.prepare(ctx, handler, call)
		if err != nil {
			continue
		}

		lifecycle := findLifecycle(replay, call, operation.Fingerprint())
		if lifecycle != nil && !lifecycle.approved && !lifecycle.denied {
			return reviewState(lifecycle, call, operation), nil
		}
	}

	return State{Kind: StateReady}, nil
}

func (c *Controller) prepare(
	ctx context.Context,
	handler Handler,
	call agent.ToolCall,
) (execution.Operation, error) {
	if handler == nil {
		return execution.Operation{}, errors.New("coding approval: controlled handler unavailable")
	}

	spec, err := handler.Operation(ctx, call)
	if err != nil {
		return execution.Operation{}, err
	}

	operation, err := execution.NewOperation(ctx, c.workspace, spec)
	if err != nil {
		return execution.Operation{}, err
	}

	return operation, nil
}

func (c *Controller) authorization(
	operation execution.Operation,
	lifecycle *lifecycle,
	replay replayState,
) (execution.Authorization, error) {
	decision := c.policy.Evaluate(operation, replay.grants...)
	if authorization, ok := decision.Authorization(); ok {
		return authorization, nil
	}

	if decision.Verdict() == execution.VerdictReview && lifecycle != nil && lifecycle.approved {
		return c.policy.Approve(operation)
	}

	if decision.Verdict() == execution.VerdictReview {
		return execution.Authorization{}, ErrApprovalRequired
	}

	return execution.Authorization{}, wrapError(ErrDenied, decision.Reason())
}

func (c *Controller) appendRequest(
	call agent.ToolCall,
	operation execution.Operation,
) (*lifecycle, error) {
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}

	record := receipt{
		Event:       eventRequested,
		RequestID:   requestID,
		CallID:      call.ID,
		Tool:        call.Name,
		Fingerprint: operation.Fingerprint().String(),
	}
	if err := appendReceipt(c.journal, record); err != nil {
		return nil, err
	}

	replay := replayJournal(c.journal.Path())

	lifecycle := replay.byRequest[requestID]
	if replay.tainted || lifecycle == nil {
		return nil, ErrJournalCorrupt
	}

	return lifecycle, nil
}

func (c *Controller) repairCompletion(replay replayState) (bool, error) {
	for _, lifecycle := range replay.lifecycles {
		if lifecycle.completed || lifecycle.acknowledged {
			continue
		}

		result, ok := replay.results[lifecycle.receipt.CallID]
		lowerBound := lifecycle.startedIndex

		attempt := lifecycle.started
		if lifecycle.denied {
			lowerBound = lifecycle.requestIndex
			attempt = 0
		} else if lifecycle.started < 1 {
			continue
		}

		if !ok || result.index <= lowerBound {
			continue
		}

		if lifecycle.denied && !result.isError {
			return false, ErrJournalCorrupt
		}

		resultName := journalResultSuccess
		if result.isError {
			resultName = journalResultError
		}

		if err := appendReceipt(c.journal, receiptFor(
			lifecycle,
			eventCompleted,
			"",
			attempt,
			resultName,
		)); err != nil {
			return false, err
		}

		delete(c.volatile, lifecycle.receipt.RequestID)

		return true, nil
	}

	return false, nil
}

func (c *Controller) firstUnknown(
	replay replayState,
	pending map[string]ai.ToolCallPart,
	ignoreVolatile bool,
) *Unknown {
	for _, lifecycle := range replay.lifecycles {
		if lifecycle.started < 1 || lifecycle.completed || lifecycle.acknowledged {
			continue
		}

		if result, ok := replay.results[lifecycle.receipt.CallID]; ok && result.index > lifecycle.startedIndex {
			continue
		}

		if ignoreVolatile {
			if _, ok := c.volatile[lifecycle.receipt.RequestID]; ok {
				continue
			}
		}

		_, hasPending := pending[lifecycle.receipt.CallID]

		return &Unknown{
			RequestID:   lifecycle.receipt.RequestID,
			CallID:      lifecycle.receipt.CallID,
			Tool:        lifecycle.receipt.Tool,
			Fingerprint: lifecycle.fingerprint,
			Attempt:     lifecycle.started,
			Pending:     hasPending,
			Reason:      "started_without_durable_result",
			Recoverable: true,
		}
	}

	return nil
}

func (c *Controller) finishDenial(
	call agent.ToolCall,
	handler Handler,
	lifecycle *lifecycle,
	denialReason string,
) error {
	var denialErr error = ErrDenied
	if denialReason != "" {
		denialErr = wrapError(ErrDenied, denialReason)
	}

	parts, isError := renderResult(handler, execution.Result{}, denialErr)
	if denialReason != "" {
		parts = append(parts, ai.TextPart{Text: "[Approval denial]\n" + denialReason})
	}
	if err := c.resolveCall(call, parts, isError, nil); err != nil {
		return err
	}

	return appendReceipt(c.journal, receiptFor(lifecycle, eventCompleted, "", 0, journalResultError))
}

func (c *Controller) resolveRendered(
	call agent.ToolCall,
	handler Handler,
	result execution.Result,
	runErr error,
) error {
	parts, isError := renderResult(handler, result, runErr)

	return c.resolveCall(call, parts, isError, nil)
}

func (c *Controller) resolveCall(
	call agent.ToolCall,
	parts []ai.Part,
	isError bool,
	runErr error,
) error {
	if runErr != nil {
		parts = agent.TextResult(runErr.Error())
		isError = true
	}

	if err := c.resolver.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: call.ID,
		Content:    parts,
		IsError:    isError,
	}); err != nil {
		return fmt.Errorf("coding approval: persist tool result: %w", err)
	}

	return nil
}

func renderResult(
	handler Handler,
	result execution.Result,
	runErr error,
) ([]ai.Part, bool) {
	if handler == nil {
		if runErr == nil {
			runErr = errors.New("coding approval: controlled handler unavailable")
		}

		return agent.TextResult(runErr.Error()), true
	}

	parts, renderErr := handler.Render(result, runErr)
	if renderErr != nil {
		return agent.TextResult(renderErr.Error()), true
	}

	return parts, false
}

func findLifecycle(
	replay replayState,
	call agent.ToolCall,
	fingerprint execution.Fingerprint,
) *lifecycle {
	for _, lifecycle := range slices.Backward(replay.lifecycles) {
		if lifecycle.receipt.CallID == call.ID && lifecycle.receipt.Tool == call.Name &&
			lifecycle.fingerprint == fingerprint && !lifecycle.completed && !lifecycle.acknowledged {
			return lifecycle
		}
	}

	return nil
}

func receiptFor(
	lifecycle *lifecycle,
	event receiptEvent,
	choice Choice,
	attempt int,
	result string,
) receipt {
	record := lifecycle.receipt
	record.Event = event
	record.Choice = choice
	record.Attempt = attempt
	record.Result = result

	return record
}

func reviewState(lifecycle *lifecycle, call agent.ToolCall, operation execution.Operation) State {
	call.Args = slices.Clone(call.Args)

	return State{
		Kind: StateReview,
		Review: &Review{
			RequestID: lifecycle.receipt.RequestID,
			Call:      call,
			Operation: operation,
		},
	}
}

func taintedState(replay replayState) State {
	return State{
		Kind: StateUnknown,
		Unknown: &Unknown{
			RequestID:   replay.taintEntry,
			Reason:      "malformed_journal",
			Recoverable: false,
		},
	}
}

func indexPending(pending []ai.ToolCallPart) map[string]ai.ToolCallPart {
	byID := make(map[string]ai.ToolCallPart, len(pending))
	for _, call := range pending {
		byID[call.ID] = call
	}

	return byID
}

func pendingCall(pending []ai.ToolCallPart, id string) (agent.ToolCall, bool) {
	for _, call := range pending {
		if call.ID == id {
			return agent.ToolCall{ID: call.ID, Name: call.Name, Args: slices.Clone(call.Args)}, true
		}
	}

	return agent.ToolCall{}, false
}

type agentProgressSink struct {
	stdout int64
	stderr int64
}

func (s *agentProgressSink) WriteOutput(ctx context.Context, chunk execution.OutputChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	stream := "stdout"
	total := &s.stdout

	if chunk.Stream == execution.StreamStderr {
		stream = "stderr"
		total = &s.stderr
	}

	*total += int64(len(chunk.Data))

	agent.ReportProgress(ctx, ai.TextPart{Text: fmt.Sprintf(
		"shell progress: %s +%d bytes (total %d)",
		stream,
		len(chunk.Data),
		*total,
	)})

	return nil
}
