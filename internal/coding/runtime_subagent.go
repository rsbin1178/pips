package coding

import (
	"context"

	"github.com/rsbin/pips/internal/coding/subagent"
)

func (r *Runtime) subagentObserver(
	current *interaction,
	emitter *eventEmitter,
) subagent.Observer {
	return func(_ context.Context, event subagent.Event) error {
		eventType := subagentEventType(event)

		payload := SubagentLifecycle{
			Role: event.Role, State: event.State,
			ChildSessionID: event.ChildSessionID,
			ParentRunID:    event.ParentRunID, ChildRunID: event.ChildRunID,
			Model: event.Model, TaskPreview: event.TaskPreview,
			Activity: event.Activity, Code: event.Code,
			Stop: event.Stop, Turns: event.Turns, ToolCalls: event.ToolCalls,
			Usage:          tokenUsageFromAI(event.Usage),
			DurationMillis: event.Duration.Milliseconds(),
		}
		if isTerminalSubagentState(event.State) {
			current.addSubagentUsage(event.ChildSessionID, payload.Usage)
		}

		return emitter.emit(current.id, event.ParentRunID, eventType, payload)
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
