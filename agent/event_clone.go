package agent

import (
	"reflect"
	"slices"

	"github.com/rsbin1178/pips/ai"
)

// partSliceKey identifies one concrete view of a Part slice. The memo keeps
// cloning finite even if a caller constructs cyclic ToolResult content.
type partSliceKey struct {
	data uintptr
	len  int
	cap  int
}

func cloneEvent(event Event) Event {
	switch payload := event.payload.(type) {
	case RunStarted, TurnStarted, CandidateDiscarded, TurnCompleted, RunCompleted:
		return event
	case ModelStreamEvent:
		if payload.Event.Usage == nil {
			return event
		}
	}

	event.payload = cloneEventPayload(event.payload)

	return event
}

func cloneEventPayload(payload EventPayload) EventPayload {
	switch value := payload.(type) {
	case ModelStreamEvent:
		if value.Event.Usage == nil {
			return payload
		}

		usage := *value.Event.Usage
		value.Event.Usage = &usage

		return value
	case MessageCommitted:
		value.Message = cloneEventMessage(value.Message)
		return value
	case ToolStarted:
		value.Call = cloneEventToolCall(value.Call)
		return value
	case ToolUpdated:
		memo := make(map[partSliceKey][]ai.Part)
		value.Call = cloneEventToolCall(value.Call)
		value.Update = cloneEventParts(value.Update, memo)

		return value
	case ToolCompleted:
		memo := make(map[partSliceKey][]ai.Part)
		value.Call = cloneEventToolCall(value.Call)
		value.Result.Content = cloneEventParts(value.Result.Content, memo)

		return value
	default:
		return value
	}
}

func cloneEventMessage(message ai.Message) ai.Message {
	cloned, err := ai.CloneMessage(message)
	if err != nil {
		return message
	}

	return cloned
}

func cloneEventToolCall(call ai.ToolCallPart) ai.ToolCallPart {
	call.Args = slices.Clone(call.Args)

	return call
}

func cloneEventParts(parts []ai.Part, memo map[partSliceKey][]ai.Part) []ai.Part {
	if parts == nil {
		return nil
	}

	if len(parts) == 0 {
		return []ai.Part{}
	}

	key := partSliceKey{
		data: reflect.ValueOf(parts).Pointer(),
		len:  len(parts),
		cap:  cap(parts),
	}
	if cloned, ok := memo[key]; ok {
		return cloned
	}

	cloned := make([]ai.Part, len(parts))
	memo[key] = cloned

	for index, part := range parts {
		switch value := part.(type) {
		case ai.ImagePart:
			value.Source.Data = slices.Clone(value.Source.Data)
			cloned[index] = value
		case ai.FilePart:
			value.Source.Data = slices.Clone(value.Source.Data)
			cloned[index] = value
		case ai.ToolCallPart:
			cloned[index] = cloneEventToolCall(value)
		case ai.ToolResultPart:
			value.Content = cloneEventParts(value.Content, memo)
			cloned[index] = value
		case ai.StructuredContentPart:
			value.Data = slices.Clone(value.Data)
			cloned[index] = value
		case ai.EmbeddedResourcePart:
			value.Blob = slices.Clone(value.Blob)
			cloned[index] = value
		default:
			cloned[index] = value
		}
	}

	return cloned
}
