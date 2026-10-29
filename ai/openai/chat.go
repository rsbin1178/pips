package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

const chatPath = "chat/completions"

// Generate implements ai.LanguageModel.
func (m *Model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	switch m.resolveAPI() {
	case APIResponses:
		return m.generateResponses(ctx, req)
	default:
		return m.generateChat(ctx, req)
	}
}

// Stream implements ai.LanguageModel.
func (m *Model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	switch m.resolveAPI() {
	case APIResponses:
		return m.streamResponses(ctx, req)
	default:
		return m.streamChat(ctx, req)
	}
}

func (m *Model) generateChat(ctx context.Context, req ai.Request) (*ai.Response, error) {
	body, warnings, err := m.chatRequestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed chatResponse

	raw, err := m.client.PostJSON(ctx, chatPath, m.authHeaders(), body, &parsed, m.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: chat completions: %w", m.label(), err)
	}

	resp, err := responseFromChat(parsed, raw, m.provider)
	if err != nil {
		return nil, fmt.Errorf("%s: chat completions: %w", m.label(), err)
	}

	resp.Warnings = warnings

	return resp, nil
}

func (m *Model) streamChat(ctx context.Context, req ai.Request) ai.Stream {
	body, warnings, err := m.chatRequestFrom(req, true)

	emit := func(provider ai.Provider, events eventSource, yield func(ai.StreamEvent, error) bool) {
		emitChatStream(provider, events, withWarnings(warnings, yield))
	}

	return m.runStream(ctx, chatPath, "chat completions", body, err, emit)
}

// chatStreamState tracks what has been emitted so the SSE chunk sequence
// becomes a well-formed ai.Stream (start → deltas → tool ends → end).
type chatStreamState struct {
	provider          ai.Provider
	startSent         bool
	endSent           bool
	finish            ai.FinishReason
	id, model         string
	refused           bool
	toolArgs          map[int]*strings.Builder
	toolIdentity      map[int]bool
	usage             *ai.Usage
	openTools         []int
	toolOpen          map[int]bool
	openRouterDetails *openRouterReasoningAccumulator
	mistralThinking   *mistralThinkingAccumulator
}

// emitChatStream drives the Chat Completions SSE dialect: JSON chunks under
// "data:" lines, terminated by a [DONE] sentinel.
func emitChatStream(provider ai.Provider, events eventSource, yield func(ai.StreamEvent, error) bool) {
	state := &chatStreamState{provider: provider, toolOpen: make(map[int]bool), toolArgs: make(map[int]*strings.Builder), toolIdentity: make(map[int]bool)}

	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: chat completions stream: %w", provider, err))
			return
		}

		if event.Data == doneSentinel {
			state.emitEnd(yield)
			return
		}

		var chunk chatResponse
		if err := jsonx.Unmarshal([]byte(event.Data), &chunk); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding stream chunk: %w", provider, err))
			return
		}

		if !state.emitChunk(chunk, yield) {
			return
		}
	}

	// A finish reason is authoritative even when the optional sentinel is absent.
	state.emitEnd(yield)
}

const doneSentinel = "[DONE]"

// emitChunk translates one parsed chunk; it reports false when the consumer
// stopped iterating.
func (s *chatStreamState) emitChunk(chunk chatResponse, yield func(ai.StreamEvent, error) bool) bool {
	if !s.startSent {
		s.startSent = true
		s.id, s.model = chunk.ID, chunk.Model

		if !yield(ai.StreamEvent{
			Type:     ai.StreamMessageStart,
			Provider: s.provider,
			ID:       chunk.ID,
			Model:    chunk.Model,
		}, nil) {
			return false
		}
	}

	if chunk.Usage != nil {
		u := usageFromChat(chunk.Usage)
		s.usage = &u
	}

	if len(chunk.Choices) == 0 {
		return true
	}

	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		s.finish = finishReasonFromChat(choice.FinishReason)
	}

	if choice.Delta == nil {
		return true
	}

	return s.emitDelta(*choice.Delta, yield)
}

func (s *chatStreamState) emitDelta(delta chatChoiceMessage, yield func(ai.StreamEvent, error) bool) bool {
	s.refused = s.refused || delta.Refusal != ""
	switch {
	case len(delta.ReasoningDetails) > 0:
		if !s.emitOpenRouterReasoning(delta.ReasoningDetails, yield) {
			return false
		}
	case delta.ReasoningContent != "":
		if !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: delta.ReasoningContent}, nil) {
			return false
		}
	case delta.Reasoning != "":
		if !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: delta.Reasoning}, nil) {
			return false
		}
	}

	if !s.emitChatContent(delta.Content, yield) {
		return false
	}

	for _, call := range delta.ToolCalls {
		if !s.emitToolCallDelta(call, yield) {
			return false
		}
	}

	return true
}

// emitToolCallDelta handles one incremental tool_calls entry: the first
// fragment for an index carries id+name (start), every fragment may carry an
// arguments piece.
func (s *chatStreamState) emitToolCallDelta(call chatToolCall, yield func(ai.StreamEvent, error) bool) bool {
	index := 0
	if call.Index != nil {
		index = *call.Index
	}

	if !s.toolOpen[index] {
		s.toolOpen[index] = true
		s.openTools = append(s.openTools, index)
		s.toolIdentity[index] = call.ID != "" && call.Function.Name != ""

		start := ai.StreamEvent{
			Type:          ai.StreamToolCallStart,
			ToolCallIndex: index,
			ToolCallID:    call.ID,
			ToolCallName:  call.Function.Name,
		}
		if !yield(start, nil) {
			return false
		}
	}

	if call.Function.Arguments != "" {
		if s.toolArgs[index] == nil {
			s.toolArgs[index] = &strings.Builder{}
		}

		_, _ = s.toolArgs[index].WriteString(call.Function.Arguments)

		delta := ai.StreamEvent{
			Type:          ai.StreamToolCallDelta,
			ToolCallIndex: index,
			ArgsDelta:     call.Function.Arguments,
		}
		if !yield(delta, nil) {
			return false
		}
	}

	return true
}

// emitEnd closes any open tool calls and emits the final message_end.
func (s *chatStreamState) emitEnd(yield func(ai.StreamEvent, error) bool) {
	if s.endSent {
		return
	}

	s.endSent = true

	resp := &ai.Response{Provider: s.provider, ID: s.id, Model: s.model, FinishReason: s.finish}
	if s.usage != nil {
		resp.Usage = *s.usage
	}

	var cause error

	switch {
	case s.finish == ai.FinishContentFilter:
		cause = ai.ErrContentFiltered
	case s.refused:
		cause = ai.ErrRefused
	case s.finish == "":
		cause = ai.ErrIncompleteStream
	}

	if cause != nil {
		yield(ai.StreamEvent{}, ai.NewResponseError(cause, resp, s.finish != ""))
		return
	}

	if !s.validTools() {
		yield(ai.StreamEvent{}, ai.NewResponseError(ai.ErrIncompleteStream, resp, true))
		return
	}

	for _, index := range s.openTools {
		if !yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: index}, nil) {
			return
		}
	}

	yield(ai.StreamEvent{
		Type:         ai.StreamMessageEnd,
		FinishReason: s.finish,
		Usage:        s.usage,
	}, nil)
}

func (s *chatStreamState) validTools() bool {
	if s.finish == ai.FinishLength {
		return true
	}

	for _, index := range s.openTools {
		if !s.toolIdentity[index] || s.toolArgs[index] == nil || !json.Valid([]byte(s.toolArgs[index].String())) {
			return false
		}
	}

	return true
}
