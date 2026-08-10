package ai

import (
	"fmt"
	"reflect"
)

type partSliceKey struct {
	data uintptr
	len  int
	cap  int
}

// CloneParts returns a deep, cycle-safe copy of a supported role-neutral part
// graph.
func CloneParts(parts []Part) ([]Part, error) {
	if err := validateParts(parts); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}

	return cloneParts(parts), nil
}

// MessageParts returns a role-neutral snapshot of a message's parts. The
// snapshot recursively owns mutable byte and JSON data.
func MessageParts(message Message) ([]Part, error) {
	if err := ValidateMessage(message); err != nil {
		return nil, err
	}

	parts, err := messagePartsView(message)
	if err != nil {
		return nil, err
	}

	return cloneParts(parts), nil
}

// CloneMessage returns a deep copy of a supported concrete message.
func CloneMessage(message Message) (Message, error) {
	if err := ValidateMessage(message); err != nil {
		return nil, err
	}

	switch message := message.(type) {
	case SystemMessage:
		return SystemMessage{Parts: cloneSystemParts(message.Parts)}, nil
	case UserMessage:
		return UserMessage{Parts: cloneUserParts(message.Parts)}, nil
	case AssistantMessage:
		return AssistantMessage{Parts: cloneAssistantParts(message.Parts)}, nil
	case ToolMessage:
		parts := make([]ToolResultPart, len(message.Parts))

		memo := make(map[partSliceKey][]Part)
		for i, part := range message.Parts {
			parts[i] = cloneToolResultPart(part, memo)
		}

		return ToolMessage{Parts: parts}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported concrete type %T", ErrInvalidMessage, message)
	}
}

func cloneSystemParts(parts []SystemPart) []SystemPart {
	if parts == nil {
		return nil
	}

	cloned := make([]SystemPart, len(parts))
	for i, part := range parts {
		if text, ok := part.(TextPart); ok {
			cloned[i] = text
		}
	}

	return cloned
}

func cloneUserParts(parts []UserPart) []UserPart {
	if parts == nil {
		return nil
	}

	cloned := make([]UserPart, len(parts))
	for i, part := range parts {
		switch part := part.(type) {
		case TextPart:
			cloned[i] = part
		case ImagePart:
			part.Source.Data = append([]byte(nil), part.Source.Data...)
			cloned[i] = part
		case FilePart:
			part.Source.Data = append([]byte(nil), part.Source.Data...)
			cloned[i] = part
		}
	}

	return cloned
}

func cloneAssistantParts(parts []AssistantPart) []AssistantPart {
	if parts == nil {
		return nil
	}

	cloned := make([]AssistantPart, len(parts))
	for i, part := range parts {
		switch part := part.(type) {
		case TextPart:
			cloned[i] = part
		case ReasoningPart:
			cloned[i] = part
		case ToolCallPart:
			part.Args = append(JSON(nil), part.Args...)
			cloned[i] = part
		}
	}

	return cloned
}

func cloneParts(parts []Part) []Part {
	return clonePartsWithMemo(parts, make(map[partSliceKey][]Part))
}

func clonePartsWithMemo(parts []Part, memo map[partSliceKey][]Part) []Part {
	if parts == nil {
		return nil
	}

	if len(parts) == 0 {
		return []Part{}
	}

	key := partSliceKey{data: reflect.ValueOf(parts).Pointer(), len: len(parts), cap: cap(parts)}
	if cloned, ok := memo[key]; ok {
		return cloned
	}

	cloned := make([]Part, len(parts))

	memo[key] = cloned
	for i, part := range parts {
		cloned[i] = clonePart(part, memo)
	}

	return cloned
}

func clonePart(part Part, memo map[partSliceKey][]Part) Part {
	switch part := part.(type) {
	case TextPart:
		return part
	case ImagePart:
		part.Source.Data = append([]byte(nil), part.Source.Data...)
		return part
	case FilePart:
		part.Source.Data = append([]byte(nil), part.Source.Data...)
		return part
	case ReasoningPart:
		return part
	case ToolCallPart:
		part.Args = append(JSON(nil), part.Args...)
		return part
	case ToolResultPart:
		return cloneToolResultPart(part, memo)
	default:
		return nil
	}
}

func cloneToolResultPart(part ToolResultPart, memo map[partSliceKey][]Part) ToolResultPart {
	part.Content = clonePartsWithMemo(part.Content, memo)

	return part
}
