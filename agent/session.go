package agent

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/rsbin/pips/ai"
)

// Session holds the conversation state an agent runs against: the message
// history and the token usage accumulated across runs. It is safe for
// concurrent use, but only one run may be active at a time ([ErrRunActive]).
//
// Sessions serialize to a stable JSON envelope (reusing the [ai.Message]
// format), so they can be persisted and resumed across processes. The zero
// value is not usable; construct with [NewSession].
type Session struct {
	mu       sync.Mutex
	messages []ai.Message
	usage    ai.Usage
	running  bool
}

// NewSession returns a session seeded with the given messages (for example a
// restored history), oldest first.
func NewSession(msgs ...ai.Message) *Session {
	return &Session{messages: slices.Clone(msgs)}
}

// Messages returns a copy of the conversation so far, oldest first. The
// message structs are copies; treat their Parts as read-only.
func (s *Session) Messages() []ai.Message {
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
		if msg.Role == ai.RoleTool {
			for _, p := range msg.Parts {
				if r, ok := p.(ai.ToolResultPart); ok {
					answered[r.ToolCallID] = true
				}
			}

			continue
		}

		if msg.Role != ai.RoleAssistant {
			return nil
		}

		var pending []ai.ToolCallPart

		for _, p := range msg.Parts {
			if c, ok := p.(ai.ToolCallPart); ok && !answered[c.ID] {
				pending = append(pending, c)
			}
		}

		return pending
	}

	return nil
}

// ResolvePending answers the session's pending tool calls: fn is invoked for
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

	parts := make([]ai.Part, 0, len(pending))

	for _, call := range pending {
		result := ai.ToolResultPart{ToolCallID: call.ID, Name: call.Name}

		content, err := fn(ctx, call)
		if err != nil {
			result.Content = TextResult(err.Error())
			result.IsError = true
		} else {
			result.Content = content
		}

		parts = append(parts, result)
	}

	s.Append(ai.Message{Role: ai.RoleTool, Parts: parts})

	return nil
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
	Messages []ai.Message `json:"messages"`
	Usage    ai.Usage     `json:"usage"`
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
