package openai

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

const responsesPath = "responses"

func (m *Model) generateResponses(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, err := m.responsesRequestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed responsesResponse

	raw, err := m.client.PostJSON(ctx, responsesPath, m.authHeaders(), body, &parsed, m.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: responses: %w", m.label(), err)
	}

	if parsed.Status == "failed" && parsed.Error != nil {
		return nil, responsesFailure(m.provider, parsed.Error, raw)
	}

	return responseFromResponses(parsed, raw, m.provider), nil
}

// responsesFailure builds an error for a response whose status is "failed",
// so the failure code and message are not lost in Raw.
func responsesFailure(provider ai.Provider, e *responsesError, raw []byte) error {
	apiErr := &ai.Error{
		Provider: provider,
		Code:     e.Code,
		Message:  e.Message,
		Raw:      raw,
	}

	switch e.Code {
	case "rate_limit_exceeded":
		return apiErr.WithSentinel(ai.ErrRateLimited)
	case "server_error":
		return apiErr.WithSentinel(ai.ErrOverloaded)
	default:
		return apiErr
	}
}

func (m *Model) streamResponses(ctx context.Context, req ai.Request) ai.Stream {
	body, err := m.responsesRequestFrom(req, true)
	return m.runStream(ctx, responsesPath, "responses", body, err, emitResponsesStream)
}

// emitResponsesStream translates the Responses semantic-event dialect. Each
// SSE event's payload has a "type" field; the relevant ones are response
// lifecycle, output_text/reasoning deltas, and function-call item
// add/args-delta.
func emitResponsesStream(provider ai.Provider, events eventSource, yield func(ai.StreamEvent, error) bool) {
	d := &responsesStreamState{
		provider:        provider,
		toolSlot:        make(map[int]int),
		reasoningFlavor: make(map[int]string),
	}

	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: responses stream: %w", provider, err))
			return
		}

		if event.Data == "" || event.Data == doneSentinel {
			continue
		}

		var ev responsesStreamEvent
		if err := jsonx.Unmarshal([]byte(event.Data), &ev); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding stream event: %w", provider, err))
			return
		}

		if !d.handle(ev, yield) {
			return
		}
	}
}

type responsesStreamState struct {
	provider  ai.Provider
	startSent bool
	// toolSlot maps a function_call output_index to its ai tool-call index.
	toolSlot map[int]int
	nextTool int
	// reasoningFlavor records, per output_index, which reasoning text stream
	// the response is using. See handleReasoningDelta.
	reasoningFlavor map[int]string
	nextCitation    int
}

// Reasoning text flavors. A response streams a reasoning item either as
// summaries or as raw reasoning text, depending on the provider.
const (
	reasoningFlavorSummary = "summary"
	reasoningFlavorText    = "text"
)

func (d *responsesStreamState) handle(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	switch ev.Type {
	case "response.created":
		return d.handleCreated(ev, yield)
	case "response.output_text.delta":
		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: ev.Delta}, nil)
	case "response.reasoning_summary_text.delta":
		return d.handleReasoningDelta(ev, reasoningFlavorSummary, yield)
	case "response.reasoning_text.delta":
		return d.handleReasoningDelta(ev, reasoningFlavorText, yield)
	case "response.output_item.added":
		return d.handleItemAdded(ev, yield)
	case "response.output_item.done":
		return d.handleOutputItemDone(ev, yield)
	case "response.function_call_arguments.delta":
		return d.handleArgsDelta(ev, yield)
	case "response.function_call_arguments.done":
		return d.handleArgsDone(ev, yield)
	case "response.completed", "response.incomplete":
		return d.handleTerminal(ev, yield)
	case "response.failed":
		return d.handleFailed(ev, yield)
	case "error":
		yield(ai.StreamEvent{}, streamError(d.provider, ev))
		return false
	default:
		return true
	}
}

// handleFailed surfaces a response that failed mid-stream. A failure without
// an error object is only observable through its terminal status.
func (d *responsesStreamState) handleFailed(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Response != nil && ev.Response.Error != nil {
		yield(ai.StreamEvent{}, responsesFailure(d.provider, ev.Response.Error, nil))
		return false
	}

	return d.handleTerminal(ev, yield)
}

// streamError builds an *ai.Error for a Responses mid-stream error event,
// wrapping a class sentinel when the error code identifies one so errors.Is
// behaves the same as on the HTTP-status path.
func streamError(provider ai.Provider, ev responsesStreamEvent) error {
	apiErr := &ai.Error{Provider: provider, Code: ev.Code, Message: ev.Message}

	switch ev.Code {
	case "rate_limit_exceeded":
		return apiErr.WithSentinel(ai.ErrRateLimited)
	case "server_error":
		return apiErr.WithSentinel(ai.ErrOverloaded)
	default:
		return apiErr
	}
}

func (d *responsesStreamState) handleCreated(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if d.startSent {
		return true
	}

	d.startSent = true
	start := ai.StreamEvent{Type: ai.StreamMessageStart, Provider: d.provider}

	if ev.Response != nil {
		start.ID = ev.Response.ID
		start.Model = ev.Response.Model
	}

	return yield(start, nil)
}

func (d *responsesStreamState) handleItemAdded(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Item == nil {
		return true
	}

	if ev.Item.Type == typeReasoning {
		return d.handleReasoningItem(ev, yield)
	}

	if ev.Item.Type != typeFunctionCall {
		return true
	}

	idx := d.nextTool
	d.nextTool++
	d.toolSlot[ev.OutputIndex] = idx

	return yield(ai.StreamEvent{
		Type:          ai.StreamToolCallStart,
		ToolCallIndex: idx,
		ToolCallID:    encodeResponsesToolCallID(ev.Item.CallID, ev.Item.ID),
		ToolCallName:  ev.Item.Name,
	}, nil)
}

// handleReasoningDelta forwards a reasoning text fragment. Providers stream
// either summaries (response.reasoning_summary_text.*) or raw reasoning text
// (response.reasoning_text.*, what open-weight servers emit), and a response
// that sends both is rendering the same reasoning twice. The first flavor seen
// for an output index wins so the text reaches the caller exactly once.
func (d *responsesStreamState) handleReasoningDelta(
	ev responsesStreamEvent,
	flavor string,
	yield func(ai.StreamEvent, error) bool,
) bool {
	if seen, ok := d.reasoningFlavor[ev.OutputIndex]; ok && seen != flavor {
		return true
	}

	d.reasoningFlavor[ev.OutputIndex] = flavor

	return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: ev.Delta}, nil)
}

func (d *responsesStreamState) handleReasoningItem(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Item == nil || ev.Item.Type != typeReasoning {
		return true
	}

	signature := encodeResponsesReasoningState(*ev.Item)
	if signature == "" {
		return true
	}

	return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Signature: signature}, nil)
}

func (d *responsesStreamState) handleOutputItemDone(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Item == nil {
		return true
	}

	if ev.Item.Type == typeReasoning {
		return d.handleReasoningItem(ev, yield)
	}

	if ev.Item.Type == typeMessage {
		for _, c := range ev.Item.Content {
			for _, ann := range c.Annotations {
				if ann.Type == "url_citation" {
					cit := ai.Citation{
						URL:   ann.URL,
						Title: ann.Title,
						Index: d.nextCitation,
						TextRange: &ai.TextRange{
							Start: ann.StartIndex,
							End:   ann.EndIndex,
						},
					}
					d.nextCitation++
					if !yield(ai.StreamEvent{Type: ai.StreamCitation, Citation: &cit}, nil) {
						return false
					}
				}
			}
		}
	}

	return true
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

		var queries []string
		for _, item := range ev.Response.Output {
			if item.Type == "web_search_call" && item.Action != nil && item.Action.Query != "" {
				queries = append(queries, item.Action.Query)
			}
		}
		if len(queries) > 0 {
			end.Grounding = &ai.GroundingMetadata{
				WebSearchQueries: queries,
			}
		}
	}

	return yield(end, nil)
}
