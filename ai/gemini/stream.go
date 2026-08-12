package gemini

import (
	"fmt"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// emitStream translates Gemini's SSE dialect — each event is a full
// GenerateContentResponse carrying the next increment — into ai.StreamEvents.
// Text and thought parts stream as deltas; a functionCall arrives whole in one
// chunk, so it is emitted as start+delta+end together.
func emitStream(events eventSource, yield func(ai.StreamEvent, error) bool) {
	d := &streamState{}

	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("gemini: streamGenerateContent: %w", err))
			return
		}

		if event.Data == "" {
			continue
		}

		var chunk generateResponse
		if err := jsonx.Unmarshal([]byte(event.Data), &chunk); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("gemini: decoding stream chunk: %w", err))
			return
		}

		if !d.emitChunk(chunk, yield) {
			return
		}
	}

	d.emitEnd(yield)
}

type streamState struct {
	startSent bool
	endSent   bool
	finish    ai.FinishReason
	usage     *ai.Usage
	toolIndex int
	sawCall   bool
}

func (s *streamState) emitChunk(chunk generateResponse, yield func(ai.StreamEvent, error) bool) bool {
	if !s.startSent {
		s.startSent = true

		if !yield(ai.StreamEvent{
			Type:     ai.StreamMessageStart,
			Provider: ai.ProviderGemini,
			ID:       chunk.ResponseID,
			Model:    chunk.ModelVersion,
		}, nil) {
			return false
		}
	}

	if chunk.UsageMetadata != nil {
		u := usageFrom(chunk.UsageMetadata)
		s.usage = &u
	}

	if len(chunk.Candidates) == 0 {
		return true
	}

	candidate := chunk.Candidates[0]
	if candidate.FinishReason != "" {
		s.finish = finishReasonFrom(candidate.FinishReason)
	}

	for _, part := range candidate.Content.Parts {
		if !s.emitPart(part, yield) {
			return false
		}
	}

	return true
}

func (s *streamState) emitPart(part wirePart, yield func(ai.StreamEvent, error) bool) bool {
	switch {
	case part.FunctionCall != nil:
		return s.emitFunctionCall(part, yield)
	case part.Thought:
		return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: part.Text, Signature: part.ThoughtSignature}, nil)
	case part.Text != "":
		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: part.Text}, nil)
	default:
		return true
	}
}

// emitFunctionCall emits a whole tool call — Gemini delivers it in one chunk
// rather than as argument fragments — as start, one args delta, and end.
func (s *streamState) emitFunctionCall(part wirePart, yield func(ai.StreamEvent, error) bool) bool {
	s.sawCall = true
	index := s.toolIndex
	s.toolIndex++

	id := syntheticCallID(index, part.FunctionCall.ID, part.ThoughtSignature)
	if !yield(ai.StreamEvent{
		Type:          ai.StreamToolCallStart,
		ToolCallIndex: index,
		ToolCallID:    id,
		ToolCallName:  part.FunctionCall.Name,
	}, nil) {
		return false
	}

	if len(part.FunctionCall.Args) > 0 {
		if !yield(ai.StreamEvent{
			Type:          ai.StreamToolCallDelta,
			ToolCallIndex: index,
			ArgsDelta:     string(part.FunctionCall.Args),
		}, nil) {
			return false
		}
	}

	return yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: index}, nil)
}

func (s *streamState) emitEnd(yield func(ai.StreamEvent, error) bool) {
	if s.endSent {
		return
	}

	s.endSent = true

	finish := s.finish
	if finish == ai.FinishStop && s.sawCall {
		finish = ai.FinishToolCalls
	}

	yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: finish, Usage: s.usage}, nil)
}
