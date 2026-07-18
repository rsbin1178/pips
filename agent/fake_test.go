package agent_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/rsbin/pips/ai"
)

// fakeModel plays a scripted sequence of turns. Generate consumes one turn
// per call; Stream replays the same turn as streaming events, so both agent
// paths run against identical scripts.
type fakeModel struct {
	mu       sync.Mutex
	script   []fakeTurn
	pos      int
	requests []ai.Request
	// abandoned reports that a stream consumer stopped before the script
	// turn finished playing.
	abandoned atomic.Bool
}

type fakeTurn struct {
	resp *ai.Response
	err  error
}

var errScriptExhausted = errors.New("fake: script exhausted")

func newFakeModel(turns ...fakeTurn) *fakeModel {
	return &fakeModel{script: turns}
}

// reply scripts a successful model response.
func reply(resp *ai.Response) fakeTurn {
	return fakeTurn{resp: resp}
}

// fail scripts a model error.
func fail(err error) fakeTurn {
	return fakeTurn{err: err}
}

// textResponse builds a plain text assistant response.
func textResponse(text string) *ai.Response {
	return &ai.Response{
		Message:      ai.AssistantText(text),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// callResponse builds an assistant response requesting the given tool calls.
func callResponse(calls ...ai.ToolCallPart) *ai.Response {
	parts := make([]ai.Part, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c)
	}

	return &ai.Response{
		Message:      ai.Message{Role: ai.RoleAssistant, Parts: parts},
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func call(id, name, args string) ai.ToolCallPart {
	return ai.ToolCallPart{ID: id, Name: name, Args: ai.JSON(args)}
}

func (m *fakeModel) take(req ai.Request) fakeTurn {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, req)

	if m.pos >= len(m.script) {
		return fakeTurn{err: errScriptExhausted}
	}

	turn := m.script[m.pos]
	m.pos++

	return turn
}

// Requests returns the requests observed so far.
func (m *fakeModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

func (m *fakeModel) Generate(_ context.Context, req ai.Request) (*ai.Response, error) {
	turn := m.take(req)
	if turn.err != nil {
		return nil, turn.err
	}

	return turn.resp, nil
}

func (m *fakeModel) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		turn := m.take(req)
		if turn.err != nil {
			yield(ai.StreamEvent{}, turn.err)
			return
		}

		for _, ev := range streamEvents(turn.resp) {
			if ctx.Err() != nil {
				yield(ai.StreamEvent{}, ctx.Err())
				return
			}

			if !yield(ev, nil) {
				m.abandoned.Store(true)
				return
			}
		}
	}
}

func (m *fakeModel) Provider() ai.Provider         { return ai.Provider("fake") }
func (m *fakeModel) ModelID() string               { return "fake-1" }
func (m *fakeModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true, Tools: true} }

// streamEvents decomposes a response into the event sequence a provider
// stream would produce; ai.Collect folds it back into an equal response.
func streamEvents(resp *ai.Response) []ai.StreamEvent {
	events := []ai.StreamEvent{{Type: ai.StreamMessageStart, ID: resp.ID, Model: resp.Model}}
	toolIndex := 0

	for _, part := range resp.Message.Parts {
		switch part := part.(type) {
		case ai.TextPart:
			events = append(events, ai.StreamEvent{Type: ai.StreamTextDelta, Text: part.Text})
		case ai.ToolCallPart:
			events = append(events,
				ai.StreamEvent{
					Type:          ai.StreamToolCallStart,
					ToolCallIndex: toolIndex,
					ToolCallID:    part.ID,
					ToolCallName:  part.Name,
				},
				ai.StreamEvent{
					Type:          ai.StreamToolCallDelta,
					ToolCallIndex: toolIndex,
					ArgsDelta:     string(part.Args),
				},
				ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: toolIndex},
			)

			toolIndex++
		}
	}

	usage := resp.Usage

	return append(events, ai.StreamEvent{
		Type:         ai.StreamMessageEnd,
		FinishReason: resp.FinishReason,
		Usage:        &usage,
	})
}
