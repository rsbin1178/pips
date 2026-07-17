package openai

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
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
	body, err := m.chatRequestFrom(req, false)
	if err != nil {
		return nil, err
	}

	var parsed chatResponse

	raw, err := m.client.PostJSON(ctx, chatPath, m.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("openai: chat completions: %w", err)
	}

	resp, err := responseFromChat(parsed, raw)
	if err != nil {
		return nil, fmt.Errorf("openai: chat completions: %w", err)
	}

	return resp, nil
}

func (m *Model) streamChat(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		body, err := m.chatRequestFrom(req, true)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		stream, err := m.client.PostStream(ctx, chatPath, m.authHeaders(), body, decodeError)
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("openai: chat completions stream: %w", err))
			return
		}
		defer stream.Close() //nolint:errcheck // best-effort cleanup on all exit paths

		emitChatStream(newSSEParser(stream, m.client.MaxStreamLineSize()), yield)
	}
}

// chatStreamState tracks what has been emitted so the SSE chunk sequence
// becomes a well-formed ai.Stream (start → deltas → tool ends → end).
type chatStreamState struct {
	startSent bool
	endSent   bool
	finish    ai.FinishReason
	usage     *ai.Usage
	openTools []int
	toolOpen  map[int]bool
}

// emitChatStream drives the Chat Completions SSE dialect: JSON chunks under
// "data:" lines, terminated by a [DONE] sentinel.
func emitChatStream(events eventSource, yield func(ai.StreamEvent, error) bool) {
	state := &chatStreamState{toolOpen: make(map[int]bool)}

	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("openai: chat completions stream: %w", err))
			return
		}

		if event.Data == doneSentinel {
			state.emitEnd(yield)
			return
		}

		var chunk chatResponse
		if err := jsonx.Unmarshal([]byte(event.Data), &chunk); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("openai: decoding stream chunk: %w", err))
			return
		}

		if !state.emitChunk(chunk, yield) {
			return
		}
	}

	// Stream ended without [DONE]; still close out what we saw.
	state.emitEnd(yield)
}

const doneSentinel = "[DONE]"

// emitChunk translates one parsed chunk; it reports false when the consumer
// stopped iterating.
func (s *chatStreamState) emitChunk(chunk chatResponse, yield func(ai.StreamEvent, error) bool) bool {
	if !s.startSent {
		s.startSent = true

		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: chunk.ID, Model: chunk.Model}, nil) {
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
	if delta.ReasoningContent != "" {
		if !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: delta.ReasoningContent}, nil) {
			return false
		}
	}

	if delta.Content != nil && *delta.Content != "" {
		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: *delta.Content}, nil) {
			return false
		}
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
