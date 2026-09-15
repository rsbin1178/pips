package anthropic

import (
	"fmt"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// streamDecoder maps Anthropic's typed SSE events onto ai.StreamEvents.
//
// The wire sequence is: message_start, then per content block
// content_block_start / content_block_delta* / content_block_stop, then
// message_delta (carrying stop_reason and output usage) and message_stop.
// Thinking blocks emit thinking_delta then signature_delta; tool_use blocks
// emit input_json_delta.
type streamDecoder struct {
	blockKind map[int]blockKind
	toolIndex map[int]int // content-block index -> ai tool-call index
	nextTool  int
	toolUse   bool

	finish      ai.FinishReason
	inputUsage  int
	outputUsage int
	cacheRead   int
	cacheWrite  int
}

type blockKind int

const (
	blockOther blockKind = iota
	blockText
	blockThinking
	blockToolUse
)

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{
		blockKind: make(map[int]blockKind),
		toolIndex: make(map[int]int),
	}
}

func (d *streamDecoder) emit(events eventSource, yield func(ai.StreamEvent, error) bool) {
	for event, err := range events {
		if err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("anthropic: messages stream: %w", err))
			return
		}

		if event.Data == "" {
			continue
		}

		var env streamEnvelope
		if err := jsonx.Unmarshal([]byte(event.Data), &env); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("anthropic: decoding stream event: %w", err))
			return
		}

		if !d.handle(env, yield) {
			return
		}
	}
}

// handle processes one decoded event; it returns false when the consumer
// stopped iterating.
func (d *streamDecoder) handle(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	switch env.Type {
	case "message_start":
		return d.handleMessageStart(env, yield)
	case "content_block_start":
		return d.handleBlockStart(env, yield)
	case "content_block_delta":
		return d.handleBlockDelta(env, yield)
	case "content_block_stop":
		return d.handleBlockStop(env, yield)
	case "message_delta":
		return d.handleMessageDelta(env, yield)
	case "message_stop":
		return yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: d.finishReason(), Usage: d.usage()}, nil)
	case "error":
		msg := "stream error"
		if env.Error != nil {
			msg = env.Error.Message
		}

		yield(ai.StreamEvent{}, streamErrorFrom(env.Error, msg))

		return false
	default:
		// ping and unknown events carry no content.
		return true
	}
}

// finishReason returns the normalized stop reason for message_stop. Anthropic
// reports it on message_delta, but compatible servers exist that never send
// that event, so a missing reason falls back to what the streamed content
// implies: a message that opened a tool_use block stopped to call the tool,
// anything else completed normally. Only the server can distinguish a
// max_tokens stop, so truncated messages are reported as their content
// suggests and the lost reason cannot be recovered here.
func (d *streamDecoder) finishReason() ai.FinishReason {
	if d.finish != "" {
		return d.finish
	}

	if d.toolUse {
		return ai.FinishToolCalls
	}

	return ai.FinishStop
}

// finish and usage carry message-level results accumulated across events.
func (d *streamDecoder) usage() *ai.Usage {
	u := ai.Usage{InputTokens: d.inputUsage, OutputTokens: d.outputUsage}
	u.InputTokens += d.cacheRead + d.cacheWrite
	u.CachedInputTokens = d.cacheRead
	u.CacheWriteTokens = d.cacheWrite

	return &u
}

func (d *streamDecoder) handleMessageStart(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	ev := ai.StreamEvent{Type: ai.StreamMessageStart, Provider: ai.ProviderAnthropic}
	if env.Message != nil {
		ev.ID = env.Message.ID
		ev.Model = env.Message.Model
		d.inputUsage = env.Message.Usage.InputTokens
		d.cacheRead = env.Message.Usage.CacheReadInputTokens
		d.cacheWrite = env.Message.Usage.CacheCreationInputTokens
	}

	return yield(ev, nil)
}

func (d *streamDecoder) handleBlockStart(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	if env.ContentBlock == nil {
		return true
	}

	switch env.ContentBlock.Type {
	case blockTypeText:
		d.blockKind[env.Index] = blockText
	case blockTypeThinking, blockTypeRedactedThinking:
		d.blockKind[env.Index] = blockThinking
	case blockTypeToolUse:
		return d.startToolBlock(env, yield)
	default:
		d.blockKind[env.Index] = blockOther
	}

	return true
}

// startToolBlock begins a tool_use block.
func (d *streamDecoder) startToolBlock(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	d.blockKind[env.Index] = blockToolUse
	d.toolUse = true

	toolIdx := d.nextTool
	d.nextTool++
	d.toolIndex[env.Index] = toolIdx

	return yield(ai.StreamEvent{
		Type:          ai.StreamToolCallStart,
		ToolCallIndex: toolIdx,
		ToolCallID:    env.ContentBlock.ID,
		ToolCallName:  env.ContentBlock.Name,
	}, nil)
}

func (d *streamDecoder) handleBlockDelta(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	if env.Delta == nil {
		return true
	}

	switch env.Delta.Type {
	case "text_delta":
		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: env.Delta.Text}, nil)
	case "thinking_delta":
		return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: env.Delta.Thinking}, nil)
	case "signature_delta":
		return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Signature: env.Delta.Signature}, nil)
	case "input_json_delta":
		return d.handleInputDelta(env, yield)
	default:
		return true
	}
}

// handleInputDelta routes a tool input fragment to its normalized tool call.
func (d *streamDecoder) handleInputDelta(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	return yield(ai.StreamEvent{
		Type:          ai.StreamToolCallDelta,
		ToolCallIndex: d.toolIndex[env.Index],
		ArgsDelta:     env.Delta.PartialJSON,
	}, nil)
}

func (d *streamDecoder) handleBlockStop(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	if d.blockKind[env.Index] == blockToolUse {
		return yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: d.toolIndex[env.Index]}, nil)
	}

	return true
}

func (d *streamDecoder) handleMessageDelta(env streamEnvelope, _ func(ai.StreamEvent, error) bool) bool {
	if env.Delta != nil && env.Delta.StopReason != "" {
		d.finish = finishReasonFrom(env.Delta.StopReason)
	}

	if env.Usage != nil {
		d.outputUsage = env.Usage.OutputTokens
	}

	return true
}

func errType(e *streamError) string {
	if e == nil {
		return ""
	}

	return e.Type
}

// streamErrorFrom builds an *ai.Error for a mid-stream error event, wrapping
// the class sentinel that matches Anthropic's error type so errors.Is works
// the same as on the HTTP-status path (e.g. overloaded_error → ErrOverloaded).
func streamErrorFrom(e *streamError, msg string) error {
	apiErr := &ai.Error{Provider: ai.ProviderAnthropic, Type: errType(e), Message: msg}

	switch errType(e) {
	case "overloaded_error", "api_error":
		return apiErr.WithSentinel(ai.ErrOverloaded)
	case "rate_limit_error":
		return apiErr.WithSentinel(ai.ErrRateLimited)
	case "authentication_error", "permission_error":
		return apiErr.WithSentinel(ai.ErrAuth)
	case "invalid_request_error", "not_found_error":
		return apiErr.WithSentinel(ai.ErrInvalidRequest)
	default:
		return apiErr
	}
}
