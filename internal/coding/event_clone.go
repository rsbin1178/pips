//nolint:wsl_v5 // Closed payload copy branches intentionally stay compact.
package coding

import (
	"slices"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
)

func cloneEvent(event Event) Event {
	event.Payload = cloneEventPayload(event.Payload)

	return event
}

//nolint:gocyclo,cyclop,funlen // The sealed payload taxonomy has one explicit copy branch per variant.
func cloneEventPayload(payload EventPayload) EventPayload {
	switch value := payload.(type) {
	case SessionOpened:
		return value
	case SessionClosed:
		return value
	case SessionTreeChanged:
		value.Tree = value.Tree.Clone()
		value.Transcript = cloneMessages(value.Transcript)
		value.Tasks = value.Tasks.Clone()
		return value
	case SessionNavigated:
		return value
	case SessionForked:
		return value
	case CompactionStarted:
		return value
	case CompactionCompleted:
		return value
	case ModeChanged:
		return value
	case InteractionStarted:
		value.NotificationIDs = slices.Clone(value.NotificationIDs)
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
	case MessageDiscarded:
		return value
	case ToolStarted:
		value.Call = cloneToolCall(value.Call)
		return value
	case ToolUpdated:
		value.Call = cloneToolCall(value.Call)
		value.Update, _ = ai.CloneParts(value.Update)

		return value
	case ToolCompleted:
		value.Call = cloneToolCall(value.Call)
		cloned, err := ai.CloneMessage(value.Result)
		if err == nil {
			if result, ok := cloned.(ai.ToolMessage); ok {
				value.Result = result
			}
		}

		return value
	case SubagentLifecycle:
		return value
	case TeamLifecycle:
		return value
	case TeamControlLifecycle:
		return value
	case TeamIntegrationLifecycle:
		return value
	case ApprovalRequired:
		return cloneApprovalRequired(value)
	case ApprovalUnknown:
		return cloneApprovalUnknown(value)
	case ApprovalResolved:
		return value
	case QuestionRequired:
		value.Request = question.CloneRequest(value.Request)
		return value
	case QuestionResolved:
		value.Resolution = question.CloneResolution(value.Resolution)
		return value
	case QuestionRejected:
		return value
	case PlanReviewRequired:
		value.Request = planreview.CloneRequest(value.Request)
		return value
	case PlanReviewResolved:
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
	cloned, err := ai.CloneMessage(message)
	if err != nil {
		return message
	}

	return cloned
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

func toolResultMessage(result ai.ToolResultPart) ai.ToolMessage {
	cloned, err := ai.CloneParts([]ai.Part{result})
	if err != nil {
		return ai.ToolResults(result)
	}

	part, ok := cloned[0].(ai.ToolResultPart)
	if !ok {
		return ai.ToolResults(result)
	}

	return ai.ToolResults(part)
}

func toolUpdateMessage(parts []ai.Part) []ai.Part {
	cloned, _ := ai.CloneParts(parts)

	return cloned
}
