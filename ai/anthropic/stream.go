package anthropic

import (
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
)

// streamDecoder maps Anthropic's typed SSE events onto ai.StreamEvents.
//
// The wire sequence is: message_start, then per content block
// content_block_start / content_block_delta* / content_block_stop, then
// message_delta (carrying stop_reason and output usage) and message_stop.
// Thinking blocks emit thinking_delta then signature_delta; tool_use blocks
// emit input_json_delta. A structured-output tool call is rewritten into text
// deltas so streaming matches the other providers.
type streamDecoder struct {
	structured bool // request used ResponseFormat (forced tool = the answer)

	blockKind map[int]blockKind
	toolIndex map[int]int // content-block index -> ai tool-call index
	nextTool  int

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
	blockStructured
)

func newStreamDecoder(req ai.Request) *streamDecoder {
	return &streamDecoder{
		structured: req.ResponseFormat != nil,
		blockKind:  make(map[int]blockKind),
		toolIndex:  make(map[int]int),
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
		return yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: d.finish, Usage: d.usage()}, nil)
	case "error":
		msg := "stream error"
		if env.Error != nil {
			msg = env.Error.Message
		}

		yield(ai.StreamEvent{}, &ai.Error{Provider: ai.ProviderAnthropic, Type: errType(env.Error), Message: msg})

		return false
	default:
		// ping and unknown events carry no content.
		return true
	}
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
	ev := ai.StreamEvent{Type: ai.StreamMessageStart}
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

// startToolBlock begins a tool_use block. When the request coerced structured
// output, its single forced tool is the answer, so its input is streamed as
// text rather than a tool call.
func (d *streamDecoder) startToolBlock(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	if d.structured {
		d.blockKind[env.Index] = blockStructured
		return true
	}

	d.blockKind[env.Index] = blockToolUse

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

// handleInputDelta routes a tool input fragment: to a text delta for a
// structured-output block, to a tool-call delta otherwise.
func (d *streamDecoder) handleInputDelta(env streamEnvelope, yield func(ai.StreamEvent, error) bool) bool {
	if d.blockKind[env.Index] == blockStructured {
		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: env.Delta.PartialJSON}, nil)
	}

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
		if d.structured && d.finish == ai.FinishToolCalls {
			d.finish = ai.FinishStop
		}
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
