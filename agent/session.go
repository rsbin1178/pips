package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/rsbin/pips/ai"
)

// ToolResolution supplies an out-of-band result for one pending tool call.
// Content is returned to the model; IsError marks a rejection or failed
// approval as a tool error rather than a run error.
type ToolResolution struct {
	ToolCallID string
	Content    []ai.Part
	IsError    bool
}

// Session holds the conversation state an agent runs against: the message
// history and the token usage accumulated across runs. It is safe for
// concurrent use, but only one run may be active at a time ([ErrRunActive]).
//
// Sessions serialize to a stable JSON envelope (reusing the [ai.Message]
// format), so they can be persisted and resumed across processes. The zero
// value is not usable; construct with [NewSession].
type Session struct {
	mu       sync.Mutex
	messages ai.Messages
	usage    ai.Usage
	running  bool

	// steering and followUps queue messages for injection into a running
	// loop (see Steer and FollowUp). They are runtime state and are not
	// serialized.
	steering  ai.Messages
	followUps ai.Messages
}

// NewSession returns a session seeded with the given messages (for example a
// restored history), oldest first.
func NewSession(msgs ...ai.Message) *Session {
	return &Session{messages: slices.Clone(msgs)}
}

// Messages returns a copy of the conversation so far, oldest first. The
// message structs are copies; treat their Parts as read-only.
func (s *Session) Messages() ai.Messages {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.messages)
}

// Append adds messages to the history. Runs snapshot the history at each
// turn, so appending during an active run affects the next model call, not
// the in-flight one.
func (s *Session) Append(msgs ...ai.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, msgs...)
}

// Usage returns the token usage accumulated across all runs on this session.
func (s *Session) Usage() ai.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.usage
}

// Replace swaps the entire message history for msgs. It is the commit point
// for context compaction: rewrite the history out-of-band (or from a
// [WithPrepareTurn] hook) and the next model call sees the new form. Usage
// accounting and queued messages are unaffected.
func (s *Session) Replace(msgs ...ai.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = slices.Clone(msgs)
}

// Steer queues messages for injection into the running loop: they are
// appended at the start of the next turn, before the next model call, letting
// a user redirect the agent mid-run. Queued messages survive until a run
// drains them (see [WithSteeringMode]); pending steering also keeps the loop
// going when the model would otherwise finish. Safe to call from any
// goroutine, with or without an active run.
func (s *Session) Steer(msgs ...ai.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.steering = append(s.steering, msgs...)
}

// FollowUp queues messages that run only after the agent would otherwise
// stop cleanly: when a turn produces no tool calls and no steering is queued,
// follow-ups are injected and the loop continues instead of finishing. Safe
// to call from any goroutine.
func (s *Session) FollowUp(msgs ...ai.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.followUps = append(s.followUps, msgs...)
}

// ClearSteering discards all queued steering messages.
func (s *Session) ClearSteering() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.steering = nil
}

// ClearFollowUps discards all queued follow-up messages.
func (s *Session) ClearFollowUps() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.followUps = nil
}

// HasQueued reports whether any steering or follow-up messages are queued.
func (s *Session) HasQueued() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.steering) > 0 || len(s.followUps) > 0
}

// drainSteering removes and returns queued steering messages according to
// mode: the oldest one ([QueueDrainOne]) or all of them ([QueueDrainAll]).
func (s *Session) drainSteering(mode QueueMode) ai.Messages {
	s.mu.Lock()
	defer s.mu.Unlock()

	var drained ai.Messages

	drained, s.steering = drainQueue(s.steering, mode)

	return drained
}

// drainFollowUps removes and returns queued follow-up messages according to
// mode.
func (s *Session) drainFollowUps(mode QueueMode) ai.Messages {
	s.mu.Lock()
	defer s.mu.Unlock()

	var drained ai.Messages

	drained, s.followUps = drainQueue(s.followUps, mode)

	return drained
}

// drainQueue splits a queue according to mode. The drained slice is
// capacity-capped so later appends to either side cannot alias.
func drainQueue(queue ai.Messages, mode QueueMode) (drained, rest ai.Messages) {
	switch {
	case len(queue) == 0:
		return nil, queue
	case mode == QueueDrainAll:
		return queue, nil
	default:
		return queue[:1:1], queue[1:]
	}
}

// Pending returns the tool calls at the session tail that have no matching
// results, in call order. A non-empty result means the conversation cannot
// continue until they are resolved (see [Session.ResolvePending]).
func (s *Session) Pending() []ai.ToolCallPart {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pendingLocked()
}

func (s *Session) pendingLocked() []ai.ToolCallPart {
	answered := make(map[string]bool)

	// Walk trailing tool messages to collect answered call IDs, then check
	// the assistant message they respond to.
	for _, msg := range slices.Backward(s.messages) {
		switch msg := msg.(type) {
		case ai.ToolMessage:
			for _, result := range msg.Parts {
				answered[result.ToolCallID] = true
			}

			continue
		case ai.AssistantMessage:
			var pending []ai.ToolCallPart

			for _, part := range msg.Parts {
				if call, ok := part.(ai.ToolCallPart); ok && !answered[call.ID] {
					pending = append(pending, call)
				}
			}

			return pending
		default:
			return nil
		}
	}

	return nil
}

// ResolveToolCalls answers any subset of the session's pending tool calls.
// Resolutions are validated atomically and appended in the original call
// order, regardless of argument order. Calls omitted from resolutions remain
// pending and survive session serialization. It fails with [ErrRunActive]
// during a run, [ErrInvalidToolResolution] for empty/duplicate IDs, or
// [ErrToolCallNotPending] for a stale/unknown ID. An empty resolution list is
// a no-op.
func (s *Session) ResolveToolCalls(resolutions ...ToolResolution) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return ErrRunActive
	}

	if len(resolutions) == 0 {
		return nil
	}

	pending := s.pendingLocked()
	byID := make(map[string]ToolResolution, len(resolutions))

	for _, resolution := range resolutions {
		if resolution.ToolCallID == "" {
			return fmt.Errorf("%w: empty tool-call ID", ErrInvalidToolResolution)
		}

		if _, duplicate := byID[resolution.ToolCallID]; duplicate {
			return fmt.Errorf(
				"%w: duplicate tool-call ID %q",
				ErrInvalidToolResolution,
				resolution.ToolCallID,
			)
		}

		byID[resolution.ToolCallID] = resolution
	}

	pendingByID := make(map[string]ai.ToolCallPart, len(pending))
	for _, call := range pending {
		pendingByID[call.ID] = call
	}

	for id := range byID {
		if _, ok := pendingByID[id]; !ok {
			return fmt.Errorf("%w: %s", ErrToolCallNotPending, id)
		}
	}

	parts := make([]ai.ToolResultPart, 0, len(resolutions))

	for _, call := range pending {
		resolution, ok := byID[call.ID]
		if !ok {
			continue
		}

		parts = append(parts, ai.ToolResultPart{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    resolution.Content,
			IsError:    resolution.IsError,
		})
	}

	s.messages = append(s.messages, ai.ToolMessage{Parts: parts})

	return nil
}

// ResolvePending answers all of the session's pending tool calls: fn is invoked for
// each call in order, and the outcomes are appended as a single tool-result
// message. An fn error becomes an error tool result carrying the error text
// (return an error to reject a call), so the model learns the outcome either
// way. It is a no-op when nothing is pending and fails with [ErrRunActive]
// during an active run.
func (s *Session) ResolvePending(ctx context.Context, fn func(ctx context.Context, call ai.ToolCallPart) ([]ai.Part, error)) error {
	s.mu.Lock()

	if s.running {
		s.mu.Unlock()
		return ErrRunActive
	}

	pending := s.pendingLocked()
	s.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	resolutions := make([]ToolResolution, 0, len(pending))

	for _, call := range pending {
		content, err := fn(ctx, call)
		if err != nil {
			resolutions = append(resolutions, ToolResolution{
				ToolCallID: call.ID,
				Content:    TextResult(err.Error()),
				IsError:    true,
			})

			continue
		}

		resolutions = append(resolutions, ToolResolution{ToolCallID: call.ID, Content: content})
	}

	return s.ResolveToolCalls(resolutions...)
}

// begin marks the session as running after checking preconditions.
func (s *Session) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return ErrRunActive
	}

	if len(s.pendingLocked()) > 0 {
		return ErrPendingToolCalls
	}

	s.running = true

	return nil
}

// end clears the running flag.
func (s *Session) end() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.running = false
}

// addUsage folds one response's usage into the cross-run total.
func (s *Session) addUsage(u ai.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.usage.Add(u)
}

type sessionJSON struct {
	Messages ai.Messages `json:"messages"`
	Usage    ai.Usage    `json:"usage"`
}

// MarshalJSON implements [json.Marshaler]. The running flag is transient and
// not serialized.
func (s *Session) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return json.Marshal(sessionJSON{Messages: s.messages, Usage: s.usage})
}

// UnmarshalJSON implements [json.Unmarshaler], replacing the session's state.
func (s *Session) UnmarshalJSON(data []byte) error {
	var raw sessionJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = raw.Messages
	s.usage = raw.Usage

	return nil
}
