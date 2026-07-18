package agent

import "github.com/rsbin/pips/ai"

// EventType discriminates [Event] variants.
type EventType string

// Event types, in the order a run produces them: one run_start; per turn a
// turn_start, deltas (streaming only), the assistant message, tool lifecycle
// pairs, the tool-result message, and a turn_end; one run_end on clean
// termination (failed runs surface an error instead).
const (
	// EventRunStart opens the run.
	EventRunStart EventType = "run_start"
	// EventTurnStart opens a turn; Turn identifies it.
	EventTurnStart EventType = "turn_start"
	// EventDelta carries one model streaming increment in Delta. It is only
	// produced by [Agent.Stream], never [Agent.Run].
	EventDelta EventType = "delta"
	// EventMessage reports a completed message appended to the session
	// (the assistant turn, then the tool-result message) in Message.
	EventMessage EventType = "message"
	// EventToolStart announces a tool call about to be gated and executed; it
	// carries Call.
	EventToolStart EventType = "tool_start"
	// EventToolUpdate carries a partial-result update published by a running
	// tool via [ReportProgress]; it carries Call and Update. Delivery is
	// best-effort.
	EventToolUpdate EventType = "tool_update"
	// EventToolEnd reports a finished tool call; it carries Call and Result
	// (denials and synthesized failures included).
	EventToolEnd EventType = "tool_end"
	// EventTurnEnd closes a turn; it carries the run's cumulative Usage.
	EventTurnEnd EventType = "turn_end"
	// EventRunEnd closes the run; it carries Stop and the run's cumulative
	// Usage.
	EventRunEnd EventType = "run_end"
)

// Event is one normalized increment of an agent run. Only the fields
// documented for the event's Type are meaningful. Pointer fields reference
// copies made at emission time, so events are safe to retain; treat their
// contents as read-only.
type Event struct {
	Type EventType

	// Turn is the 1-based turn number, 0 on run_start.
	Turn int

	// Delta is the model streaming event for delta events.
	Delta ai.StreamEvent

	// Message is the appended message for message events.
	Message *ai.Message

	// Call is set on tool_start, tool_update, and tool_end; Result on
	// tool_end; Update on tool_update.
	Call   *ai.ToolCallPart
	Result *ai.ToolResultPart
	Update []ai.Part

	// Stop is set on run_end.
	Stop StopReason
	// Usage is the run's cumulative usage, set on turn_end and run_end.
	Usage ai.Usage
}

// emitFunc delivers one event and reports whether the run should keep going;
// false means the stream consumer stopped iterating.
type emitFunc func(Event) bool

// Event constructors copy their payloads so consumers can retain events
// safely.

func messageEvent(turn int, msg ai.Message) Event {
	m := msg
	return Event{Type: EventMessage, Turn: turn, Message: &m}
}

func toolStartEvent(turn int, call ai.ToolCallPart) Event {
	c := call
	return Event{Type: EventToolStart, Turn: turn, Call: &c}
}

func toolEndEvent(turn int, call ai.ToolCallPart, result ai.ToolResultPart) Event {
	c, r := call, result

	return Event{Type: EventToolEnd, Turn: turn, Call: &c, Result: &r}
}

func toolUpdateEvent(turn int, call ai.ToolCallPart, update []ai.Part) Event {
	c := call
	return Event{Type: EventToolUpdate, Turn: turn, Call: &c, Update: update}
}
