//nolint:wsl_v5 // Parent lifecycle and child projection commits stay adjacent.
package coding

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/subagent"
)

type childProjection struct {
	state         State
	writer        *eventWriter
	projector     *agentProjector
	interactionID string
	pendingInput  []ai.Message
	finished      bool
}

func (r *Runtime) subagentObserver(
	current *interaction,
	emitter *eventEmitter,
) subagent.Observer {
	return func(ctx context.Context, event subagent.Event) error {
		eventType := subagentEventType(event)

		payload := SubagentLifecycle{
			Role: event.Role, State: event.State,
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
			current.addSubagentUsage(event.ChildSessionID, payload.Usage)
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

		var projectionErr error
		switch event.State {
		case subagent.StateCreated:
			projectionErr = r.openChildProjection(ctx, event)
		case subagent.StateSucceeded, subagent.StateFailed,
			subagent.StateCanceled, subagent.StateInterrupted:
			projectionErr = r.finishChildProjection(ctx, event)
		case subagent.StateRunning:
		}
		if projectionErr != nil {
			return projectionErr
		}
		if event.Delivery == subagent.DeliveryBackground &&
			isTerminalSubagentState(event.State) {
			if err := r.notifications.EnqueueCompletion(event); err != nil {
				return err
			}
			r.signalNotifications()
		}

		return nil
	}
}

func (r *Runtime) openChildProjection(ctx context.Context, event subagent.Event) error {
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
	r.children[event.ChildSessionID] = child
	eventTime := childEventTime(event.Time)
	if err := publisher.publishChildGeneratedLocked(
		ctx,
		child,
		eventTime,
		"",
		EventSessionOpened,
		SessionOpened{Provider: r.model.Provider(), ModelID: r.model.ModelID()},
	); err != nil {
		return err
	}
	if err := publisher.publishChildGeneratedLocked(
		ctx, child, eventTime, "", EventInteractionStarted, InteractionStarted{},
	); err != nil {
		return err
	}

	return publisher.publishChildGeneratedLocked(
		ctx, child, eventTime, "", EventStatusChanged, StatusChanged{Phase: PhaseRunning},
	)
}

func (r *Runtime) observeChildAgentEvent(ctx context.Context, childEvent subagent.AgentEvent) error {
	publisher := r.publisher
	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	child, exists := r.children[childEvent.ChildSessionID]
	if !exists || child.finished {
		return fmt.Errorf("%w: child projection is unavailable", ErrEventProtocol)
	}
	if len(child.pendingInput) == 0 && childEvent.Task != "" {
		child.pendingInput = []ai.Message{ai.UserText(childEvent.Task)}
	}

	projected, err := child.projector.project(childEvent.Event)
	if err != nil {
		return err
	}
	if err := publisher.publishChildLocked(ctx, child, projected); err != nil {
		return err
	}
	if childEvent.Event.Type != agent.EventTurnStart || len(child.pendingInput) == 0 {
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
	}

	return nil
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
			Outcome: outcome, Usage: tokenUsageFromAI(event.Usage),
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
		Provider: r.model.Provider(), ModelID: r.model.ModelID(), Phase: PhaseIdle,
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
