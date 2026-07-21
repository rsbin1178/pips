//nolint:wsl_v5 // Interaction transitions keep durable and reducer commits adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/changes/git"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/tools"
)

var errConsumerStopped = errors.New("coding runtime: event consumer stopped")

type eventEmitter struct {
	runtime          *Runtime
	observeTelemetry func(Event) []IntegrationDiagnostic
	yield            func(Event, error) bool
	alive            bool
}

func newEventEmitter(
	ctx context.Context,
	runtime *Runtime,
	yield func(Event, error) bool,
	alive bool,
) *eventEmitter {
	return &eventEmitter{
		runtime: runtime,
		observeTelemetry: func(event Event) []IntegrationDiagnostic {
			return runtime.observeEvent(ctx, event)
		},
		yield: yield,
		alive: alive,
	}
}

func (e *eventEmitter) emit(
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) error {
	event, err := e.runtime.writer.write(interactionID, runID, eventType, payload)
	if err != nil {
		return err
	}

	return e.publish(event)
}

func (e *eventEmitter) publish(event Event) error {
	e.runtime.mu.Lock()
	next, err := Reduce(e.runtime.state, event)
	if err == nil {
		e.runtime.state = next
	}
	e.runtime.mu.Unlock()
	if err != nil {
		return err
	}

	diagnostics := e.observeTelemetry(event)

	consumerStopped := false
	if e.alive && !e.yield(event, nil) {
		e.alive = false
		consumerStopped = true
	}

	for _, diagnostic := range diagnostics {
		if err := e.emit("", "", EventIntegrationDiagnostic, diagnostic); err != nil {
			if errors.Is(err, errConsumerStopped) {
				consumerStopped = true

				continue
			}

			return err
		}
	}

	if consumerStopped {
		return errConsumerStopped
	}

	return nil
}

func (e *eventEmitter) fail(err error) {
	if err != nil && e.alive {
		e.yield(Event{}, err)
	}
}

//nolint:gocyclo,funlen // Prompt/continue/resolve share one auditable transition driver.
func (r *Runtime) run(
	parent context.Context,
	kind runtimeOperationKind,
	resolution approval.Resolution,
	messages []ai.Message,
	yield func(Event, error) bool,
) {
	ctx, operation, err := r.beginOperation(parent, kind, messages)
	if err != nil {
		yield(Event{}, err)
		return
	}
	defer r.endOperation(operation)

	emitter := newEventEmitter(ctx, r, yield, true)

	var current *interaction
	switch kind {
	case operationPrompt:
		interactionID, startErr := r.journal.start()
		if startErr != nil {
			emitter.fail(startErr)
			return
		}

		if err = emitter.emit(
			interactionID,
			"",
			EventInteractionStarted,
			InteractionStarted{},
		); err != nil {
			finishErr := r.finishUnopenedInteraction(
				interactionID,
				InteractionCanceled,
				emitter,
			)
			if !errors.Is(err, errConsumerStopped) {
				emitter.fail(errors.Join(err, finishErr))
			}
			return
		}

		current, err = r.openInteraction(ctx, interactionID, false, emitter)
		if err != nil {
			outcome := InteractionFailed
			if errors.Is(err, errConsumerStopped) || errors.Is(err, context.Canceled) {
				outcome = InteractionCanceled
			}

			finishErr := r.finishUnopenedInteraction(
				interactionID,
				outcome,
				emitter,
			)
			if !errors.Is(err, errConsumerStopped) {
				emitter.fail(errors.Join(err, finishErr))
			}
			return
		}

		if err = emitter.emit(
			current.id,
			"",
			EventStatusChanged,
			StatusChanged{Phase: PhaseRunning},
		); err != nil {
			finishErr := r.finishInteraction(ctx, current, InteractionCanceled, emitter)
			emitter.fail(errors.Join(err, finishErr))
			return
		}
	case operationContinue:
		current, err = r.openInteraction(ctx, r.recovery.PendingID, true, emitter)
		if err == nil {
			err = r.reconcileAndContinue(ctx, current, emitter)
		}
	case operationResolve:
		r.mu.Lock()
		current = r.interaction
		r.mu.Unlock()

		var approvalState approval.State
		approvalState, err = r.controller.Resolve(ctx, resolution, nil)
		if err == nil {
			err = emitter.emit(
				current.id,
				"",
				EventApprovalResolved,
				ApprovalResolved{
					RequestID: resolution.RequestID,
					Choice:    resolution.Choice,
				},
			)
		}

		if err == nil {
			err = r.handleApprovalState(ctx, current, approvalState, emitter)
		}
	}

	if err != nil {
		outcome := InteractionFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, errConsumerStopped) ||
			errors.Is(parent.Err(), context.Canceled) {
			outcome = InteractionCanceled
		}

		finishErr := r.finishInteraction(context.WithoutCancel(ctx), current, outcome, emitter)
		if !errors.Is(err, errConsumerStopped) {
			emitter.fail(errors.Join(err, finishErr))
		}

		return
	}

	if current == nil || r.interactionPaused(current) {
		return
	}

	for {
		stop, driveErr := r.driveHarness(ctx, current, messages, emitter)
		messages = nil
		if driveErr != nil {
			outcome := InteractionFailed
			if errors.Is(driveErr, context.Canceled) || errors.Is(driveErr, errConsumerStopped) ||
				errors.Is(parent.Err(), context.Canceled) {
				outcome = InteractionCanceled
			}

			errorEventErr := r.emitRunError(current, driveErr, outcome, emitter)
			finishErr := r.finishInteraction(context.WithoutCancel(ctx), current, outcome, emitter)
			if !errors.Is(driveErr, errConsumerStopped) {
				emitter.fail(errors.Join(driveErr, errorEventErr, finishErr))
			}

			return
		}

		if stop != agent.StopPaused {
			finishErr := r.finishInteraction(
				context.WithoutCancel(ctx),
				current,
				InteractionSucceeded,
				emitter,
			)
			emitter.fail(finishErr)

			return
		}

		if err := r.reconcileAndContinue(ctx, current, emitter); err != nil {
			finishErr := r.finishInteraction(
				context.WithoutCancel(ctx),
				current,
				InteractionFailed,
				emitter,
			)
			emitter.fail(errors.Join(err, finishErr))

			return
		}

		if r.interactionPaused(current) {
			return
		}
	}
}

func (r *Runtime) finishUnopenedInteraction(
	interactionID string,
	outcome InteractionOutcome,
	emitter *eventEmitter,
) error {
	journalErr := r.journal.complete(interactionID, outcome, TokenUsage{}, 0)
	eventErr := emitter.emit(
		interactionID,
		"",
		EventInteractionCompleted,
		InteractionCompleted{Outcome: outcome},
	)

	return errors.Join(journalErr, eventErr)
}

//nolint:gocyclo // Every public operation has distinct durable preconditions.
func (r *Runtime) beginOperation(
	parent context.Context,
	kind runtimeOperationKind,
	messages []ai.Message,
) (context.Context, *runtimeOperation, error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing {
		return nil, nil, stateError(string(kind), r.state.Phase, ErrRuntimeClosed)
	}

	if r.active != nil {
		return nil, nil, stateError(string(kind), r.state.Phase, ErrRuntimeBusy)
	}

	switch kind {
	case operationPrompt:
		if len(messages) == 0 {
			return nil, nil, fmt.Errorf("%w: prompt messages are required", ErrRuntimeInvalid)
		}

		if r.state.Phase != PhaseIdle || r.interaction != nil || r.recovery.PendingID != "" {
			return nil, nil, stateError(string(kind), r.state.Phase, ErrRuntimePending)
		}
	case operationContinue:
		if r.state.Phase != PhasePaused || r.interaction != nil || r.recovery.PendingID == "" {
			return nil, nil, stateError(string(kind), r.state.Phase, ErrRuntimeNotPaused)
		}
	case operationResolve:
		if r.state.Phase != PhasePaused || r.interaction == nil {
			return nil, nil, stateError(string(kind), r.state.Phase, ErrRuntimeNotPaused)
		}
	default:
		return nil, nil, fmt.Errorf("%w: unknown operation", ErrRuntimeInvalid)
	}

	ctx, cancel := context.WithCancel(parent)
	operation := &runtimeOperation{cancel: cancel, done: make(chan struct{})}
	r.active = operation

	return ctx, operation, nil
}

func (r *Runtime) endOperation(operation *runtimeOperation) {
	operation.cancel()

	r.mu.Lock()
	if r.active == operation {
		r.active = nil
	}
	close(operation.done)
	r.mu.Unlock()
}

//nolint:gocyclo,funlen // Snapshot construction preserves catalog and lease rollback boundaries.
func (r *Runtime) openInteraction(
	ctx context.Context,
	interactionID string,
	resumed bool,
	emitter *eventEmitter,
) (_ *interaction, returnErr error) {
	if snapshot, attempted, err := r.connections.RefreshChanged(ctx); err != nil {
		_ = emitter.emit("", "", EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: componentMCP, Code: "refresh_failed",
			Message: "MCP refresh failed; the previous tool snapshot remains active",
		})
	} else if attempted {
		_ = snapshot
	}

	activation, err := r.extensions.Acquire()
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, activation.Release(context.WithoutCancel(ctx)))
		}
	}()

	snapshot := activation.Snapshot()
	skills, diagnostics, err := r.resources.ResolveSkills(snapshot.SkillEntries()...)
	if err != nil {
		return nil, err
	}

	for _, diagnostic := range diagnostics {
		if err := emitter.emit(interactionID, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: "resource", Code: diagnostic.Code, Message: diagnostic.Message,
		}); err != nil {
			return nil, err
		}
	}

	skillCatalog, err := harness.NewSkillCatalog(skills...)
	if err != nil {
		return nil, err
	}

	skillTool, err := harness.NewSkillTool(skillCatalog)
	if err != nil {
		return nil, err
	}

	shell, ok := r.controller.Tool("shell")
	if !ok {
		return nil, errors.New("coding runtime: controlled shell is unavailable")
	}

	localCatalog, err := tools.NewCatalog(
		r.tree,
		r.opts.ToolLimits,
		tools.WithControlledShell(shell),
	)
	if err != nil {
		return nil, err
	}

	skillTools, err := catalog.New(catalog.Local("coding.skills", catalog.RiskRead, skillTool)...)
	if err != nil {
		return nil, err
	}

	mcpCatalog, err := catalog.New(r.connections.Snapshot().Entries...)
	if err != nil {
		return nil, err
	}

	merged, err := catalog.Merge(localCatalog, skillTools, snapshot.Catalog(), mcpCatalog)
	if err != nil {
		return nil, err
	}

	policy := catalog.AllowAll(r.workspace.Identity().Key(), catalog.RiskPrivileged)
	search, err := catalog.NewToolSearch(merged, policy, catalog.ToolSearchOptions{
		Enabled: r.config.ToolSearch,
	})
	if err != nil {
		return nil, err
	}

	visibleTools, err := search.Tools(ctx)
	if err != nil {
		return nil, err
	}

	allTools, err := merged.Snapshot(ctx, policy)
	if err != nil {
		return nil, err
	}
	allTools = appendUniqueTools(allTools, visibleTools...)

	extensionHooks := snapshot.Hooks()
	extensionObserver := newGuardedAgentObserver(extensionHooks.Observe)
	controlHooks := extensionHooks
	controlHooks.Observe = nil
	composed := extension.ComposeHooks(
		controlHooks,
		extension.Hooks{BeforeTool: r.controller.BeforeTool},
		extension.Hooks{PrepareTurn: search.PrepareTurn},
	)

	harnessOptions := []harness.Option{
		harness.WithTools(visibleTools...),
		harness.WithSkillCatalog(skillCatalog),
		harness.WithTemplates(snapshot.Prompts()...),
		harness.WithAgentOptions(append(
			composed.AgentOptions(),
			agent.WithToolTimeout(r.opts.ToolTimeout),
		)...),
		harness.WithOnEvent(func(eventCtx context.Context, event agent.Event) {
			extensionObserver.observe(eventCtx, event)
			r.observers.observe(eventCtx, event)
		}),
	}

	value, err := harness.New(snapshot.Model(r.model), r.session, harnessOptions...)
	if err != nil {
		return nil, err
	}

	if err := r.pending.set(allTools, extensionHooks, r.opts.ToolTimeout); err != nil {
		return nil, err
	}
	r.resolver.set(value)

	current := &interaction{
		id: interactionID, startedAt: time.Now().UTC(), resumed: resumed,
		activation: activation, harness: value, search: search, observer: extensionObserver,
	}

	baseline, captureErr := r.inspector.Capture(ctx)
	if captureErr == nil {
		current.baseline = baseline
		current.hasBaseline = true
	} else {
		code := "capture_failed"
		if errors.Is(captureErr, git.ErrNotRepository) {
			code = "not_repository"
		}

		if err := emitter.emit(interactionID, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: "changes", Code: code,
			Message:  "workspace change attribution is unavailable for this interaction",
			Disabled: true,
		}); err != nil {
			r.pending.clear()
			r.resolver.set(nil)

			return nil, err
		}
	}

	r.mu.Lock()
	if r.closing || r.closed || r.interaction != nil {
		r.mu.Unlock()
		r.pending.clear()
		r.resolver.set(nil)

		return nil, stateError("open interaction", r.state.Phase, ErrRuntimeClosed)
	}
	r.interaction = current
	r.mu.Unlock()

	activation = nil

	return current, nil
}

func appendUniqueTools(values []agent.Tool, additions ...agent.Tool) []agent.Tool {
	result := slices.Clone(values)
	seen := make(map[string]struct{}, len(result)+len(additions))
	for _, tool := range result {
		seen[tool.Decl().Name] = struct{}{}
	}

	for _, tool := range additions {
		if _, duplicate := seen[tool.Decl().Name]; duplicate {
			continue
		}

		seen[tool.Decl().Name] = struct{}{}
		result = append(result, tool)
	}

	return result
}

//nolint:gocyclo,nestif // Projection mirrors the ordered Agent stream protocol.
func (r *Runtime) driveHarness(
	ctx context.Context,
	current *interaction,
	messages []ai.Message,
	emitter *eventEmitter,
) (agent.StopReason, error) {
	projector, err := newAgentProjector(r.writer, current.id)
	if err != nil {
		return "", err
	}

	var stop agent.StopReason
	inputPending := cloneMessages(messages)
	for event, streamErr := range current.harness.PromptMessagesStream(ctx, messages...) {
		if event.Type != "" {
			projected, projectErr := projector.project(event)
			if projectErr != nil {
				return "", projectErr
			}

			if completed, ok := projected.Payload.(RunCompleted); ok {
				stop = completed.Stop
				addUsage(&current.usage, completed.Usage)
				current.activeRunID = ""
			}

			if _, ok := projected.Payload.(RunStarted); ok {
				current.activeRunID = projected.RunID
				current.runIDs = append(current.runIDs, projected.RunID)
			}

			if err := emitter.publish(projected); err != nil {
				return "", err
			}

			if event.Type == agent.EventTurnStart && len(inputPending) > 0 {
				for _, message := range inputPending {
					if err := emitter.emit(
						current.id,
						event.RunID,
						EventMessageCommitted,
						MessageCommitted{Message: message},
					); err != nil {
						return "", err
					}
				}

				inputPending = nil
			}

			if err := r.emitObserverDiagnostics(current, emitter); err != nil {
				return "", err
			}
		}

		if streamErr != nil {
			return "", streamErr
		}
	}

	if stop == "" {
		return "", errors.New("coding runtime: Agent stream ended without run completion")
	}

	return stop, nil
}

func (r *Runtime) emitObserverDiagnostics(
	current *interaction,
	emitter *eventEmitter,
) error {
	if current.observer.drainDisabled() {
		if err := emitter.emit(current.id, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: "observer", Code: "extension_observer_disabled",
			Message:  "an Extension event observer panicked and was disabled",
			Disabled: true,
		}); err != nil {
			return err
		}
	}

	for range r.observers.drainDisabled() {
		if err := emitter.emit(current.id, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: "observer", Code: "agent_observer_disabled",
			Message:  "an Agent event observer panicked and was disabled",
			Disabled: true,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (r *Runtime) emitRunError(
	current *interaction,
	runErr error,
	outcome InteractionOutcome,
	emitter *eventEmitter,
) error {
	code := "run_failed"
	message := "Agent run failed"
	if outcome == InteractionCanceled || errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, errConsumerStopped) {
		code = "run_canceled"
		message = "Agent run was canceled"
	}

	return emitter.emit(current.id, current.activeRunID, EventError, RuntimeError{
		Code: code, Message: message, Fatal: true,
	})
}

func (r *Runtime) reconcileAndContinue(
	ctx context.Context,
	current *interaction,
	emitter *eventEmitter,
) error {
	state, err := r.controller.Reconcile(ctx, nil)
	if err != nil {
		return err
	}

	return r.handleApprovalState(ctx, current, state, emitter)
}

func (r *Runtime) handleApprovalState(
	_ context.Context,
	current *interaction,
	state approval.State,
	emitter *eventEmitter,
) error {
	switch state.Kind {
	case approval.StateReady:
		if r.Snapshot().Phase == PhasePaused {
			return emitter.emit(current.id, "", EventStatusChanged, StatusChanged{Phase: PhaseRunning})
		}

		return nil
	case approval.StateReview:
		if state.Review == nil {
			return approval.ErrJournalCorrupt
		}

		if r.Snapshot().Phase == PhaseRunning {
			if err := emitter.emit(
				current.id,
				"",
				EventStatusChanged,
				StatusChanged{Phase: PhasePaused},
			); err != nil {
				return err
			}
		}

		return emitter.emit(
			current.id,
			"",
			EventApprovalRequired,
			approvalRequired(*state.Review),
		)
	case approval.StateUnknown:
		if state.Unknown == nil {
			return approval.ErrJournalCorrupt
		}

		if r.Snapshot().Phase == PhaseRunning {
			if err := emitter.emit(
				current.id,
				"",
				EventStatusChanged,
				StatusChanged{Phase: PhasePaused},
			); err != nil {
				return err
			}
		}

		return emitter.emit(
			current.id,
			"",
			EventApprovalUnknown,
			approvalUnknown(*state.Unknown),
		)
	default:
		return approval.ErrJournalCorrupt
	}
}

func approvalRequired(review approval.Review) ApprovalRequired {
	command := append([]string{review.Operation.Executable()}, review.Operation.Args()...)

	return ApprovalRequired{
		RequestID:     review.RequestID,
		CallID:        review.Call.ID,
		Tool:          review.Call.Name,
		Command:       command,
		CWD:           review.Operation.CWD(),
		Justification: review.Operation.Justification(),
		Choices: []approval.Choice{
			approval.ChoiceAllowOnce,
			approval.ChoiceAllowSession,
			approval.ChoiceDeny,
		},
	}
}

func approvalUnknown(unknown approval.Unknown) ApprovalUnknown {
	choices := []approval.Choice{approval.ChoiceAcknowledge}
	if unknown.Recoverable && unknown.Pending {
		choices = []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed}
	}

	attempt := max(unknown.Attempt, 1)

	fingerprint := unknown.Fingerprint.String()
	if fingerprint == "0000000000000000000000000000000000000000000000000000000000000000" {
		fingerprint = "unknown"
	}

	callID := unknown.CallID
	if callID == "" {
		callID = "unknown"
	}

	tool := unknown.Tool
	if tool == "" {
		tool = "unknown"
	}

	return ApprovalUnknown{
		RequestID:   unknown.RequestID,
		CallID:      callID,
		Tool:        tool,
		Fingerprint: fingerprint,
		Attempt:     attempt,
		Pending:     unknown.Pending,
		Reason:      unknown.Reason,
		Recoverable: unknown.Recoverable,
		Choices:     choices,
	}
}

func (r *Runtime) interactionPaused(current *interaction) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.interaction == current && r.state.Phase == PhasePaused
}

func (r *Runtime) finishInteraction(
	ctx context.Context,
	current *interaction,
	outcome InteractionOutcome,
	emitter *eventEmitter,
) error {
	if current == nil {
		return nil
	}

	errs := make([]error, 0, 4)
	if current.hasBaseline {
		report, err := r.inspector.Changes(ctx, current.baseline)
		if err != nil {
			diagnosticErr := emitter.emit(current.id, "", EventIntegrationDiagnostic, IntegrationDiagnostic{
				Component: "changes", Code: "report_failed",
				Message:  "workspace change attribution failed",
				Disabled: true,
			})
			errs = append(errs, diagnosticErr)
		} else if err := emitter.emit(
			current.id,
			"",
			EventWorkspaceChanged,
			workspaceChanged(report),
		); err != nil {
			errs = append(errs, err)
		}
	}

	duration := max(time.Since(current.startedAt), time.Duration(0))
	durationMillis := min(duration.Milliseconds(), maxEventDurationMS)

	if err := r.journal.complete(current.id, outcome, current.usage, durationMillis); err != nil {
		errs = append(errs, err)
	}

	if err := emitter.emit(current.id, "", EventInteractionCompleted, InteractionCompleted{
		Outcome: outcome, Usage: current.usage, DurationMillis: durationMillis,
	}); err != nil {
		errs = append(errs, err)
	}

	if err := emitter.emit(current.id, "", EventStatusChanged, StatusChanged{Phase: PhaseIdle}); err != nil {
		errs = append(errs, err)
	}

	for _, runID := range current.runIDs {
		current.search.Forget(runID)
	}
	r.pending.clear()
	r.resolver.set(nil)

	r.mu.Lock()
	if r.interaction == current {
		r.interaction = nil
	}
	r.recovery.PendingID = ""
	r.mu.Unlock()

	if err := current.activation.Release(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func workspaceChanged(report changes.Report) WorkspaceChanged {
	entries := report.Entries()
	projected := make([]WorkspaceChange, 0, min(len(entries), maxEventItems))
	truncated := report.Truncated()
	for _, entry := range entries {
		if len(projected) == maxEventItems {
			truncated = true

			break
		}

		if !validWorkspacePath(entry.Path) ||
			(entry.PreviousPath != "" && !validWorkspacePath(entry.PreviousPath)) {
			truncated = true

			continue
		}

		projected = append(projected, WorkspaceChange{
			Path: entry.Path, PreviousPath: entry.PreviousPath, Kind: entry.Kind,
		})
	}

	diff, diffTruncated := boundedEventText(report.Diff(), maxEventTextBytes)

	return WorkspaceChanged{
		Entries: projected, Diff: diff, Truncated: truncated || diffTruncated,
	}
}

func boundedEventText(value string, maximum int) (string, bool) {
	truncated := false
	if len(value) > maximum {
		end := maximum
		for end > 0 && !utf8.ValidString(value[:end]) {
			end--
		}

		value = value[:end]
		truncated = true
	}

	if !validBoundedText(value, maximum, true) {
		return "", true
	}

	return value, truncated
}

// Steer queues messages into the current Agent invocation.
func (r *Runtime) Steer(messages ...ai.Message) error {
	current, err := r.activeHarness("steer")
	if err != nil {
		return err
	}

	return current.Steer(cloneMessages(messages)...)
}

// FollowUp queues messages after the current Agent invocation settles.
func (r *Runtime) FollowUp(messages ...ai.Message) error {
	current, err := r.activeHarness("follow up")
	if err != nil {
		return err
	}

	return current.FollowUp(cloneMessages(messages)...)
}

func (r *Runtime) activeHarness(operation string) (*harness.Harness, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing {
		return nil, stateError(operation, r.state.Phase, ErrRuntimeClosed)
	}

	if r.active == nil || r.interaction == nil {
		return nil, stateError(operation, r.state.Phase, ErrRuntimeBusy)
	}

	return r.interaction.harness, nil
}

// Cancel requests cancellation of the current operation.
func (r *Runtime) Cancel() error {
	if r == nil {
		return ErrRuntimeClosed
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()
		return stateError("cancel", phase, ErrRuntimeClosed)
	}

	operation := r.active
	phase := r.state.Phase
	r.mu.Unlock()
	if operation == nil {
		return stateError("cancel", phase, ErrRuntimeBusy)
	}

	operation.cancel()

	return nil
}

var (
	_ approval.PendingRunner = (*pendingRunner)(nil)
	_ approval.Resolver      = (*activeResolver)(nil)
	_ codingmcp.ProjectGit   = (*git.Inspector)(nil)
)
