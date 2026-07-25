//nolint:wsl_v5 // Privacy projection steps intentionally stay adjacent per payload.
package coding

import (
	"encoding/json"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

// Disclosure selects the content retained at a serialization boundary.
type Disclosure uint8

// Disclosure policies. Safe is the default for JSONL and non-interactive use.
const (
	DisclosureSafe Disclosure = iota
	DisclosureContent
)

// ExportEvent is an event that has passed an explicit disclosure projection.
type ExportEvent struct {
	event Event
}

// Event returns a defensive copy of the projected event.
func (e ExportEvent) Event() Event {
	return cloneEvent(e.event)
}

// MarshalJSON serializes the already-projected event.
func (e ExportEvent) MarshalJSON() ([]byte, error) {
	if err := ValidateEvent(e.event); err != nil {
		return nil, err
	}

	return marshalEventEnvelope(e.event)
}

// UnmarshalJSON strictly decodes an exported event.
func (e *ExportEvent) UnmarshalJSON(data []byte) error {
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		return err
	}

	e.event = decoded

	return nil
}

// Project applies an explicit disclosure policy and returns a defensive event.
func Project(event Event, disclosure Disclosure) (ExportEvent, error) {
	if err := ValidateEvent(event); err != nil {
		return ExportEvent{}, err
	}

	projected := cloneEvent(event)

	switch disclosure {
	case DisclosureSafe:
		projected.Payload = projectSafePayload(projected.Payload)
	case DisclosureContent:
	default:
		return ExportEvent{}, invalidEvent("unknown disclosure %d", disclosure)
	}

	if err := ValidateEvent(projected); err != nil {
		return ExportEvent{}, err
	}

	return ExportEvent{event: projected}, nil
}

//nolint:gocyclo,cyclop // The privacy projection exhaustively handles the sealed payload taxonomy.
func projectSafePayload(payload EventPayload) EventPayload {
	switch value := payload.(type) {
	case MessageCommitted:
		value.Message = safeMessage(value.Message)
		return value
	case MessageDelta:
		value.ResponseID = ""
		switch value.Kind {
		case ai.StreamReasoningDelta:
			value.Text = ""
			value.Signature = ""
		case ai.StreamToolCallDelta:
			value.Arguments = ""
		case ai.StreamMessageStart, ai.StreamTextDelta, ai.StreamToolCallStart,
			ai.StreamToolCallEnd, ai.StreamMessageEnd:
		}

		return value
	case ToolStarted:
		value.Call.Arguments = nil
		return value
	case ToolUpdated:
		value.Call.Arguments = nil
		value.Update = safeMessage(value.Update)

		return value
	case ToolCompleted:
		value.Call.Arguments = nil
		value.Result = safeMessage(value.Result)

		return value
	case ApprovalRequired:
		value.Command = nil
		value.CWD = ""
		value.Justification = ""

		return value
	case ApprovalUnknown:
		value.Fingerprint = "redacted"
		value.Reason = ""

		return value
	case WorkspaceChanged:
		value.Diff = ""
		return value
	case SubagentLifecycle:
		value.TaskPreview = ""
		value.Activity.Action = ""
		value.Activity.Target = ""
		return value
	case SessionTreeChanged:
		value.Tree.Name = ""
		for index := range value.Tree.Nodes {
			value.Tree.Nodes[index].Label = ""
		}
		for index := range value.Transcript {
			value.Transcript[index] = safeMessage(value.Transcript[index])
		}

		return value
	case IntegrationDiagnostic:
		value.Message = ""
		return value
	case RuntimeError:
		value.Message = ""
		return value
	default:
		return cloneEventPayload(payload)
	}
}

func safeMessage(message ai.Message) ai.Message {
	safe := ai.Message{Role: message.Role}
	if message.Role == ai.RoleUser || message.Role == ai.RoleSystem {
		return safe
	}

	for _, part := range message.Parts {
		switch value := part.(type) {
		case ai.TextPart:
			if message.Role == ai.RoleAssistant {
				safe.Parts = append(safe.Parts, value)
			}
		case ai.ReasoningPart:
			if message.Role == ai.RoleAssistant {
				safe.Parts = append(safe.Parts, ai.ReasoningPart{Redacted: true})
			}
		case ai.ToolCallPart:
			value.Args = nil
			safe.Parts = append(safe.Parts, value)
		case ai.ToolResultPart:
			value.Content = nil
			safe.Parts = append(safe.Parts, value)
		case ai.ImagePart, ai.FilePart:
			// Provider IDs, URLs, filenames, and inline data are content-bearing.
		}
	}

	return safe
}

// TelemetryEvent is a low-cardinality, content-free observation derived from
// one Coding event. It never contains session, interaction, run, request, or
// provider-response identifiers.
type TelemetryEvent struct {
	Type           EventType          `json:"type"`
	Time           time.Time          `json:"time"`
	Provider       ai.Provider        `json:"provider,omitempty"`
	ModelID        string             `json:"model_id,omitempty"`
	Agent          string             `json:"agent,omitempty"`
	SubagentRole   string             `json:"subagent_role,omitempty"`
	SubagentState  string             `json:"subagent_state,omitempty"`
	Tool           string             `json:"tool,omitempty"`
	ToolCalls      int                `json:"tool_calls,omitempty"`
	Stop           agent.StopReason   `json:"stop,omitempty"`
	Phase          Phase              `json:"phase,omitempty"`
	Outcome        InteractionOutcome `json:"outcome,omitempty"`
	Component      string             `json:"component,omitempty"`
	Code           string             `json:"code,omitempty"`
	Turns          int                `json:"turns,omitempty"`
	Changes        int                `json:"changes,omitempty"`
	Nodes          int                `json:"nodes,omitempty"`
	CompactionMode CompactionMode     `json:"compaction_mode,omitempty"`
	TokensBefore   int                `json:"tokens_before,omitempty"`
	TokensAfter    int                `json:"tokens_after,omitempty"`
	DurationMillis int64              `json:"duration_ms,omitempty"`
	Usage          TokenUsage         `json:"usage"`
	Resumed        bool               `json:"resumed,omitempty"`
	Failed         bool               `json:"failed,omitempty"`
}

// Telemetry projects one validated event into content-free observability data.
//
//nolint:gocyclo,cyclop // The telemetry table is deliberately exhaustive.
func Telemetry(event Event) (TelemetryEvent, error) {
	if err := ValidateEvent(event); err != nil {
		return TelemetryEvent{}, err
	}

	projected := TelemetryEvent{Type: event.Type, Time: event.Time}
	switch value := event.Payload.(type) {
	case SessionOpened:
		projected.Provider = value.Provider
		projected.ModelID = value.ModelID
		projected.Resumed = value.Resumed
	case SessionClosed:
		projected.Code = string(value.Reason)
	case SessionTreeChanged:
		projected.Nodes = len(value.Tree.Nodes)
	case SessionNavigated:
		projected.Code = "navigated"
	case SessionForked:
		projected.Code = "forked"
	case CompactionStarted:
		projected.CompactionMode = value.Mode
		projected.TokensBefore = value.Preview.EstimatedTokens
	case CompactionCompleted:
		projected.CompactionMode = value.Mode
		projected.TokensBefore = value.TokensBefore
		projected.TokensAfter = value.TokensAfter
		projected.DurationMillis = value.DurationMillis
	case InteractionStarted:
		projected.Resumed = value.Resumed
		projected.Code = string(value.Source)
	case InteractionCompleted:
		projected.Outcome = value.Outcome
		projected.DurationMillis = value.DurationMillis
		projected.Usage = value.Usage
		projected.Failed = value.Outcome == InteractionFailed
	case RunStarted:
		projected.Agent = value.Agent
	case RunCompleted:
		projected.Stop = value.Stop
		projected.Turns = value.Turns
		projected.Usage = value.Usage
	case TurnCompleted:
		projected.Turns = value.Turn
		projected.Usage = value.Usage
	case MessageDelta:
		projected.Provider = value.Provider

		projected.ModelID = value.Model
		if value.Usage != nil {
			projected.Usage = *value.Usage
		}
	case ToolStarted:
		projected.Tool = value.Call.Name
	case ToolUpdated:
		projected.Tool = value.Call.Name
	case ToolCompleted:
		projected.Tool = value.Call.Name
		projected.Failed = toolMessageFailed(value.Result)
	case SubagentLifecycle:
		projectSubagentTelemetry(&projected, event.Type, value)
	case ApprovalRequired:
		projected.Tool = value.Tool
	case ApprovalUnknown:
		projected.Tool = value.Tool
		projected.Code = "outcome_unknown"
	case ApprovalResolved:
		projected.Code = string(value.Choice)
	case WorkspaceChanged:
		projected.Changes = len(value.Entries)
	case StatusChanged:
		projected.Phase = value.Phase
	case IntegrationDiagnostic:
		projected.Component = value.Component
		projected.Code = value.Code
		projected.Failed = true
	case RuntimeError:
		projected.Code = value.Code
		projected.Failed = true
	case TurnStarted, MessageCommitted:
	}

	return projected, nil
}

func projectSubagentTelemetry(
	projected *TelemetryEvent,
	eventType EventType,
	value SubagentLifecycle,
) {
	projected.SubagentRole = string(value.Role)
	projected.SubagentState = string(value.State)
	projected.Agent = "subagent/" + string(value.Role)
	projected.ModelID = value.Model
	projected.Code = value.Code
	projected.Stop = value.Stop
	projected.Turns = value.Turns
	projected.ToolCalls = value.ToolCalls
	projected.DurationMillis = value.DurationMillis
	projected.Usage = value.Usage
	projected.Failed = eventType == EventSubagentFailed ||
		eventType == EventSubagentCanceled || eventType == EventSubagentInterrupted
}

func toolMessageFailed(message ai.Message) bool {
	for _, part := range message.Parts {
		if result, ok := part.(ai.ToolResultPart); ok {
			return result.IsError
		}
	}

	return false
}

var (
	_ json.Marshaler   = ExportEvent{}
	_ json.Unmarshaler = (*ExportEvent)(nil)
)
