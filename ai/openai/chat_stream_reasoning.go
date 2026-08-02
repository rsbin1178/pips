package openai

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rsbin/pips/ai"
)

type openRouterReasoningAccumulator struct {
	order   []string
	details map[string]map[string]json.RawMessage
}

func (s *chatStreamState) emitOpenRouterReasoning(
	details []json.RawMessage,
	yield func(ai.StreamEvent, error) bool,
) bool {
	if s.openRouterDetails == nil {
		s.openRouterDetails = &openRouterReasoningAccumulator{
			details: make(map[string]map[string]json.RawMessage),
		}
	}

	text, merged, err := s.openRouterDetails.append(details)
	if err != nil {
		yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding streamed reasoning_details: %w", s.provider, err))

		return false
	}

	signature, err := encodeChatReasoningState(chatReasoningState{
		Kind:              chatReasoningOpenRouter,
		OpenRouterDetails: merged,
	})
	if err != nil {
		yield(ai.StreamEvent{}, fmt.Errorf("%s: preserving streamed reasoning_details: %w", s.provider, err))

		return false
	}

	return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: text, Signature: signature}, nil)
}

func (a *openRouterReasoningAccumulator) append(
	incoming []json.RawMessage,
) (string, []json.RawMessage, error) {
	var visible strings.Builder

	for position, raw := range incoming {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return "", nil, fmt.Errorf("decoding member %d: %w", position, err)
		}

		typeName, err := rawStringField(fields, "type")
		if err != nil || typeName == "" {
			return "", nil, fmt.Errorf("member %d needs a string type", position)
		}

		if !knownOpenRouterReasoningType(typeName) {
			return "", nil, fmt.Errorf("member %d has unsupported type %q", position, typeName)
		}

		visible.WriteString(visibleReasoningFragment(typeName, fields))

		key := openRouterReasoningKey(typeName, fields, position)
		target, exists := a.details[key]

		if !exists {
			target = make(map[string]json.RawMessage, len(fields))
			a.details[key] = target
			a.order = append(a.order, key)
		}

		if err := mergeReasoningDetail(target, fields); err != nil {
			return "", nil, fmt.Errorf("merging member %d: %w", position, err)
		}
	}

	merged := make([]json.RawMessage, 0, len(a.order))
	for _, key := range a.order {
		raw, err := json.Marshal(a.details[key])
		if err != nil {
			return "", nil, fmt.Errorf("encoding merged member: %w", err)
		}

		merged = append(merged, raw)
	}

	if _, err := openRouterReasoningText(merged); err != nil {
		return "", nil, err
	}

	return visible.String(), merged, nil
}

func knownOpenRouterReasoningType(value string) bool {
	switch value {
	case "reasoning.text", "reasoning.summary", "reasoning.encrypted":
		return true
	default:
		return false
	}
}

func visibleReasoningFragment(typeName string, fields map[string]json.RawMessage) string {
	var field string

	switch typeName {
	case "reasoning.text":
		field = typeText
	case "reasoning.summary":
		field = "summary"
	default:
		return ""
	}

	value, _ := rawStringField(fields, field)

	return value
}

func openRouterReasoningKey(typeName string, fields map[string]json.RawMessage, position int) string {
	if raw, ok := fields["index"]; ok {
		return typeName + "|index=" + string(raw)
	}

	if id, _ := rawStringField(fields, "id"); id != "" {
		return typeName + "|id=" + id
	}

	return typeName + "|position=" + strconv.Itoa(position)
}

func mergeReasoningDetail(target, incoming map[string]json.RawMessage) error {
	for name, raw := range incoming {
		if !reasoningDetailFragmentField(name) || len(target[name]) == 0 {
			target[name] = cloneRawMessage(raw)

			continue
		}

		previous, err := rawStringField(target, name)
		if err != nil {
			return err
		}

		fragment, err := rawStringField(incoming, name)
		if err != nil {
			return err
		}

		combined, err := json.Marshal(previous + fragment)
		if err != nil {
			return fmt.Errorf("encoding %s fragment: %w", name, err)
		}

		target[name] = combined
	}

	return nil
}

func reasoningDetailFragmentField(name string) bool {
	switch name {
	case "text", "summary", "data", "signature":
		return true
	default:
		return false
	}
}

func rawStringField(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", name, err)
	}

	return value, nil
}

type mistralThinkingAccumulator struct {
	fields   map[string]json.RawMessage
	thinking []json.RawMessage
}

func (s *chatStreamState) emitChatContent(
	content *json.RawMessage,
	yield func(ai.StreamEvent, error) bool,
) bool {
	if content == nil || len(*content) == 0 || string(*content) == "null" {
		return true
	}

	var text string
	if err := json.Unmarshal(*content, &text); err == nil {
		s.mistralThinking = nil

		if text == "" {
			return true
		}

		return yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: text}, nil)
	}

	var chunks []json.RawMessage
	if err := json.Unmarshal(*content, &chunks); err != nil {
		yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding streamed assistant content: %w", s.provider, err))

		return false
	}

	return s.emitChatContentChunks(chunks, yield)
}

func (s *chatStreamState) emitChatContentChunks(
	chunks []json.RawMessage,
	yield func(ai.StreamEvent, error) bool,
) bool {
	for index, raw := range chunks {
		var chunk mistralContentChunk
		if err := json.Unmarshal(raw, &chunk); err != nil {
			yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding streamed content[%d]: %w", s.provider, index, err))

			return false
		}

		switch chunk.Type {
		case typeText:
			s.mistralThinking = nil

			if chunk.Text != "" && !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: chunk.Text}, nil) {
				return false
			}
		case typeThinking:
			if !s.emitMistralThinking(raw, yield) {
				return false
			}
		default:
			yield(ai.StreamEvent{}, fmt.Errorf("%s: streamed content[%d] has unsupported type %q", s.provider, index, chunk.Type))

			return false
		}
	}

	return true
}

func (s *chatStreamState) emitMistralThinking(
	raw json.RawMessage,
	yield func(ai.StreamEvent, error) bool,
) bool {
	if s.mistralThinking == nil {
		s.mistralThinking = &mistralThinkingAccumulator{fields: make(map[string]json.RawMessage)}
	}

	text, merged, err := s.mistralThinking.append(raw)
	if err != nil {
		yield(ai.StreamEvent{}, fmt.Errorf("%s: decoding streamed thinking chunk: %w", s.provider, err))

		return false
	}

	signature, err := encodeChatReasoningState(chatReasoningState{
		Kind:            chatReasoningMistral,
		MistralThinking: merged,
	})
	if err != nil {
		yield(ai.StreamEvent{}, fmt.Errorf("%s: preserving streamed thinking chunk: %w", s.provider, err))

		return false
	}

	return yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: text, Signature: signature}, nil)
}

func (a *mistralThinkingAccumulator) append(raw json.RawMessage) (string, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, err
	}

	typeName, err := rawStringField(fields, "type")
	if err != nil || typeName != typeThinking {
		return "", nil, fmt.Errorf("expected thinking chunk, got %q", typeName)
	}

	var incoming []json.RawMessage
	if thinkingRaw, ok := fields["thinking"]; ok {
		if err := json.Unmarshal(thinkingRaw, &incoming); err != nil {
			return "", nil, fmt.Errorf("thinking must be an array: %w", err)
		}
	}

	visible, err := mistralThinkingText(raw)
	if err != nil {
		return "", nil, err
	}

	a.thinking = append(a.thinking, cloneRawMessages(incoming)...)

	for name, value := range fields {
		if name == typeThinking {
			continue
		}

		a.fields[name] = cloneRawMessage(value)
	}

	thinking, err := json.Marshal(a.thinking)
	if err != nil {
		return "", nil, fmt.Errorf("encoding accumulated thinking: %w", err)
	}

	a.fields[typeThinking] = thinking

	merged, err := json.Marshal(a.fields)
	if err != nil {
		return "", nil, fmt.Errorf("encoding accumulated thinking chunk: %w", err)
	}

	return visible, merged, nil
}
