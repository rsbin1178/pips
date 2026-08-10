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
		value.Message = cloneMessage(value.Message)
	}
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	value.Data = slices.Clone(value.Data)

	return value
}

func cloneMessage(value ai.Message) ai.Message {
	cloned, err := ai.CloneMessage(value)
	if err != nil {
		return value
	}

	return cloned
}
