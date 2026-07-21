package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var eventTestTime = time.Date(2026, time.July, 21, 8, 30, 0, 123_000_000, time.UTC)

type eventCase struct {
	name  string
	event Event
}

func TestEventTaxonomyRoundTrip(t *testing.T) {
	t.Parallel()

	for _, test := range eventCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, ValidateEvent(test.event))

			encoded, err := MarshalEvent(test.event)
			require.NoError(t, err)

			decoded, err := UnmarshalEvent(encoded)
			require.NoError(t, err)
			assert.Equal(t, test.event, decoded)

			exported, err := Project(test.event, DisclosureSafe)
			require.NoError(t, err)

			exportedJSON, err := json.Marshal(exported)
			require.NoError(t, err)

			var decodedExport ExportEvent
			require.NoError(t, json.Unmarshal(exportedJSON, &decodedExport))
			require.NoError(t, ValidateEvent(decodedExport.Event()))

			telemetry, err := Telemetry(test.event)
			require.NoError(t, err)
			assert.Equal(t, test.event.Type, telemetry.Type)
		})
	}
}

func TestEventRejectsDirectJSONMarshal(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(eventCases()[0].event)
	require.ErrorIs(t, err, ErrUnsafeEventEncoding)
}

func TestUnmarshalEventRejectsInvalidWireValues(t *testing.T) {
	t.Parallel()

	encoded, err := MarshalEvent(eventCases()[0].event)
	require.NoError(t, err)

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown envelope field", data: strings.Replace(
			string(encoded), `,"payload":`, `,"unknown":true,"payload":`, 1,
		)},
		{name: "unknown payload field", data: strings.Replace(
			string(encoded), `"resumed":false`, `"resumed":false,"unknown":true`, 1,
		)},
		{name: "unknown schema", data: strings.Replace(
			string(encoded), EventSchema, "pips.coding.event/v9", 1,
		)},
		{name: "unknown type", data: strings.Replace(
			string(encoded), string(EventSessionOpened), "session.missing", 1,
		)},
		{name: "payload mismatch", data: strings.Replace(
			string(encoded), string(EventSessionOpened), string(EventSessionClosed), 1,
		)},
		{name: "zero sequence", data: strings.Replace(
			string(encoded), `"sequence":1`, `"sequence":0`, 1,
		)},
		{name: "trailing value", data: string(encoded) + `{}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, decodeErr := UnmarshalEvent([]byte(test.data))
			require.ErrorIs(t, decodeErr, ErrInvalidEvent)
		})
	}
}

func TestUnmarshalEventRejectsNestedUnknownAndDuplicateFields(t *testing.T) {
	t.Parallel()

	messageEvent := newTestEvent(
		EventMessageCommitted,
		MessageCommitted{Message: ai.AssistantText("done")},
	)
	encodedMessage, err := MarshalEvent(messageEvent)
	require.NoError(t, err)

	sessionEvent, err := MarshalEvent(eventCases()[0].event)
	require.NoError(t, err)

	tests := []struct {
		name string
		data string
	}{
		{
			name: "unknown message field",
			data: strings.Replace(
				string(encodedMessage),
				`"role":"assistant"`,
				`"role":"assistant","unknown":true`,
				1,
			),
		},
		{
			name: "unknown part field",
			data: strings.Replace(
				string(encodedMessage),
				`"type":"text"`,
				`"type":"text","unknown":true`,
				1,
			),
		},
		{
			name: "duplicate envelope field",
			data: strings.Replace(
				string(sessionEvent),
				`"sequence":1`,
				`"sequence":1,"sequence":1`,
				1,
			),
		},
		{
			name: "duplicate nested field",
			data: strings.Replace(
				string(encodedMessage),
				`"text":"done"`,
				`"text":"done","text":"again"`,
				1,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, decodeErr := UnmarshalEvent([]byte(test.data))
			require.ErrorIs(t, decodeErr, ErrInvalidEvent)
		})
	}
}

func TestProjectRemovesSensitiveContent(t *testing.T) {
	t.Parallel()

	secret := "secret-value-/private/workspace"
	event := newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.Assistant(
		ai.Text("answer"),
		ai.ReasoningPart{Text: secret, Signature: secret},
		ai.ToolCallPart{ID: "call-1", Name: "shell", Args: ai.JSON(`{"token":"` + secret + `"}`)},
		ai.ImageData("image/png", []byte(secret)),
	)})

	safe, err := Project(event, DisclosureSafe)
	require.NoError(t, err)

	encodedSafe, err := json.Marshal(safe)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedSafe), secret)
	assert.Contains(t, string(encodedSafe), "answer")

	content, err := Project(event, DisclosureContent)
	require.NoError(t, err)

	encodedContent, err := json.Marshal(content)
	require.NoError(t, err)
	assert.Contains(t, string(encodedContent), secret)

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encodedTelemetry, err := json.Marshal(telemetry)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedTelemetry), secret)
	assert.NotContains(t, string(encodedTelemetry), event.SessionID)
	assert.NotContains(t, string(encodedTelemetry), event.InteractionID)
	assert.NotContains(t, string(encodedTelemetry), event.RunID)
}

func TestSafeDisclosureMatrix(t *testing.T) {
	t.Parallel()

	secret := "privacy-secret"
	call := ToolCall{
		ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"value":"` + secret + `"}`),
	}
	tests := []struct {
		name     string
		event    Event
		retained string
	}{
		{
			name: "user prompt",
			event: newTestEvent(
				EventMessageCommitted,
				MessageCommitted{Message: ai.UserText(secret)},
			),
		},
		{
			name: "reasoning delta",
			event: newTestEvent(EventMessageDelta, MessageDelta{
				Kind: ai.StreamReasoningDelta, Text: secret, Signature: secret,
			}),
		},
		{
			name:  "tool arguments",
			event: newTestEvent(EventToolStarted, ToolStarted{Turn: 1, Call: call}),
		},
		{
			name: "tool progress",
			event: newTestEvent(EventToolUpdated, ToolUpdated{
				Turn: 1, Call: call,
				Update: ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.Text(secret)}},
			}),
		},
		{
			name: "tool result",
			event: newTestEvent(EventToolCompleted, ToolCompleted{
				Turn: 1, Call: call, Result: ai.ToolResultText(call.ID, call.Name, secret),
			}),
		},
		{
			name: "approval operation",
			event: newInteractionEvent(EventApprovalRequired, ApprovalRequired{
				RequestID: "request-1", CallID: call.ID, Tool: call.Name,
				Command: []string{"command", secret}, CWD: "/" + secret,
				Justification: secret,
				Choices:       []approval.Choice{approval.ChoiceAllowOnce},
			}),
		},
		{
			name: "workspace diff",
			event: newInteractionEvent(EventWorkspaceChanged, WorkspaceChanged{
				Entries: []WorkspaceChange{{Path: "main.go", Kind: changes.KindModified}},
				Diff:    secret,
			}),
			retained: "main.go",
		},
		{
			name: "diagnostic detail",
			event: newStatusEvent(EventIntegrationDiagnostic, IntegrationDiagnostic{
				Component: "mcp", Code: "connect_failed", Message: secret,
			}),
		},
		{
			name: "error detail",
			event: newStatusEvent(EventError, RuntimeError{
				Code: "runtime_failed", Message: secret,
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			exported, err := Project(test.event, DisclosureSafe)
			require.NoError(t, err)
			encoded, err := json.Marshal(exported)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), secret)

			if test.retained != "" {
				assert.Contains(t, string(encoded), test.retained)
			}

			telemetry, err := Telemetry(test.event)
			require.NoError(t, err)
			encodedTelemetry, err := json.Marshal(telemetry)
			require.NoError(t, err)
			assert.NotContains(t, string(encodedTelemetry), secret)
		})
	}
}

func TestProjectReturnsDefensiveContent(t *testing.T) {
	t.Parallel()

	arguments := ai.JSON(`{"path":"main.go"}`)
	event := newTestEvent(EventToolStarted, ToolStarted{
		Turn: 1,
		Call: ToolCall{ID: "call-1", Name: "read_file", Arguments: arguments},
	})

	projected, err := Project(event, DisclosureContent)
	require.NoError(t, err)

	arguments[2] = 'X'
	payload, ok := projected.Event().Payload.(ToolStarted)
	require.True(t, ok)
	assert.JSONEq(t, `{"path":"main.go"}`, string(payload.Call.Arguments))
}

func TestExportEventGolden(t *testing.T) {
	t.Parallel()

	event := newTestEvent(EventToolCompleted, ToolCompleted{
		Turn:   2,
		Call:   ToolCall{ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"cmd":"go test ./..."}`)},
		Result: ai.ToolResultText("call-1", "shell", "ok"),
	})
	event.Sequence = 42

	exported, err := Project(event, DisclosureSafe)
	require.NoError(t, err)

	encoded, err := json.MarshalIndent(exported, "", "  ")
	require.NoError(t, err)

	encoded = append(encoded, '\n')

	goldenPath := filepath.Join("testdata", "event.golden.json")
	want, err := os.ReadFile(goldenPath) //nolint:gosec // fixed testdata path
	require.NoError(t, err)
	assert.Equal(t, string(want), string(encoded))
}

func FuzzUnmarshalEvent(f *testing.F) {
	seed, err := MarshalEvent(eventCases()[0].event)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(seed)
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		event, decodeErr := UnmarshalEvent(data)
		if decodeErr != nil {
			return
		}

		if validateErr := ValidateEvent(event); validateErr != nil {
			t.Fatalf("decoded invalid event: %v", validateErr)
		}

		if _, encodeErr := MarshalEvent(event); encodeErr != nil {
			t.Fatalf("re-encode decoded event: %v", encodeErr)
		}
	})
}

func eventCases() []eventCase {
	toolCall := ToolCall{ID: "call-1", Name: "read_file", Arguments: ai.JSON(`{"path":"main.go"}`)}
	usage := TokenUsage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1}

	return []eventCase{
		{name: "session opened", event: newSessionEvent(EventSessionOpened, SessionOpened{
			Resumed: false, Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		})},
		{name: "session closed", event: newSessionEvent(
			EventSessionClosed, SessionClosed{Reason: SessionClosedNormally},
		)},
		{name: "interaction started", event: newInteractionEvent(
			EventInteractionStarted, InteractionStarted{Resumed: true},
		)},
		{name: "interaction completed", event: newInteractionEvent(
			EventInteractionCompleted,
			InteractionCompleted{Outcome: InteractionSucceeded, Usage: usage, DurationMillis: 250},
		)},
		{name: "run started", event: newTestEvent(
			EventRunStarted, RunStarted{Agent: "coding", ParentRunID: "parent-run"},
		)},
		{name: "run completed", event: newTestEvent(
			EventRunCompleted, RunCompleted{Stop: agent.StopEndTurn, Turns: 2, Usage: usage},
		)},
		{name: "turn started", event: newTestEvent(EventTurnStarted, TurnStarted{Turn: 1})},
		{name: "turn completed", event: newTestEvent(
			EventTurnCompleted, TurnCompleted{Turn: 1, Usage: usage},
		)},
		{name: "message committed", event: newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("done")},
		)},
		{name: "message delta", event: newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: "thinking", Signature: "signature",
		})},
		{name: "tool started", event: newTestEvent(
			EventToolStarted, ToolStarted{Turn: 1, Call: toolCall},
		)},
		{name: "tool updated", event: newTestEvent(EventToolUpdated, ToolUpdated{
			Turn: 1, Call: toolCall,
			Update: ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.Text("half")}},
		})},
		{name: "tool completed", event: newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1, Call: toolCall,
			Result: ai.ToolResultText(toolCall.ID, toolCall.Name, "done"),
		})},
		{name: "approval required", event: newInteractionEvent(
			EventApprovalRequired,
			ApprovalRequired{
				RequestID: "request-1", CallID: toolCall.ID, Tool: toolCall.Name,
				Command: []string{"cat", "main.go"}, CWD: "/workspace",
				Justification: "inspect source",
				Choices:       []approval.Choice{approval.ChoiceAllowOnce, approval.ChoiceDeny},
			},
		)},
		{name: "approval unknown", event: newInteractionEvent(
			EventApprovalUnknown,
			ApprovalUnknown{
				RequestID: "request-1", CallID: toolCall.ID, Tool: toolCall.Name,
				Fingerprint: "abcdef", Attempt: 1, Pending: true,
				Reason: "process may have started", Recoverable: true,
				Choices: []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed},
			},
		)},
		{name: "approval resolved", event: newInteractionEvent(
			EventApprovalResolved,
			ApprovalResolved{RequestID: "request-1", Choice: approval.ChoiceAllowOnce},
		)},
		{name: "workspace changed", event: newInteractionEvent(
			EventWorkspaceChanged,
			WorkspaceChanged{
				Entries: []WorkspaceChange{{Path: "main.go", Kind: changes.KindModified}},
				Diff:    "diff --git a/main.go b/main.go", Truncated: true,
			},
		)},
		{name: "status changed", event: newStatusEvent(
			EventStatusChanged, StatusChanged{Phase: PhaseRunning},
		)},
		{name: "integration diagnostic", event: newStatusEvent(
			EventIntegrationDiagnostic,
			IntegrationDiagnostic{Component: "mcp", Code: "connect_failed", Message: "unavailable", Disabled: true},
		)},
		{name: "error", event: newStatusEvent(
			EventError, RuntimeError{Code: "model_failed", Message: "request failed", Fatal: true},
		)},
	}
}

func newSessionEvent(eventType EventType, payload EventPayload) Event {
	return Event{
		Schema: EventSchema, Sequence: 1, Time: eventTestTime,
		SessionID: "session-1", Type: eventType, Payload: payload,
	}
}

func newInteractionEvent(eventType EventType, payload EventPayload) Event {
	event := newSessionEvent(eventType, payload)
	event.InteractionID = "interaction-1"

	return event
}

func newTestEvent(eventType EventType, payload EventPayload) Event {
	event := newInteractionEvent(eventType, payload)
	event.RunID = "run-1"

	return event
}

func newStatusEvent(eventType EventType, payload EventPayload) Event {
	return newSessionEvent(eventType, payload)
}

func TestErrorsAreInspectable(t *testing.T) {
	t.Parallel()

	_, err := UnmarshalEvent([]byte(`{}`))
	assert.ErrorIs(t, err, ErrInvalidEvent)
}
