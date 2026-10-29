package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

const responsesPath = "responses"

const (
	responseStatusCompleted  = "completed"
	responseStatusIncomplete = "incomplete"
)

func (m *Model) generateResponses(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, warnings, err := m.responsesRequestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed responsesResponse

	raw, err := m.client.PostJSON(ctx, responsesPath, m.authHeaders(), body, &parsed, m.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: responses: %w", m.label(), err)
	}

	if parsed.Status == "failed" {
		if parsed.Error != nil {
			return nil, responsesFailure(m.provider, parsed.Error, raw)
		}

		resp := responseFromResponses(parsed, nil, m.provider)

		return resp, responsesStateError(ai.ErrResponseFailed, resp, parsed.Status)
	}

	resp := responseFromResponses(parsed, raw, m.provider)

	resp.Warnings = warnings
	if cause := responsesRejection(parsed); cause != nil {
		return resp, responsesStateError(cause, resp, parsed.Status)
	}

	if err := validateResponsesTools(parsed); err != nil {
		return resp, responsesStateError(err, resp, parsed.Status)
	}

	return resp, nil
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

	if sentinel := responsesErrorClass(e.Code, e.Type); sentinel != nil {
		return apiErr.WithSentinel(sentinel)
	}

	return apiErr
}

// responsesErrorClass maps the class values a provider may use onto the
// provider-neutral sentinels. The Responses dialect names the class in the code,
// while the nested shape a router sends may carry a more specific code beside the
// family in the type, so every candidate is tried and the first known class wins.
func responsesErrorClass(values ...string) error {
	for _, value := range values {
		switch value {
		case "rate_limit_exceeded":
			return ai.ErrRateLimited
		case "server_error":
			return ai.ErrOverloaded
		}
	}

	return nil
}

func (m *Model) streamResponses(ctx context.Context, req ai.Request) ai.Stream {
	body, warnings, err := m.responsesRequestFrom(req, true)

	emit := func(provider ai.Provider, events eventSource, yield func(ai.StreamEvent, error) bool) {
		emitResponsesStream(provider, events, withWarnings(warnings, yield))
	}

	return m.runStream(ctx, responsesPath, "responses", body, err, emit)
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
		text:            make(map[responseTextKey]*strings.Builder),
		reasoning:       make(map[int]*strings.Builder),
		items:           make(map[string]int),
		tools:           make(map[int]*responseToolState),
		citations:       make(map[responseAnnotation]bool),
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

	if !d.terminal {
		yield(ai.StreamEvent{}, d.failure(ai.ErrIncompleteStream))
	}
}

type responsesStreamState struct {
	provider  ai.Provider
	startSent bool
	terminal  bool
	response  *responsesResponse
	id, model string
	text      map[responseTextKey]*strings.Builder
	reasoning map[int]*strings.Builder
	items     map[string]int
	tools     map[int]*responseToolState
	citations map[responseAnnotation]bool
	refused   bool
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
		return d.handleTextDelta(ev, yield)
	case "response.refusal.delta":
		d.refused = true
		return true
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
	d.response = ev.Response

	d.terminal = true
	if ev.Response != nil && ev.Response.Error != nil {
		yield(ai.StreamEvent{}, responsesFailure(d.provider, ev.Response.Error, nil))
		return false
	}

	yield(ai.StreamEvent{}, d.failure(ai.ErrResponseFailed))

	return false
}

// streamError builds an *ai.Error for a Responses mid-stream error event,
// wrapping a class sentinel when the error code identifies one so errors.Is
// behaves the same as on the HTTP-status path. It accepts both shapes the event
// arrives in: the dialect's top-level code and message, and the nested object a
// router sends, preferring the top level when both are present.
func streamError(provider ai.Provider, ev responsesStreamEvent) error {
	apiErr := &ai.Error{Provider: provider, Code: ev.Code, Message: ev.Message}
	classes := []string{ev.Code}

	if ev.Error != nil {
		if apiErr.Code == "" {
			apiErr.Code = ev.Error.Code
		}

		if apiErr.Message == "" {
			apiErr.Message = ev.Error.Message
		}

		apiErr.Type = ev.Error.Type
		classes = append(classes, ev.Error.Code, ev.Error.Type)
	}

	if sentinel := responsesErrorClass(classes...); sentinel != nil {
		return apiErr.WithSentinel(sentinel)
	}

	return apiErr
}

func (d *responsesStreamState) handleCreated(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if d.startSent {
		return true
	}

	d.startSent = true
	start := ai.StreamEvent{Type: ai.StreamMessageStart, Provider: d.provider}

	if ev.Response != nil {
		d.response = ev.Response
		start.ID = ev.Response.ID
		start.Model = ev.Response.Model
		d.id, d.model = start.ID, start.Model
	}

	return yield(start, nil)
}

func (d *responsesStreamState) handleItemAdded(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if ev.Item == nil {
		return true
	}

	if ev.Item.ID != "" {
		d.items[ev.Item.ID] = ev.OutputIndex
	}

	if ev.Item.Type == typeReasoning {
		return d.handleReasoningItem(ev, yield)
	}

	if ev.Item.Type != typeFunctionCall {
		return true
	}

	return d.reconcileTool(ev.OutputIndex, *ev.Item, yield)
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
	if d.reasoning[ev.OutputIndex] == nil {
		d.reasoning[ev.OutputIndex] = &strings.Builder{}
	}

	_, _ = d.reasoning[ev.OutputIndex].WriteString(ev.Delta)

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

	return d.reconcileItem(ev.OutputIndex, *ev.Item, yield)
}

func (d *responsesStreamState) handleArgsDelta(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	idx, ok := d.toolSlot[ev.OutputIndex]
	if !ok {
		return true
	}

	_, _ = d.tools[ev.OutputIndex].args.WriteString(ev.Delta)

	return yield(ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: idx, ArgsDelta: ev.Delta}, nil)
}

func (d *responsesStreamState) handleArgsDone(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	tool, ok := d.tools[ev.OutputIndex]
	if !ok {
		return true
	}

	tool.complete = true

	if ev.Arguments == nil {
		return true
	}

	return d.reconcileToolArgs(ev.OutputIndex, tool, *ev.Arguments, yield)
}

func (d *responsesStreamState) handleTerminal(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	if d.terminal {
		return false
	}

	d.terminal = true
	ev = normalizeResponsesTerminal(ev)

	end := ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}
	if ev.Response != nil && !d.reconcileTerminal(ev, &end, yield) {
		return false
	}

	if d.refused {
		yield(ai.StreamEvent{}, d.failure(ai.ErrRefused))
		return false
	}

	if end.FinishReason != ai.FinishLength {
		for _, tool := range d.tools {
			if !validResponseTool(tool) {
				yield(ai.StreamEvent{}, d.failure(ai.ErrIncompleteStream))
				return false
			}
		}
	}

	if end.FinishReason == ai.FinishStop && len(d.tools) > 0 {
		end.FinishReason = ai.FinishToolCalls
	}

	for index := range d.nextTool {
		if !yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: index}, nil) {
			return false
		}
	}

	yield(end, nil)

	return false
}

func normalizeResponsesTerminal(ev responsesStreamEvent) responsesStreamEvent {
	if ev.Type != "response.incomplete" {
		return ev
	}

	body := responsesResponse{}
	if ev.Response != nil {
		body = *ev.Response
	}

	body.Status = responseStatusIncomplete
	ev.Response = &body

	return ev
}

func (d *responsesStreamState) failure(cause error) error {
	resp := &ai.Response{Provider: d.provider, ID: d.id, Model: d.model}
	for _, text := range d.text {
		resp.Message.Parts = append(resp.Message.Parts, ai.TextPart{Text: text.String()})
	}

	for range d.tools {
		resp.Message.Parts = append(resp.Message.Parts, ai.ToolCallPart{})
	}

	for range d.reasoning {
		resp.Message.Parts = append(resp.Message.Parts, ai.ReasoningPart{})
	}

	err := ai.NewResponseError(cause, resp, d.terminal)
	if d.response != nil {
		final := ai.NewResponseError(cause, responseFromResponses(*d.response, nil, d.provider), d.terminal)
		err.FinishReason, err.Usage, err.Status = final.FinishReason, final.Usage, d.response.Status
		err.TextBytes, err.ToolCalls = max(err.TextBytes, final.TextBytes), max(err.ToolCalls, final.ToolCalls)
		err.HasReasoning = err.HasReasoning || final.HasReasoning
	}

	return err
}

func (d *responsesStreamState) reconcileTerminal(ev responsesStreamEvent, end *ai.StreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	d.response = ev.Response
	resp := responseFromResponses(*ev.Response, nil, d.provider)

	cause := responsesRejection(*ev.Response)
	if d.refused {
		cause = ai.ErrRefused
	}

	if cause == nil {
		cause = validateResponsesTools(*ev.Response)
	}

	if cause != nil {
		yield(ai.StreamEvent{}, d.failure(cause))
		return false
	}

	if !d.startSent && !d.handleCreated(ev, yield) {
		return false
	}

	end.FinishReason = resp.FinishReason
	usage := resp.Usage
	end.Usage, end.Grounding = &usage, resp.Grounding

	for index, item := range ev.Response.Output {
		index = d.outputIndex(index, item)
		if !d.reconcileItem(index, item, yield) {
			return false
		}
	}

	return true
}

func (d *responsesStreamState) outputIndex(index int, item responseItem) int {
	if original, ok := d.items[item.ID]; item.ID != "" && ok {
		index = original
	}

	if item.Type == typeFunctionCall {
		return d.toolIndex(index, item)
	}

	return index
}

func (d *responsesStreamState) handleTextDelta(ev responsesStreamEvent, yield func(ai.StreamEvent, error) bool) bool {
	key := responseTextKey{ev.OutputIndex, ev.ContentIndex}
	if d.text[key] == nil {
		d.text[key] = &strings.Builder{}
	}

	_, _ = d.text[key].WriteString(ev.Delta)

	return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: ev.Delta}, nil)
}
