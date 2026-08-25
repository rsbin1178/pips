package ai

import (
	"fmt"
	"reflect"
)

// Validate checks that a system message uses its concrete value form and only
// contains supported system content.
func (m SystemMessage) Validate() error {
	for i, part := range m.Parts {
		if _, ok := part.(TextPart); !ok {
			return invalidMessagePart("system", i, part)
		}
	}

	return nil
}

// Validate checks that a user message uses only text, image, and file content.
func (m UserMessage) Validate() error {
	for i, part := range m.Parts {
		switch part.(type) {
		case TextPart, ImagePart, FilePart:
		default:
			return invalidMessagePart("user", i, part)
		}
	}

	return nil
}

// Validate checks that an assistant message uses only text, reasoning, and
// tool-call content.
func (m AssistantMessage) Validate() error {
	for i, part := range m.Parts {
		switch part.(type) {
		case TextPart, ReasoningPart, ToolCallPart:
		default:
			return invalidMessagePart("assistant", i, part)
		}
	}

	return nil
}

// Validate checks the nested content of every result in a tool message.
func (m ToolMessage) Validate() error {
	for i, result := range m.Parts {
		if err := validateParts(result.Content); err != nil {
			return fmt.Errorf("%w: tool result %d: %w", ErrInvalidMessage, i, err)
		}
	}

	return nil
}

// Validate checks every message and enforces that system messages form a
// prefix of the conversation.
func (messages Messages) Validate() error {
	seenConversation := false

	for i, message := range messages {
		if err := ValidateMessage(message); err != nil {
			return fmt.Errorf("%w: message %d: %w", ErrInvalidMessage, i, err)
		}

		if _, ok := message.(SystemMessage); ok {
			if seenConversation {
				return fmt.Errorf("%w: system message at index %d follows conversation content", ErrInvalidMessage, i)
			}

			continue
		}

		seenConversation = true
	}

	return nil
}

// ValidateMessage checks that message is a supported concrete value and that
// its content belongs to that role.
func ValidateMessage(message Message) error {
	switch message := message.(type) {
	case SystemMessage:
		return message.Validate()
	case UserMessage:
		return message.Validate()
	case AssistantMessage:
		return message.Validate()
	case ToolMessage:
		return message.Validate()
	case nil:
		return fmt.Errorf("%w: nil", ErrInvalidMessage)
	default:
		return fmt.Errorf("%w: unsupported concrete type %T", ErrInvalidMessage, message)
	}
}

// SplitSystem validates messages and separates the leading system prefix from
// the provider conversation. Returned slices are shallow projections intended
// for immediate read-only adapter use.
func (messages Messages) SplitSystem() ([]SystemMessage, Messages, error) {
	if err := messages.Validate(); err != nil {
		return nil, nil, err
	}

	var system []SystemMessage

	index := 0
	for index < len(messages) {
		message, ok := messages[index].(SystemMessage)
		if !ok {
			break
		}

		system = append(system, message)
		index++
	}

	conversation := append(Messages(nil), messages[index:]...)

	return system, conversation, nil
}

// validateResultContentKind enforces the field rules of the tool-result-only
// kinds: structured content must be a JSON object, links require a URI, and
// embedded resources require a URI plus a MIME type for blob bodies.
func validateResultContentKind(index int, part Part) error {
	switch part := part.(type) {
	case StructuredContentPart:
		if err := validateStructuredObject(part.Data); err != nil {
			return fmt.Errorf("part %d structured content: %w", index, err)
		}
	case ResourceLinkPart:
		if part.URI == "" {
			return fmt.Errorf("part %d resource link: uri is required", index)
		}
	case EmbeddedResourcePart:
		if part.URI == "" {
			return fmt.Errorf("part %d embedded resource: uri is required", index)
		}

		if part.Blob != nil && part.MIMEType == "" {
			return fmt.Errorf("part %d embedded resource: mime type is required for blob resources", index)
		}
	default:
		return fmt.Errorf("part %d has unsupported concrete type %T", index, part)
	}

	return nil
}

func invalidMessagePart(role string, index int, part any) error {
	return fmt.Errorf("%w: %s part %d has unsupported concrete type %T", ErrInvalidMessage, role, index, part)
}

func validateParts(parts []Part) error {
	return validatePartsSeen(parts, make(map[partSliceKey]bool))
}

func validatePartsSeen(parts []Part, seen map[partSliceKey]bool) error {
	if len(parts) > 0 {
		key := partSliceKey{data: reflect.ValueOf(parts).Pointer(), len: len(parts), cap: cap(parts)}
		if seen[key] {
			return nil
		}

		seen[key] = true
	}

	for i, part := range parts {
		switch part := part.(type) {
		case TextPart, ImagePart, FilePart, ReasoningPart, ToolCallPart:
		case StructuredContentPart, ResourceLinkPart, EmbeddedResourcePart:
			if err := validateResultContentKind(i, part); err != nil {
				return err
			}
		case ToolResultPart:
			if err := validatePartsSeen(part.Content, seen); err != nil {
				return fmt.Errorf("part %d tool result: %w", i, err)
			}
		default:
			return fmt.Errorf("part %d has unsupported concrete type %T", i, part)
		}
	}

	return nil
}
