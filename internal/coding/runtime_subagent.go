//nolint:wsl_v5 // Parent lifecycle and child projection commits stay adjacent.
package coding

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

type childProjection struct {
	state         State
	writer        *eventWriter
	projector     *agentProjector
	interactionID string
	pendingInput  []ai.Message
	// taskPublished records that the child's own task already reached its
	// projection as the opening user message. Every child agent event carries the
	// task, so without this marker a child that runs a second turn would publish
	// its prompt again at that turn boundary.
	taskPublished bool
	finished      bool
}

// subagentObserver projects one child's lifecycle into the conversation. current
// is the interaction the child's Run belongs to; a scheduled fire has none, so a
// nil interaction is the supported case rather than an accident: the child is
// projected into its own session and its background completion is delivered
// durably, with no conversation Run to attribute it to.
func (r *Runtime) subagentObserver(
	current *interaction,
	emitter *eventEmitter,
) subagent.Observer {
	return func(ctx context.Context, event subagent.Event) error {
		err := r.observeSubagent(ctx, current, emitter, event)

		// A scheduled fire skipped while its previous child ran resumes on that
		// child's terminal event, so the firing loop is woken here rather than
		// polling.
		if isTerminalSubagentState(event.State) {
			r.signalScheduler()
		}

		return err
	}
}

// observeSubagent commits one child lifecycle. A nil current is a scheduled
// fire's child: the conversation has no active Run to attribute it to, so the
// child is projected into its own session and its background completion rides
// the durable notification path. The conversation's reducer accepts a
// SubagentLifecycle only against an active interaction, so nothing is emitted
// for an unattributed child.
func (r *Runtime) observeSubagent(
	ctx context.Context,
	current *interaction,
	emitter *eventEmitter,
	event subagent.Event,
) error {
	if current == nil {
		return r.observeUnattributedChild(ctx, event)
	}

	eventType := subagentEventType(event)
	nested := r.subagentEventIsNested(current, event)

	payload := SubagentLifecycle{
		Identity: event.Identity, Role: event.Role, State: event.State,
		ChildSessionID:      event.ChildSessionID,
		ParentInteractionID: event.ParentInteractionID,
		ParentRunID:         event.ParentRunID,
		ParentToolCallID:    event.ParentToolCallID,
		RootInteractionID:   event.RootInteractionID,
		Delivery:            event.Delivery,
		ChildRunID:          event.ChildRunID,
		Model:               event.Model, TaskPreview: event.TaskPreview,
		Activity: event.Activity, Code: event.Code,
		Stop: event.Stop, Turns: event.Turns, ToolCalls: event.ToolCalls,
		Usage:          tokenUsageFromAI(event.Usage),
		DurationMillis: event.Duration.Milliseconds(),
	}
	if isTerminalSubagentState(event.State) && event.Delivery != subagent.DeliveryBackground {
		current.addSubagentUsage(event.ChildSessionID, payload.Model, payload.Usage)
	}
	// Nested lifecycle belongs to its actual parent child run, which is not
	// an active run in the root conversation reducer. Keep it out of the
	// root timeline while retaining the ordinary descendant child projection.
	if nested {
		return r.projectChildLifecycle(ctx, event)
	}

	interactionID := event.ParentInteractionID
	if interactionID == "" {
		interactionID = current.id
	}

	target := emitter
	if event.Delivery == subagent.DeliveryBackground {
		target = newEventEmitter(ctx, r, nil, false)
	}

	if err := target.emit(interactionID, event.ParentRunID, eventType, payload); err != nil {
		return err
	}

	if err := r.projectChildLifecycle(ctx, event); err != nil {
		return err
	}

	if isTerminalSubagentState(event.State) {
		return r.enqueueChildCompletion(event)
	}

	return nil
}

// projectChildLifecycle opens or closes the child's own session projection for
// one lifecycle event. It never touches the conversation's reducer.
func (r *Runtime) projectChildLifecycle(ctx context.Context, event subagent.Event) error {
	switch event.State {
	case subagent.StateCreated:
		return r.openChildProjection(ctx, event, r.inheritedChildHistory(ctx, event))
	case subagent.StateSucceeded, subagent.StateFailed,
		subagent.StateCanceled, subagent.StateInterrupted:
		return r.finishChildProjection(ctx, event)
	case subagent.StateRunning:
	}

	return nil
}

// enqueueChildCompletion delivers one background child's terminal result on the
// durable notification path. A wake reports its acceptance through the message
// envelope, so its terminal result is not surfaced as a second completion, and a
// foreground result is returned to its caller instead.
func (r *Runtime) enqueueChildCompletion(event subagent.Event) error {
	if event.Delivery != subagent.DeliveryBackground || event.Wake {
		return nil
	}

	if err := r.notifications.EnqueueCompletion(event); err != nil {
		return err
	}

	r.signalNotifications()

	return nil
}

// observeUnattributedChild commits the lifecycle of a child with no interaction:
// a scheduled fire's iteration. The child is projected into its own session, and
// a direct background child's terminal result is enqueued on the durable
// notification path — the whole of what a fire reports. A deeper descendant,
// which a depth-1 child cannot have, would stay in its own projection only.
func (r *Runtime) observeUnattributedChild(ctx context.Context, event subagent.Event) error {
	if err := r.projectChildLifecycle(ctx, event); err != nil {
		return err
	}

	// A deeper descendant is not this conversation's own child, so its terminal
	// result is not a report for this conversation.
	if event.ParentSessionID != "" && event.ParentSessionID != r.handle.Metadata().ID {
		return nil
	}

	if isTerminalSubagentState(event.State) {
		return r.enqueueChildCompletion(event)
	}

	return nil
}

// subagentEventIsNested reports whether one child lifecycle belongs to a run
// that is not this conversation's. A deeper descendant and a wake a child
// started both qualify, and neither has an active Tool call in the
// conversation's Run to attribute the child to, so the child is projected
// without entering the conversation timeline. The caller has already routed a
// scheduled fire's unattributed child to observeUnattributedChild, so current is
// never nil here.
func (r *Runtime) subagentEventIsNested(current *interaction, event subagent.Event) bool {
	if event.ParentSessionID != "" && event.ParentSessionID != r.handle.Metadata().ID {
		return true
	}

	return event.ParentInteractionID != "" && event.ParentInteractionID != current.id
}

// inheritedChildHistory reads the conversation a continued child already owns.
// Its session was forked from the child it continues, so the durable transcript
// carries those turns before this run produced anything. The read happens before
// the projection takes the publisher lock, and a child that inherited nothing —
// or whose session cannot be read — contributes nothing.
func (r *Runtime) inheritedChildHistory(ctx context.Context, event subagent.Event) []ai.Message {
	if !event.Inherited || r.subagents == nil || event.ChildSessionID == "" {
		return nil
	}

	detail, err := r.subagents.Inspect(ctx, event.ChildSessionID)
	if err != nil {
		return nil
	}

	return detail.Transcript
}

func (r *Runtime) openChildProjection(
	ctx context.Context,
	event subagent.Event,
	inherited []ai.Message,
) error {
	publisher := r.publisher
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if _, exists := r.children[event.ChildSessionID]; exists {
		return fmt.Errorf("%w: child projection already exists", ErrEventProtocol)
	}

	writer, err := newEventWriter(event.ChildSessionID, time.Now)
	if err != nil {
		return err
	}
	projector, err := newAgentProjector(writer, event.ChildSessionID)
	if err != nil {
		return err
	}
	child := &childProjection{
		writer: writer, projector: projector, interactionID: event.ChildSessionID,
	}
	// A continued child owns the conversation it inherited before this run
	// produces anything, and this live State is what a reader sees while the child
	// runs. The history is copied in rather than re-published as commits: those
	// messages were committed in the child this one continues, and a commit
	// outside an open turn is not a valid projection input.
	if len(inherited) > 0 {
		child.state.Transcript = cloneMessages(inherited)
	}
	r.children[event.ChildSessionID] = child
	eventTime := childEventTime(event.Time)
	provider, modelID, contextWindow := r.childSessionModel(event.Model)
	if err := publisher.publishChildGeneratedLocked(
		ctx,
		child,
		eventTime,
		"",
		EventSessionOpened,
		SessionOpened{
			Provider: provider, ModelID: modelID, ContextWindow: contextWindow,
			Mode: r.currentOperatingMode(),
		},
	); err != nil {
		return err
	}
	if err := publisher.publishChildGeneratedLocked(
		ctx, child, eventTime, "", EventInteractionStarted,
		InteractionStarted{Mode: r.currentOperatingMode()},
	); err != nil {
		return err
	}

	return publisher.publishChildGeneratedLocked(
		ctx, child, eventTime, "", EventStatusChanged, StatusChanged{Phase: PhaseRunning},
	)
}

func (r *Runtime) childSessionModel(modelRef string) (ai.Provider, string, int) {
	if r == nil || r.model == nil {
		return "", "", 0
	}
	provider, modelID, contextWindow := r.model.Provider(), r.model.ModelID(), r.resolved.Limits.ContextWindow
	ref, err := config.ParseModelRef(modelRef)
	if err != nil {
		return provider, modelID, contextWindow
	}
	provider, modelID = ref.Provider, ref.Model
	if r.modelCatalog == nil {
		return provider, modelID, contextWindow
	}
	resolved, resolveErr := r.modelCatalog.Resolve(modelcatalog.Selection{Ref: ref})
	if resolveErr == nil {
		contextWindow = resolved.Limits.ContextWindow
	}

	return provider, modelID, contextWindow
}

func (r *Runtime) observeChildAgentEvent(ctx context.Context, childEvent subagent.AgentEvent) error {
	publisher := r.publisher
	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	child, exists := r.children[childEvent.ChildSessionID]
	if !exists || child.finished {
		return fmt.Errorf("%w: child projection is unavailable", ErrEventProtocol)
	}
	if !child.taskPublished && len(child.pendingInput) == 0 && childEvent.Task != "" {
		child.pendingInput = []ai.Message{ai.UserText(childEvent.Task)}
	}

	projected, err := child.projector.project(childEvent.Event)
	if err != nil {
		return err
	}
	if err := publisher.publishChildLocked(ctx, child, projected); err != nil {
		return err
	}
	if _, turnStarted := childEvent.Event.Payload().(agent.TurnStarted); !turnStarted || len(child.pendingInput) == 0 {
		return nil
	}

	pending := child.pendingInput
	child.pendingInput = nil
	for _, message := range pending {
		if err := publisher.publishChildGeneratedLocked(
			ctx,
			child,
			childEvent.Event.Time,
			childEvent.Event.RunID,
			EventMessageCommitted,
			MessageCommitted{Message: message},
		); err != nil {
			return err
		}
		// The task is the child's opening user message, and this flush is the one
		// place it is published. Marking it here keeps every later turn boundary
		// from publishing it again.
		child.taskPublished = true
	}

	return nil
}

// projectChildPause records an independently owned child phase transition.
// The parent interaction remains running while the child waits for its own
// approval or structured question; callers must resolve that child through
// the targetable child-control APIs.
func (r *Runtime) projectChildPause(
	ctx context.Context,
	childSessionID string,
	paused bool,
) error {
	if r == nil || childSessionID == "" {
		return ErrRuntimeClosed
	}
	publisher := r.publisher
	if publisher == nil {
		return ErrRuntimeClosed
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	child, exists := r.children[childSessionID]
	if !exists || child.finished {
		return fmt.Errorf("%w: child projection is unavailable", ErrEventProtocol)
	}
	phase := PhaseRunning
	if paused {
		phase = PhasePaused
	}

	return publisher.publishChildGeneratedLocked(
		ctx,
		child,
		time.Now().UTC(),
		"",
		EventStatusChanged,
		StatusChanged{Phase: phase},
	)
}

// projectChildWorkspaceChanged records a change report exclusively in the
// child Session projection. The parent State and parent lifecycle event remain
// content-free with respect to child file diffs.
func (r *Runtime) projectChildWorkspaceChanged(
	ctx context.Context,
	childSessionID string,
	report WorkspaceChanged,
) error {
	if r == nil || childSessionID == "" {
		return ErrRuntimeClosed
	}
	publisher := r.publisher
	if publisher == nil {
		return ErrRuntimeClosed
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	child, exists := r.children[childSessionID]
	if !exists || child.finished {
		return fmt.Errorf("%w: child projection is unavailable", ErrEventProtocol)
	}

	return publisher.publishChildGeneratedLocked(
		ctx,
		child,
		time.Now().UTC(),
		"",
		EventWorkspaceChanged,
		report,
	)
}

func (r *Runtime) finishChildProjection(ctx context.Context, event subagent.Event) error {
	publisher := r.publisher
	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	child, exists := r.children[event.ChildSessionID]
	if !exists || child.finished {
		return nil
	}
	eventTime := childEventTime(event.Time)
	outcome := childInteractionOutcome(event.State)
	if outcome != InteractionSucceeded || len(child.state.activeRuns) > 0 ||
		len(child.state.activeTools) > 0 {
		code := event.Code
		if !validCode(code) {
			code = "subagent_failed"
		}
		if err := publisher.publishChildGeneratedLocked(
			ctx,
			child,
			eventTime,
			event.ChildRunID,
			EventError,
			RuntimeError{Code: code, Message: "Subagent execution failed", Fatal: true},
		); err != nil {
			return err
		}
	}
	if err := publisher.publishChildGeneratedLocked(
		ctx,
		child,
		eventTime,
		"",
		EventInteractionCompleted,
		InteractionCompleted{
			Outcome: outcome, Stop: event.Stop, Usage: tokenUsageFromAI(event.Usage),
			DurationMillis: event.Duration.Milliseconds(),
		},
	); err != nil {
		return err
	}
	if err := publisher.publishChildGeneratedLocked(
		ctx, child, eventTime, "", EventStatusChanged, StatusChanged{Phase: PhaseIdle},
	); err != nil {
		return err
	}
	child.finished = true

	return nil
}

func childEventTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}

	return value.UTC()
}

func childInteractionOutcome(state subagent.State) InteractionOutcome {
	switch state {
	case subagent.StateSucceeded:
		return InteractionSucceeded
	case subagent.StateCanceled, subagent.StateInterrupted:
		return InteractionCanceled
	default:
		return InteractionFailed
	}
}

func subagentEventType(event subagent.Event) EventType {
	switch event.State {
	case subagent.StateCreated:
		return EventSubagentCreated
	case subagent.StateRunning:
		if event.Progress {
			return EventSubagentProgress
		}

		return EventSubagentStarted
	case subagent.StateSucceeded:
		return EventSubagentCompleted
	case subagent.StateCanceled:
		return EventSubagentCanceled
	case subagent.StateInterrupted:
		return EventSubagentInterrupted
	default:
		return EventSubagentFailed
	}
}

// ListSubagents returns durable children owned by the current parent Session.
func (r *Runtime) ListSubagents(ctx context.Context) ([]subagent.Summary, error) {
	if r == nil || r.subagents == nil {
		return nil, ErrRuntimeClosed
	}

	return r.subagents.List(ctx)
}

// InspectSubagent loads one durable child owned by the current parent Session.
func (r *Runtime) InspectSubagent(
	ctx context.Context,
	childSessionID string,
) (subagent.Detail, error) {
	if r == nil || r.subagents == nil {
		return subagent.Detail{}, ErrRuntimeClosed
	}

	return r.subagents.Inspect(ctx, childSessionID)
}

// InspectSubagentState returns the ordinary Coding State used to render a
// child Session. Live children retain their complete event projection;
// terminal children reopened from disk fall back to their durable transcript.
func (r *Runtime) InspectSubagentState(
	ctx context.Context,
	childSessionID string,
) (State, error) {
	if r == nil || r.subagents == nil || r.publisher == nil {
		return State{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}

	r.publisher.mu.Lock()
	if child, ok := r.children[childSessionID]; ok {
		state := child.state.Clone()
		r.publisher.mu.Unlock()

		return state, nil
	}
	r.publisher.mu.Unlock()

	detail, err := r.subagents.Inspect(ctx, childSessionID)
	if err != nil {
		return State{}, err
	}

	state := State{
		SessionID: childSessionID, SessionOpen: true,
		Provider: r.model.Provider(), ModelID: r.model.ModelID(),
		Mode: r.currentOperatingMode(), Phase: PhaseIdle,
		Transcript: cloneMessages(detail.Transcript),
		Interaction: InteractionState{
			ID: childSessionID, Outcome: childInteractionOutcome(detail.Summary.State),
			Usage: tokenUsageFromAI(detail.Summary.Usage),
		},
	}
	state.rebuildIndexes()

	return state.Clone(), nil
}

// WaitSubagent waits for one owned child to become terminal without affecting
// any other child or the parent interaction.
func (r *Runtime) WaitSubagent(
	ctx context.Context,
	childSessionID string,
) (subagent.Result, error) {
	if r == nil || r.subagents == nil {
		return subagent.Result{}, ErrRuntimeClosed
	}

	return r.subagents.Wait(ctx, childSessionID)
}

// CancelSubagent requests cancellation of one owned child.
func (r *Runtime) CancelSubagent(ctx context.Context, childSessionID string) error {
	if r == nil || r.subagents == nil {
		return ErrRuntimeClosed
	}

	return r.subagents.Cancel(ctx, childSessionID)
}

// SubagentControlState returns the independently owned approval/question
// state for a currently live custom child. Terminal and legacy children have
// no in-memory control target and therefore cannot be resumed through this
// path.
func (r *Runtime) SubagentControlState(childSessionID string) (ChildControlState, error) {
	scope, err := r.childControlScope(childSessionID)
	if err != nil {
		return ChildControlState{}, err
	}

	return scope.State(), nil
}

// ResolveSubagentApproval routes a decision to exactly one child-owned
// approval journal/resolver. It never resolves the parent interaction.
func (r *Runtime) ResolveSubagentApproval(
	ctx context.Context,
	childSessionID string,
	resolution approval.Resolution,
) (ChildControlState, error) {
	scope, err := r.childControlScope(childSessionID)
	if err != nil {
		return ChildControlState{}, err
	}

	return scope.ResolveApproval(ctx, resolution)
}

// ResolveSubagentQuestion routes a structured answer to exactly one
// child-owned question controller and resumes only that child when ready.
func (r *Runtime) ResolveSubagentQuestion(
	ctx context.Context,
	childSessionID string,
	resolution question.Resolution,
) (ChildControlState, error) {
	scope, err := r.childControlScope(childSessionID)
	if err != nil {
		return ChildControlState{}, err
	}

	return scope.ResolveQuestion(ctx, resolution)
}

// RejectSubagentQuestion records an explicit cancellation on exactly one
// child-owned question controller.
func (r *Runtime) RejectSubagentQuestion(
	ctx context.Context,
	childSessionID string,
	requestID string,
	schemaDigest string,
) (ChildControlState, error) {
	scope, err := r.childControlScope(childSessionID)
	if err != nil {
		return ChildControlState{}, err
	}

	return scope.RejectQuestion(ctx, requestID, schemaDigest)
}

func (r *Runtime) childControlScope(childSessionID string) (*childControlScope, error) {
	if r == nil || childSessionID == "" {
		return nil, ErrRuntimeClosed
	}
	r.mu.Lock()
	closed := r.closed || r.closing
	controls := r.childControls
	r.mu.Unlock()
	if closed || controls == nil {
		return nil, ErrRuntimeClosed
	}
	scope := controls.get(childSessionID)
	if scope == nil {
		return nil, fmt.Errorf("%w: child control target is unavailable", ErrRuntimeNotPaused)
	}

	return scope, nil
}
