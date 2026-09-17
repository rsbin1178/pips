package ai

import (
	"iter"
	"strings"
)

// Stream is a sequence of streaming events. Iterate with range; breaking out
// of the loop cancels the underlying request and releases its connection:
//
//	for ev, err := range model.Stream(ctx, req) {
//	    if err != nil { ... ; break }
//	    ...
//	}
//
// After a non-nil error the sequence yields nothing further. Connection and
// pre-flight failures surface as an error on the first iteration.
type Stream = iter.Seq2[StreamEvent, error]

// StreamEventType discriminates [StreamEvent] variants.
type StreamEventType string

// Stream event types, in the order a well-formed stream produces them:
// one message_start; any interleaving of text_delta, reasoning_delta, and
// tool_call_start/tool_call_delta/tool_call_end groups; one message_end.
const (
	// StreamMessageStart opens the response; it carries Provider, ID, and
	// Model.
	StreamMessageStart StreamEventType = "message_start"
	// StreamTextDelta carries a fragment of answer text in Text.
	StreamTextDelta StreamEventType = "text_delta"
	// StreamReasoningDelta carries a fragment of reasoning content in Text.
	StreamReasoningDelta StreamEventType = "reasoning_delta"
	// StreamToolCallStart announces a tool invocation; it carries
	// ToolCallIndex, ToolCallID, and ToolCallName.
	StreamToolCallStart StreamEventType = "tool_call_start"
	// StreamToolCallDelta carries a fragment of the call's JSON arguments in
	// ArgsDelta for the call identified by ToolCallIndex.
	StreamToolCallDelta StreamEventType = "tool_call_delta"
	// StreamToolCallEnd closes the arguments of the call identified by
	// ToolCallIndex.
	StreamToolCallEnd StreamEventType = "tool_call_end"
	// StreamCitation announces a source citation produced by a search or
	// grounding tool; it carries Citation.
	StreamCitation StreamEventType = "citation"
	// StreamMessageEnd closes the response; it carries FinishReason and,
	// when the provider reports it, Usage. It may also carry Grounding.
	StreamMessageEnd StreamEventType = "message_end"
)

// StreamEvent is one normalized increment of a streaming response. Only the
// fields documented for the event's Type are meaningful.
type StreamEvent struct {
	Type StreamEventType

	// Provider, ID, and Model are set on message_start.
	Provider Provider
	ID       string
	Model    string

	// Text is the fragment for text_delta and reasoning_delta.
	Text string
	// Signature carries opaque provider continuation state on reasoning_delta
	// events (for example Anthropic signature_delta, Gemini thoughtSignature,
	// or an OpenAI Responses reasoning item). It may arrive with an empty Text
	// and applies to the reasoning part being accumulated.
	Signature string

	// ToolCallIndex orders concurrent tool calls within the response; it is
	// set on all tool_call_* events.
	ToolCallIndex int
	// ToolCallID and ToolCallName are set on tool_call_start.
	ToolCallID   string
	ToolCallName string
	// ArgsDelta is the JSON-arguments fragment for tool_call_delta.
	ArgsDelta string

	// Citation is set on citation events.
	Citation *Citation

	// FinishReason and Usage are set on message_end. Usage is nil when the
	// provider does not report it. Grounding carries search metadata when
	// available. Warnings reports anything the adapter did not send exactly as
	// configured; it is set on message_end and is empty when the request was
	// encoded as written.
	FinishReason FinishReason
	Usage        *Usage
	Grounding    *GroundingMetadata
	Warnings     []Warning
}

// Collect drains a stream and assembles the complete [Response], preserving
// part order (text, reasoning, and tool calls appear where they occurred).
// On mid-stream failure it returns the partial response together with the
// error.
func Collect(stream Stream) (*Response, error) {
	acc := newAccumulator()

	for ev, err := range stream {
		if err != nil {
			return acc.response(), err
		}

		acc.add(ev)
	}

	return acc.response(), nil
}

// accumulator folds StreamEvents into a Response. Provider adapters and the
// retry middleware share it via Collect.
type accumulator struct {
	resp Response

	textBuf      strings.Builder
	reasoningBuf strings.Builder
	reasoningSig string
	// open tracks which buffer is accumulating so consecutive deltas of the
	// same kind merge into one part while preserving overall order.
	open StreamEventType

	// toolSlot maps a stream's ToolCallIndex to the position of its
	// ToolCallPart in resp.Message.Parts; toolArgs accumulates its arguments.
	toolSlot map[int]int
	toolArgs map[int]*strings.Builder
}

func newAccumulator() *accumulator {
	return &accumulator{
		resp:     Response{Message: AssistantMessage{}},
		toolSlot: make(map[int]int),
		toolArgs: make(map[int]*strings.Builder),
	}
}

func (a *accumulator) add(ev StreamEvent) {
	switch ev.Type {
	case StreamMessageStart:
		a.resp.Provider = ev.Provider
		a.resp.ID = ev.ID
		a.resp.Model = ev.Model
	case StreamTextDelta:
		if a.open != StreamTextDelta {
			a.flush()
			a.open = StreamTextDelta
		}

		a.textBuf.WriteString(ev.Text)
	case StreamReasoningDelta:
		if a.open != StreamReasoningDelta {
			a.flush()
			a.open = StreamReasoningDelta
		}

		a.reasoningBuf.WriteString(ev.Text)

		if ev.Signature != "" {
			a.reasoningSig = ev.Signature
		}
	case StreamToolCallStart:
		a.flush()
		a.toolSlot[ev.ToolCallIndex] = len(a.resp.Message.Parts)
		a.toolArgs[ev.ToolCallIndex] = &strings.Builder{}
		a.resp.Message.Parts = append(a.resp.Message.Parts, ToolCallPart{
			ID:   ev.ToolCallID,
			Name: ev.ToolCallName,
		})
	case StreamToolCallDelta:
		if buf, ok := a.toolArgs[ev.ToolCallIndex]; ok {
			buf.WriteString(ev.ArgsDelta)
		}
	case StreamToolCallEnd:
		a.sealToolCall(ev.ToolCallIndex)
	case StreamCitation:
		if ev.Citation != nil {
			a.resp.Citations = append(a.resp.Citations, *ev.Citation)
		}
	case StreamMessageEnd:
		a.finish(ev)
	}
}

// finish closes the response with the terminal event's summary fields.
func (a *accumulator) finish(ev StreamEvent) {
	a.flush()

	a.resp.FinishReason = ev.FinishReason
	if ev.Usage != nil {
		a.resp.Usage = *ev.Usage
	}

	if ev.Grounding != nil {
		a.resp.Grounding = ev.Grounding
	}

	if len(ev.Warnings) > 0 {
		a.resp.Warnings = append(a.resp.Warnings, ev.Warnings...)
	}
}

// flush closes the currently accumulating text or reasoning part, if any.
func (a *accumulator) flush() {
	switch a.open {
	case StreamTextDelta:
		if a.textBuf.Len() > 0 {
			a.resp.Message.Parts = append(a.resp.Message.Parts, TextPart{Text: a.textBuf.String()})
			a.textBuf.Reset()
		}
	case StreamReasoningDelta:
		if a.reasoningBuf.Len() > 0 || a.reasoningSig != "" {
			a.resp.Message.Parts = append(a.resp.Message.Parts, ReasoningPart{
				Text:      a.reasoningBuf.String(),
				Signature: a.reasoningSig,
			})
			a.reasoningBuf.Reset()

			a.reasoningSig = ""
		}
	default:
	}

	a.open = ""
}

// sealToolCall writes the accumulated arguments into the tool call's part.
func (a *accumulator) sealToolCall(index int) {
	slot, ok := a.toolSlot[index]
	if !ok {
		return
	}

	buf := a.toolArgs[index]

	call, _ := a.resp.Message.Parts[slot].(ToolCallPart)
	if args := buf.String(); args != "" {
		call.Args = JSON(args)
	}

	a.resp.Message.Parts[slot] = call
	delete(a.toolArgs, index)
}

// response finalizes and returns the accumulated Response.
func (a *accumulator) response() *Response {
	a.flush()

	for index := range a.toolArgs {
		a.sealToolCall(index)
	}

	resp := a.resp

	return &resp
}
