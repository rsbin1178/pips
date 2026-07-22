//nolint:wsl_v5 // Deep-copy steps intentionally stay adjacent by payload field.
package harness

import (
	"maps"
	"slices"

	"github.com/rsbin/pips/ai"
)

func cloneMetadata(value SessionMetadata) SessionMetadata {
	value.Extra = maps.Clone(value.Extra)

	return value
}

func cloneEntries(values []Entry) []Entry {
	cloned := make([]Entry, len(values))
	for index := range values {
		cloned[index] = cloneEntry(values[index])
	}

	return cloned
}

func cloneEntry(value Entry) Entry {
	if value.Message != nil {
		message := cloneMessage(*value.Message)
		value.Message = &message
	}
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	value.Data = slices.Clone(value.Data)

	return value
}

func cloneMessage(value ai.Message) ai.Message {
	value.Parts = cloneParts(value.Parts)

	return value
}

func cloneParts(values []ai.Part) []ai.Part {
	cloned := make([]ai.Part, len(values))
	for index, part := range values {
		switch value := part.(type) {
		case ai.ImagePart:
			value.Source.Data = slices.Clone(value.Source.Data)
			cloned[index] = value
		case ai.FilePart:
			value.Source.Data = slices.Clone(value.Source.Data)
			cloned[index] = value
		case ai.ToolCallPart:
			value.Args = slices.Clone(value.Args)
			cloned[index] = value
		case ai.ToolResultPart:
			value.Content = cloneParts(value.Content)
			cloned[index] = value
		default:
			cloned[index] = value
		}
	}

	return cloned
}
