//nolint:wsl_v5 // Strict nested decode steps intentionally stay adjacent.
package coding

import (
	"encoding/json"
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/jsonx"
)

const maxEncodedEventBytes = 4 << 20

type eventEnvelope struct {
	Schema        string          `json:"schema"`
	Sequence      uint64          `json:"sequence"`
	Time          json.RawMessage `json:"time"`
	SessionID     string          `json:"session_id"`
	InteractionID string          `json:"interaction_id,omitempty"`
	RunID         string          `json:"run_id,omitempty"`
	Type          EventType       `json:"type"`
	Payload       json.RawMessage `json:"payload"`
}

// MarshalEvent performs explicit full-content serialization for trusted local
// round trips. Persistence and process boundaries should marshal [ExportEvent].
func MarshalEvent(event Event) ([]byte, error) {
	if err := ValidateEvent(event); err != nil {
		return nil, err
	}

	return marshalEventEnvelope(event)
}

// UnmarshalEvent strictly decodes one complete Coding event object.
func UnmarshalEvent(data []byte) (Event, error) {
	if len(data) == 0 || len(data) > maxEncodedEventBytes {
		return Event{}, invalidEvent("encoded size is outside limits")
	}

	var raw struct {
		Schema        string          `json:"schema"`
		Sequence      uint64          `json:"sequence"`
		Time          json.RawMessage `json:"time"`
		SessionID     string          `json:"session_id"`
		InteractionID string          `json:"interaction_id,omitempty"`
		RunID         string          `json:"run_id,omitempty"`
		Type          EventType       `json:"type"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := strictDecode(data, &raw); err != nil {
		return Event{}, fmt.Errorf("%w: decode envelope: %w", ErrInvalidEvent, err)
	}

	var event Event

	event.Schema = raw.Schema
	event.Sequence = raw.Sequence
	event.SessionID = raw.SessionID
	event.InteractionID = raw.InteractionID
	event.RunID = raw.RunID
	event.Type = raw.Type

	if err := strictDecode(raw.Time, &event.Time); err != nil {
		return Event{}, fmt.Errorf("%w: decode time: %w", ErrInvalidEvent, err)
	}

	payload, err := decodeEventPayload(raw.Type, raw.Payload)
	if err != nil {
		return Event{}, err
	}

	event.Payload = payload
	if err := ValidateEvent(event); err != nil {
		return Event{}, err
	}

	return cloneEvent(event), nil
}

func marshalEventEnvelope(event Event) ([]byte, error) {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: encode payload: %w", ErrInvalidEvent, err)
	}

	encodedTime, err := json.Marshal(event.Time)
	if err != nil {
		return nil, fmt.Errorf("%w: encode time: %w", ErrInvalidEvent, err)
	}

	encoded, err := json.Marshal(eventEnvelope{
		Schema:        event.Schema,
		Sequence:      event.Sequence,
		Time:          encodedTime,
		SessionID:     event.SessionID,
		InteractionID: event.InteractionID,
		RunID:         event.RunID,
		Type:          event.Type,
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode envelope: %w", ErrInvalidEvent, err)
	}

	if len(encoded) > maxEncodedEventBytes {
		return nil, invalidEvent("encoded size is outside limits")
	}

	return encoded, nil
}

//nolint:gocyclo,cyclop,maintidx // Strict decoding enumerates every closed payload variant.
func decodeEventPayload(eventType EventType, data []byte) (EventPayload, error) {
	if len(data) == 0 {
		return nil, invalidEvent("payload is required")
	}

	if err := validateNestedPayloadJSON(eventType, data); err != nil {
		return nil, fmt.Errorf("%w: decode nested payload: %w", ErrInvalidEvent, err)
	}

	switch eventType {
	case EventSessionOpened:
		return decodePayload[SessionOpened](data)
	case EventSessionClosed:
		return decodePayload[SessionClosed](data)
	case EventSessionTreeChanged:
		return decodePayload[SessionTreeChanged](data)
	case EventSessionNavigated:
		return decodePayload[SessionNavigated](data)
	case EventSessionForked:
		return decodePayload[SessionForked](data)
	case EventCompactionStarted:
		return decodePayload[CompactionStarted](data)
	case EventCompactionCompleted:
		return decodePayload[CompactionCompleted](data)
	case EventModeChanged:
		return decodePayload[ModeChanged](data)
	case EventInteractionStarted:
		return decodePayload[InteractionStarted](data)
	case EventInteractionCompleted:
		return decodePayload[InteractionCompleted](data)
	case EventRunStarted:
		return decodePayload[RunStarted](data)
	case EventRunCompleted:
		return decodePayload[RunCompleted](data)
	case EventTurnStarted:
		return decodePayload[TurnStarted](data)
	case EventTurnCompleted:
		return decodePayload[TurnCompleted](data)
	case EventMessageCommitted:
		return decodePayload[MessageCommitted](data)
	case EventMessageDelta:
		return decodePayload[MessageDelta](data)
	case EventMessageDiscarded:
		return decodePayload[MessageDiscarded](data)
	case EventToolStarted:
		return decodePayload[ToolStarted](data)
	case EventToolUpdated:
		return decodePayload[ToolUpdated](data)
	case EventToolCompleted:
		return decodePayload[ToolCompleted](data)
	case EventSubagentCreated, EventSubagentStarted, EventSubagentProgress,
		EventSubagentCompleted, EventSubagentFailed, EventSubagentCanceled,
		EventSubagentInterrupted:
		return decodePayload[SubagentLifecycle](data)
	case EventTeamLifecycle:
		return decodePayload[TeamLifecycle](data)
	case EventTeamControlLifecycle:
		return decodePayload[TeamControlLifecycle](data)
	case EventTeamIntegrationLifecycle:
		return decodePayload[TeamIntegrationLifecycle](data)
	case EventApprovalRequired:
		return decodePayload[ApprovalRequired](data)
	case EventApprovalUnknown:
		return decodePayload[ApprovalUnknown](data)
	case EventApprovalResolved:
		return decodePayload[ApprovalResolved](data)
	case EventQuestionRequired:
		return decodePayload[QuestionRequired](data)
	case EventQuestionResolved:
		return decodePayload[QuestionResolved](data)
	case EventQuestionRejected:
		return decodePayload[QuestionRejected](data)
	case EventPlanReviewRequired:
		return decodePayload[PlanReviewRequired](data)
	case EventPlanReviewResolved:
		return decodePayload[PlanReviewResolved](data)
	case EventWorkspaceChanged:
		return decodePayload[WorkspaceChanged](data)
	case EventStatusChanged:
		return decodePayload[StatusChanged](data)
	case EventIntegrationDiagnostic:
		return decodePayload[IntegrationDiagnostic](data)
	case EventError:
		return decodePayload[RuntimeError](data)
	default:
		return nil, invalidEvent("unknown type %q", eventType)
	}
}

func decodePayload[T EventPayload](data []byte) (EventPayload, error) {
	var payload T
	if err := strictDecode(data, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode payload: %w", ErrInvalidEvent, err)
	}

	return payload, nil
}

func strictDecode(data []byte, target any) error {
	return jsonx.Decode(data, target)
}

func validateNestedPayloadJSON(eventType EventType, data []byte) error {
	switch eventType {
	case EventSessionTreeChanged:
		var payload struct {
			Tree       SessionTree       `json:"tree"`
			Transcript []json.RawMessage `json:"transcript"`
		}
		if err := strictDecode(data, &payload); err != nil {
			return err
		}
		for _, message := range payload.Transcript {
			if err := validateStrictMessageJSON(message); err != nil {
				return err
			}
		}

		return nil
	case EventMessageCommitted:
		var payload struct {
			Message   json.RawMessage `json:"message"`
			Synthetic bool            `json:"synthetic,omitempty"`
		}
		if err := strictDecode(data, &payload); err != nil {
			return err
		}

		return validateStrictMessageJSON(payload.Message)
	case EventToolUpdated:
		var payload struct {
			Turn   int             `json:"turn"`
			Call   ToolCall        `json:"call"`
			Update json.RawMessage `json:"update"`
		}
		if err := strictDecode(data, &payload); err != nil {
			return err
		}

		return validateStrictMessageJSON(payload.Update)
	case EventToolCompleted:
		var payload struct {
			Turn   int             `json:"turn"`
			Call   ToolCall        `json:"call"`
			Result json.RawMessage `json:"result"`
		}
		if err := strictDecode(data, &payload); err != nil {
			return err
		}

		return validateStrictMessageJSON(payload.Result)
	default:
		return nil
	}
}

func validateStrictMessageJSON(data []byte) error {
	var message struct {
		Role  ai.Role           `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := strictDecode(data, &message); err != nil {
		return err
	}

	for _, part := range message.Parts {
		if err := validateStrictPartJSON(part); err != nil {
			return err
		}
	}

	return nil
}

//nolint:cyclop // The ai.Part wire taxonomy is exhaustively checked before its custom decoder.
func validateStrictPartJSON(data []byte) error {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}

	switch header.Type {
	case "text":
		var part struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}

		return strictDecode(data, &part)
	case "image":
		var part struct {
			Type   string          `json:"type"`
			Source json.RawMessage `json:"source"`
		}
		if err := strictDecode(data, &part); err != nil {
			return err
		}

		return validateStrictMediaSourceJSON(part.Source)
	case "file":
		var part struct {
			Type   string          `json:"type"`
			Source json.RawMessage `json:"source"`
			Name   string          `json:"name,omitempty"`
		}
		if err := strictDecode(data, &part); err != nil {
			return err
		}

		return validateStrictMediaSourceJSON(part.Source)
	case "reasoning":
		var part struct {
			Type      string `json:"type"`
			Text      string `json:"text,omitempty"`
			Signature string `json:"signature,omitempty"`
			Redacted  bool   `json:"redacted,omitempty"`
		}

		return strictDecode(data, &part)
	case "tool_call":
		var part struct {
			Type      string          `json:"type"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Arguments json.RawMessage `json:"args,omitempty"`
		}

		return strictDecode(data, &part)
	case "tool_result":
		var part struct {
			Type       string            `json:"type"`
			ToolCallID string            `json:"tool_call_id,omitempty"`
			Name       string            `json:"name,omitempty"`
			Content    []json.RawMessage `json:"content,omitempty"`
			IsError    bool              `json:"is_error,omitempty"`
		}
		if err := strictDecode(data, &part); err != nil {
			return err
		}

		for _, content := range part.Content {
			if err := validateStrictPartJSON(content); err != nil {
				return err
			}
		}

		return nil
	default:
		return fmt.Errorf("unknown message part type %q", header.Type)
	}
}

func validateStrictMediaSourceJSON(data []byte) error {
	var source struct {
		ID       string `json:"id,omitempty"`
		URL      string `json:"url,omitempty"`
		Data     []byte `json:"data,omitempty"`
		MIMEType string `json:"mime_type,omitempty"`
	}

	return strictDecode(data, &source)
}
