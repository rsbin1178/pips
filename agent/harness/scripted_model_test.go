package harness_test

import (
	"context"
	"strconv"
	"sync"

	"github.com/rsbin/pips/ai"
)

// scriptedModel plays deterministic responses through Generate; the harness never
// streams.
type scriptedModel struct {
	mu       sync.Mutex
	id       string
	script   []*ai.Response
	pos      int
	requests []ai.Request
}

func newScriptedModel(id string, script ...*ai.Response) *scriptedModel {
	return &scriptedModel{id: id, script: script}
}

func textResponse(text string, inTokens int) *ai.Response {
	return &ai.Response{
		Message:      ai.AssistantText(text),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: inTokens, OutputTokens: 5},
	}
}

func callResponse(id, name, args string) *ai.Response {
	return &ai.Response{
		Message:      ai.Assistant(ai.ToolCallPart{ID: id, Name: name, Args: ai.JSON(args)}),
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func (m *scriptedModel) Generate(_ context.Context, req ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, req)

	if m.pos >= len(m.script) {
		return textResponse("script exhausted "+strconv.Itoa(m.pos), 10), nil
	}

	resp := m.script[m.pos]
	m.pos++

	return resp, nil
}

func (m *scriptedModel) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		resp, err := m.Generate(ctx, req)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		usage := resp.Usage
		for _, part := range resp.Message.Parts {
			if t, ok := part.(ai.TextPart); ok {
				if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: t.Text}, nil) {
					return
				}
			}
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: resp.FinishReason, Usage: &usage}, nil)
	}
}

func (m *scriptedModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

func (m *scriptedModel) Provider() ai.Provider { return ai.Provider("scripted") }
func (m *scriptedModel) ModelID() string       { return m.id }
func (m *scriptedModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}
