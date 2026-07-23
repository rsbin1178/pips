//nolint:wsl_v5 // Closed payload copy branches intentionally stay compact.
package coding

import (
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

func cloneEvent(event Event) Event {
	event.Payload = cloneEventPayload(event.Payload)

	return event
}

//nolint:gocyclo,cyclop // The sealed payload taxonomy has one explicit copy branch per variant.
func cloneEventPayload(payload EventPayload) EventPayload {
	switch value := payload.(type) {
	case SessionOpened:
		return value
	case SessionClosed:
		return value
	case SessionTreeChanged:
		value.Tree = value.Tree.Clone()
		value.Transcript = cloneMessages(value.Transcript)
		return value
	case SessionNavigated:
		return value
	case SessionForked:
		return value
	case CompactionStarted:
		return value
	case CompactionCompleted:
		return value
	case InteractionStarted:
		return value
	case InteractionCompleted:
		return value
	case RunStarted:
		return value
	case RunCompleted:
		return value
	case TurnStarted:
		return value
	case TurnCompleted:
		return value
	case MessageCommitted:
		value.Message = cloneMessage(value.Message)
		return value
	case MessageDelta:
		return cloneMessageDelta(value)
	case ToolStarted:
		value.Call = cloneToolCall(value.Call)
		return value
	case ToolUpdated:
		value.Call = cloneToolCall(value.Call)
		value.Update = cloneMessage(value.Update)

		return value
	case ToolCompleted:
		value.Call = cloneToolCall(value.Call)
		value.Result = cloneMessage(value.Result)

		return value
	case SubagentLifecycle:
		return value
	case ApprovalRequired:
		return cloneApprovalRequired(value)
	case ApprovalUnknown:
		return cloneApprovalUnknown(value)
	case ApprovalResolved:
		return value
	case WorkspaceChanged:
		return cloneWorkspaceChanged(value)
	case StatusChanged:
		return value
	case IntegrationDiagnostic:
		return value
	case RuntimeError:
		return value
	default:
		return payload
	}
}

func cloneToolCall(call ToolCall) ToolCall {
	call.Arguments = slices.Clone(call.Arguments)

	return call
}

func cloneMessageDelta(delta MessageDelta) MessageDelta {
	if delta.Usage != nil {
		usage := *delta.Usage
		delta.Usage = &usage
	}

	return delta
}

func cloneApprovalRequired(value ApprovalRequired) ApprovalRequired {
	value.Command = slices.Clone(value.Command)
	value.Choices = slices.Clone(value.Choices)

	return value
}

func cloneApprovalUnknown(value ApprovalUnknown) ApprovalUnknown {
	value.Choices = slices.Clone(value.Choices)

	return value
}

func cloneWorkspaceChanged(value WorkspaceChanged) WorkspaceChanged {
	value.Entries = slices.Clone(value.Entries)

	return value
}

func cloneMessage(message ai.Message) ai.Message {
	message.Parts = cloneParts(message.Parts)

	return message
}

func cloneParts(parts []ai.Part) []ai.Part {
	cloned := make([]ai.Part, len(parts))
	for index, part := range parts {
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

func tokenUsageFromAI(usage ai.Usage) TokenUsage {
	return TokenUsage{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		ReasoningTokens:   usage.ReasoningTokens,
		CachedInputTokens: usage.CachedInputTokens,
		CacheWriteTokens:  usage.CacheWriteTokens,
	}
}

func messageDeltaFromAI(event ai.StreamEvent) MessageDelta {
	delta := MessageDelta{
		Kind:          event.Type,
		Provider:      event.Provider,
		ResponseID:    event.ID,
		Model:         event.Model,
		Text:          event.Text,
		Signature:     event.Signature,
		ToolCallIndex: event.ToolCallIndex,
		ToolCallID:    event.ToolCallID,
		ToolCallName:  event.ToolCallName,
		Arguments:     event.ArgsDelta,
		FinishReason:  event.FinishReason,
	}
	if event.Usage != nil {
		usage := tokenUsageFromAI(*event.Usage)
		delta.Usage = &usage
	}

	return delta
}

func toolCallFromAI(call ai.ToolCallPart) ToolCall {
	return ToolCall{ID: call.ID, Name: call.Name, Arguments: slices.Clone(call.Args)}
}

func toolResultMessage(result ai.ToolResultPart) ai.Message {
	return ai.Message{Role: ai.RoleTool, Parts: []ai.Part{cloneParts([]ai.Part{result})[0]}}
}

func toolUpdateMessage(parts []ai.Part) ai.Message {
	return ai.Message{Role: ai.RoleTool, Parts: cloneParts(parts)}
}

func validAgentEventType(eventType agent.EventType) bool {
	switch eventType {
	case agent.EventRunStart, agent.EventTurnStart, agent.EventDelta, agent.EventMessage,
		agent.EventToolStart, agent.EventToolUpdate, agent.EventToolEnd,
		agent.EventTurnEnd, agent.EventRunEnd:
		return true
	default:
		return false
	}
}
