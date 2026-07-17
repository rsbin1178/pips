package openai

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
)

const responsesPath = "responses"

func (m *Model) generateResponses(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, err := m.responsesRequestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed responsesResponse

	raw, err := m.client.PostJSON(ctx, responsesPath, m.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("openai: responses: %w", err)
	}

	return responseFromResponses(parsed, raw), nil
}

func (m *Model) streamResponses(ctx context.Context, req ai.Request) ai.Stream {
	body, err := m.responsesRequestFrom(req, true)
	return m.runStream(ctx, responsesPath, "responses", body, err, emitResponsesStream)
}

// emitResponsesStream translates the Responses semantic-event dialect. Each
// SSE event's payload has a "type" field; the relevant ones are response
// lifecycle, output_text/reasoning_summary deltas, and function-call item
// add/args-delta.
func emitResponsesStream(events eventSource, yield func(ai.StreamEvent, error) bool) {
	d := &responsesStreamState{toolSlot: make(map[int]int)}

	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("openai: responses stream: %w", err))
			return
		}

		if event.Data == "" || event.Data == doneSentinel {
			continue
		}

		var ev responsesStreamEvent
		if err := jsonx.Unmarshal([]byte(event.Data), &ev); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("openai: decoding stream event: %w", err))
			return
		}

		if !d.handle(ev, yield) {
			return
		}
	}
}

type responsesStreamState struct {
	startSent bool
	// toolSlot maps a function_call output_index to its ai tool-call index.
	toolSlot map[int]int
	nextTool int
}

func (d *responsesStreamState) handle(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	switch ev.Type {
	case "response.created":
		return d.handleCreated(ev, yield)
	case "response.output_text.delta":
		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: ev.Delta}, nil)
	case "response.reasoning_summary_text.delta":
		return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: ev.Delta}, nil)
	case "response.output_item.added":
		return d.handleItemAdded(ev, yield)
	case "response.function_call_arguments.delta":
		return d.handleArgsDelta(ev, yield)
	case "response.function_call_arguments.done":
		return d.handleArgsDone(ev, yield)
	case "response.completed", "response.incomplete", "response.failed":
		return d.handleTerminal(ev, yield)
	case "error":
		yield(ai.StreamEvent{}, &ai.Error{Provider: ai.ProviderOpenAI, Message: ev.Message})
		return false
	default:
		return true
	}
}

func (d *responsesStreamState) handleCreated(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if d.startSent {
		return true
	}

	d.startSent = true
	start := ai.StreamEvent{Type: ai.StreamMessageStart}

	if ev.Response != nil {
		start.ID = ev.Response.ID
		start.Model = ev.Response.Model
	}

	return yield(start, nil)
}

func (d *responsesStreamState) handleItemAdded(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Item == nil || ev.Item.Type != typeFunctionCall {
		return true
	}

	idx := d.nextTool
	d.nextTool++
	d.toolSlot[ev.OutputIndex] = idx

	return yield(ai.StreamEvent{
		Type:          ai.StreamToolCallStart,
		ToolCallIndex: idx,
		ToolCallID:    ev.Item.CallID,
		ToolCallName:  ev.Item.Name,
	}, nil)
}

func (d *responsesStreamState) handleArgsDelta(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	idx, ok := d.toolSlot[ev.OutputIndex]
	if !ok {
		return true
	}

	return yield(ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: idx, ArgsDelta: ev.Delta}, nil)
}

func (d *responsesStreamState) handleArgsDone(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	idx, ok := d.toolSlot[ev.OutputIndex]
	if !ok {
		return true
	}

	return yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: idx}, nil)
}

func (d *responsesStreamState) handleTerminal(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	end := ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}

	if ev.Response != nil {
		end.FinishReason = finishReasonFromResponses(*ev.Response)

		usage := usageFromResponses(ev.Response.Usage)
		end.Usage = &usage
	}

	return yield(end, nil)
}
