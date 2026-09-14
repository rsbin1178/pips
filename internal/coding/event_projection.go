//nolint:wsl_v5 // Privacy projection steps intentionally stay adjacent per payload.
package coding

import (
	"encoding/json"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

// planToolName maps one plan decision kind onto its tool name.
func planToolName(kind planreview.Kind) string {
	if kind == planreview.KindEnter {
		return planmode.EnterToolName
	}

	return planmode.ExitToolName
}

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
		value.Update = nil

		return value
	case ToolCompleted:
		value.Call.Arguments = nil
		if result, ok := safeMessage(value.Result).(ai.ToolMessage); ok {
			value.Result = result
		}

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
	case QuestionRequired:
		value.Request.Questions = nil
		value.Request.Prompt = ""
		value.Redacted = true
		return value
	case QuestionResolved:
		value.Resolution.Answers = nil
		value.Resolution.Chat = ""
		value.Redacted = true
		return value
	case PlanReviewRequired:
		value.Request.Content = ""
		value.Request.HasContent = false
		value.Request.Size = 0
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
		value.Tasks.Items = nil

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
	switch message := message.(type) {
	case ai.SystemMessage:
		return ai.SystemMessage{}
	case ai.UserMessage:
		return ai.UserMessage{}
	case ai.AssistantMessage:
		safe := ai.AssistantMessage{}
		for _, part := range message.Parts {
			switch value := part.(type) {
			case ai.TextPart:
				safe.Parts = append(safe.Parts, value)
			case ai.ReasoningPart:
				safe.Parts = append(safe.Parts, ai.ReasoningPart{Redacted: true})
			case ai.ToolCallPart:
				value.Args = nil
				safe.Parts = append(safe.Parts, value)
			}
		}

		return safe
	case ai.ToolMessage:
		safe := ai.ToolMessage{Parts: make([]ai.ToolResultPart, len(message.Parts))}
		for index, result := range message.Parts {
			result.Content = nil
			safe.Parts[index] = result
		}

		return safe
	default:
		return message
	}
}

// TelemetrySignal identifies a content-free observation that has no durable
// Coding event, such as a rejection before child Session creation.
type TelemetrySignal string

const (
	// TelemetrySignalSubagentAdmission observes a shared-budget decision.
	TelemetrySignalSubagentAdmission TelemetrySignal = "subagent.admission"
)

// TelemetryEvent is a low-cardinality, content-free observation derived from
// one Coding event or an explicitly non-durable Runtime signal. It never
// contains session, interaction, run, request, or provider-response identifiers.
type TelemetryEvent struct {
	Signal                  TelemetrySignal           `json:"signal,omitempty"`
	Type                    EventType                 `json:"type,omitempty"`
	Time                    time.Time                 `json:"time"`
	Provider                ai.Provider               `json:"provider,omitempty"`
	ModelID                 string                    `json:"model_id,omitempty"`
	Agent                   string                    `json:"agent,omitempty"`
	SubagentRole            string                    `json:"subagent_role,omitempty"`
	SubagentState           string                    `json:"subagent_state,omitempty"`
	SubagentDelivery        subagent.Delivery         `json:"subagent_delivery,omitempty"`
	SubagentDepth           int                       `json:"subagent_depth,omitempty"`
	AdmissionOutcome        subagent.AdmissionOutcome `json:"admission_outcome,omitempty"`
	AdmissionReason         subagent.AdmissionReason  `json:"admission_reason,omitempty"`
	TeamScope               string                    `json:"team_scope,omitempty"`
	TeamState               string                    `json:"team_state,omitempty"`
	TeamActivity            string                    `json:"team_activity,omitempty"`
	TeamControlAction       string                    `json:"team_control_action,omitempty"`
	TeamControlState        string                    `json:"team_control_state,omitempty"`
	IntegrationState        string                    `json:"integration_state,omitempty"`
	IntegrationVerification string                    `json:"integration_verification,omitempty"`
	Tool                    string                    `json:"tool,omitempty"`
	ToolCalls               int                       `json:"tool_calls,omitempty"`
	Stop                    agent.StopReason          `json:"stop,omitempty"`
	Phase                   Phase                     `json:"phase,omitempty"`
	Mode                    OperatingMode             `json:"mode,omitempty"`
	Outcome                 InteractionOutcome        `json:"outcome,omitempty"`
	Component               string                    `json:"component,omitempty"`
	Code                    string                    `json:"code,omitempty"`
	Turns                   int                       `json:"turns,omitempty"`
	Changes                 int                       `json:"changes,omitempty"`
	Attempts                int                       `json:"attempts,omitempty"`
	Files                   int                       `json:"files,omitempty"`
	Nodes                   int                       `json:"nodes,omitempty"`
	Questions               int                       `json:"questions,omitempty"`
	Answers                 int                       `json:"answers,omitempty"`
	Chat                    bool                      `json:"chat,omitempty"`
	PlanBytes               int64                     `json:"plan_bytes,omitempty"`
	PlanDecision            string                    `json:"plan_decision,omitempty"`
	CompactionMode          CompactionMode            `json:"compaction_mode,omitempty"`
	TokensBefore            int                       `json:"tokens_before,omitempty"`
	TokensAfter             int                       `json:"tokens_after,omitempty"`
	DurationMillis          int64                     `json:"duration_ms,omitempty"`
	Usage                   TokenUsage                `json:"usage"`
	Resumed                 bool                      `json:"resumed,omitempty"`
	Failed                  bool                      `json:"failed,omitempty"`
}

// Telemetry projects one validated event into content-free observability data.
//
//nolint:funlen,gocyclo,cyclop // The telemetry table is deliberately exhaustive.
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
		projected.Mode = value.Mode
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
	case ModeChanged:
		projected.Mode = value.Mode
	case InteractionStarted:
		projected.Resumed = value.Resumed
		projected.Code = string(value.Source)
		projected.Mode = value.Mode
	case InteractionCompleted:
		projected.Outcome = value.Outcome
		projected.Stop = value.Stop
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
	case TeamLifecycle:
		projectTeamTelemetry(&projected, value)
	case TeamControlLifecycle:
		projected.Agent = "team_control"
		projected.TeamControlAction = string(value.Action)
		projected.TeamControlState = string(value.State)
		projected.Code = value.Code
		projected.Failed = value.State == TeamControlRejected ||
			value.State == TeamControlStale || value.State == TeamControlDeliveryUnknown
	case TeamIntegrationLifecycle:
		projected.Agent = "team_integration"
		projected.IntegrationState = string(value.State)
		projected.IntegrationVerification = value.VerificationState
		projected.Attempts = value.Attempts
		projected.Files = value.Files
		projected.Changes = value.Files
		projected.Code = value.Code
		projected.Failed = value.State == TeamIntegrationConflict ||
			value.State == TeamIntegrationVerificationFailed ||
			value.State == TeamIntegrationInterrupted
	case ApprovalRequired:
		projected.Tool = value.Tool
	case ApprovalUnknown:
		projected.Tool = value.Tool
		projected.Code = "outcome_unknown"
	case ApprovalResolved:
		projected.Code = string(value.Choice)
	case QuestionRequired:
		projected.Questions = value.Count
	case QuestionResolved:
		projected.Answers = value.AnswerCount
		projected.Chat = value.Chat
	case QuestionRejected:
		projected.Code = "rejected"
	case PlanReviewRequired:
		projected.Tool = planToolName(value.Request.Kind)
		projected.PlanBytes = value.Request.Size
	case PlanReviewResolved:
		projected.Tool = planToolName(value.Kind)
		projected.PlanDecision = string(value.Decision)
	case WorkspaceChanged:
		projected.Changes = value.Files
		if projected.Changes == 0 {
			projected.Changes = len(value.Entries)
		}
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

func projectTeamTelemetry(projected *TelemetryEvent, value TeamLifecycle) {
	projected.Agent = "team_worker"
	projected.TeamScope = "team"
	if value.AttemptID != "" {
		projected.TeamScope = "attempt"
	}
	projected.TeamState = string(value.State)
	projected.TeamActivity = string(value.Activity)
	projected.Code = value.Code
	projected.Turns = value.Turns
	projected.ToolCalls = value.ToolCalls
	projected.DurationMillis = value.DurationMillis
	projected.Usage = value.Usage
	projected.Failed = value.State == TeamLifecycleFailed ||
		value.State == TeamLifecycleCancelled ||
		value.State == TeamLifecycleInterrupted
}

func projectSubagentTelemetry(
	projected *TelemetryEvent,
	eventType EventType,
	value SubagentLifecycle,
) {
	projected.SubagentRole = string(value.Role)
	projected.SubagentState = string(value.State)
	projected.SubagentDelivery = value.Delivery
	// Profile IDs are unbounded user input and would create high-cardinality
	// telemetry dimensions. Keep the legacy builtin role when available and
	// otherwise use only the fixed durable identity kind.
	if value.Role != "" {
		projected.Agent = "subagent/" + string(value.Role)
	} else {
		projected.Agent = "subagent/" + string(value.Identity.Kind)
	}
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

func toolMessageFailed(message ai.ToolMessage) bool {
	for _, result := range message.Parts {
		if result.IsError {
			return true
		}
	}

	return false
}

var (
	_ json.Marshaler   = ExportEvent{}
	_ json.Unmarshaler = (*ExportEvent)(nil)
)
