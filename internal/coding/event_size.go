package coding

import "github.com/rsbin1178/pips/internal/coding/question"

// eventRecordBytes is the content size one replay record counts against the
// hub's byte budget. It is an estimate of the payload a consumer retains, not a
// serialized size: the envelope and fixed metadata are a small constant.
func eventRecordBytes(record EventRecord) int {
	return eventByteSize(record.Event)
}

func eventByteSize(event Event) int {
	size := 64

	switch payload := event.Payload.(type) {
	case MessageCommitted:
		size += messageByteSize(payload.Message)
	case MessageDelta:
		size += len(payload.Text) + len(payload.Signature) + len(payload.Arguments)
	case IncompleteReply:
		size += len(payload.Text)
	case ToolStarted:
		size += toolCallByteSize(payload.Call)
	case ToolUpdated:
		size += partsByteSize(payload.Update)
	case ToolCompleted:
		size += toolMessageByteSize(payload.Result)
	case QuestionRequired:
		size += questionRequestBytes(payload.Request)
	case ApprovalRequired:
		size += len(payload.Command) + len(payload.CWD) + len(payload.Justification)
		for _, choice := range payload.Choices {
			size += len(choice)
		}
	case SessionTreeChanged:
		size += messagesByteSize(payload.Transcript)
	case SubagentLifecycle:
		size += len(payload.TaskPreview) + len(payload.Model)
	case IntegrationDiagnostic:
		size += len(payload.Message) + len(payload.Code)
	case RuntimeError:
		size += len(payload.Message)
	default:
		// Lifecycle payloads are bounded counters and identifiers.
	}

	return size
}

func questionRequestBytes(request question.Request) int {
	size := len(request.Prompt)
	for _, item := range request.Questions {
		size += len(item.Header) + len(item.Question)
		for _, option := range item.Options {
			size += len(option.Label) + len(option.Description) + len(option.Preview)
		}
	}

	return size
}
