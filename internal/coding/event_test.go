//nolint:wsl_v5 // Protocol fixtures keep encode, disclosure, and telemetry assertions grouped.
package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/subagent"
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

func TestValidateEventAcceptsCustomProviderIdentity(t *testing.T) {
	t.Parallel()

	provider := ai.Provider("opencode-go")

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: provider, ModelID: "deepseek-v4-flash",
		}),
		newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamMessageStart, Provider: provider, Model: "deepseek-v4-flash",
		}),
	}
	for _, event := range events {
		require.NoError(t, ValidateEvent(event))
	}

	invalid := newSessionEvent(EventSessionOpened, SessionOpened{
		Provider: "OpenCode", ModelID: "deepseek-v4-flash",
	})
	require.ErrorIs(t, ValidateEvent(invalid), ErrInvalidEvent)
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
	var treeEvent []byte
	for _, test := range eventCases() {
		if test.event.Type == EventSessionTreeChanged {
			treeEvent, err = MarshalEvent(test.event)
			require.NoError(t, err)
			break
		}
	}
	require.NotEmpty(t, treeEvent)

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
		{
			name: "unknown tree transcript field",
			data: strings.Replace(
				string(treeEvent),
				`"role":"user"`,
				`"role":"user","unknown":true`,
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

func TestProjectRedactsPresentedPlanContentButKeepsLocalContent(t *testing.T) {
	t.Parallel()

	const secretPlan = "# Private architecture\n\nDo not export this Plan."
	request, err := planreview.ProposalFromCall(ai.ToolCallPart{
		ID: "present-1", Name: planreview.PresentToolName,
		Args: ai.JSON(`{"expected_revision":"","content":"# Private architecture\n\nDo not export this Plan."}`),
	})
	require.NoError(t, err)
	event := newInteractionEvent(
		EventPlanReviewRequired,
		PlanReviewRequired{Request: request},
	)

	safe, err := Project(event, DisclosureSafe)
	require.NoError(t, err)
	encodedSafe, err := json.Marshal(safe)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedSafe), secretPlan)
	assert.Contains(t, string(encodedSafe), request.Revision)

	content, err := Project(event, DisclosureContent)
	require.NoError(t, err)
	encodedContent, err := json.Marshal(content)
	require.NoError(t, err)
	assert.Contains(t, string(encodedContent), "Private architecture")

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encodedTelemetry, err := json.Marshal(telemetry)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedTelemetry), "Private architecture")
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
			name: "subagent task preview",
			event: newTestEvent(EventSubagentCreated, SubagentLifecycle{
				Role: subagent.RoleExplore, State: subagent.StateCreated,
				ChildSessionID: "child-1", ParentRunID: "run-1",
				Model: "openai/test", TaskPreview: secret,
				Activity: subagent.ActivitySummary{
					Action: subagent.ActivityActionRead, Target: secret,
				},
			}),
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
		Call: ToolCall{ID: "call-1", Name: "read", Arguments: arguments},
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
	toolCall := ToolCall{ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`)}
	usage := TokenUsage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1}
	request, err := question.NewRequest("question-1", "call-1", question.Spec{
		Questions: []question.Question{{
			Header: "Scope", Question: "Which scope?",
			Options: []question.Option{
				{Label: "Core", Description: "Core implementation"},
				{Label: "Tests", Description: "Test coverage"},
			},
		}},
	})
	if err != nil {
		panic(err)
	}
	resolution := question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"Core"}}},
	}
	planRequest, err := planreview.NewRequest(
		"submit-plan",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		512,
	)
	if err != nil {
		panic(err)
	}

	return []eventCase{
		{name: "session opened", event: newSessionEvent(EventSessionOpened, SessionOpened{
			Resumed: false, Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		})},
		{name: "session closed", event: newSessionEvent(
			EventSessionClosed, SessionClosed{Reason: SessionClosedNormally},
		)},
		{name: "session tree changed", event: newSessionEvent(
			EventSessionTreeChanged, SessionTreeChanged{
				Tree: SessionTree{
					SessionID: "session-1", Name: "implementation", LeafID: "node-1",
					TotalNodes: 1, Nodes: []SessionNode{{
						ID: "node-1", Kind: SessionNodeMessage, CreatedAt: eventTestTime,
						Current: true, OnActivePath: true,
					}},
				},
				Transcript: []ai.Message{ai.UserText("continue")},
			},
		)},
		{name: "session navigated", event: newSessionEvent(
			EventSessionNavigated,
			SessionNavigated{FromID: "node-2", ToID: "node-1", WithSummary: true},
		)},
		{name: "session forked", event: newSessionEvent(
			EventSessionForked,
			SessionForked{
				SourceSessionID: "session-1", TargetSessionID: "session-2", AtEntryID: "node-1",
			},
		)},
		{name: "compaction started", event: newSessionEvent(
			EventCompactionStarted,
			CompactionStarted{Mode: CompactionManual, Preview: CompactionPreview{
				Available: true, Token: "preview-token", EstimatedTokens: 3000,
				ThresholdTokens: 2000, SummarizedMessages: 4, KeptMessages: 2,
				FirstKeptID: "node-1",
			}},
		)},
		{name: "compaction completed", event: newSessionEvent(
			EventCompactionCompleted,
			CompactionCompleted{
				Mode: CompactionManual, TokensBefore: 3000, TokensAfter: 900,
				FirstKeptID: "node-1", DurationMillis: 25,
			},
		)},
		{name: "mode changed", event: newSessionEvent(
			EventModeChanged, ModeChanged{Mode: ModePlan},
		)},
		{name: "interaction started", event: newInteractionEvent(
			EventInteractionStarted, InteractionStarted{Resumed: true},
		)},
		{name: "interaction completed", event: newInteractionEvent(
			EventInteractionCompleted,
			InteractionCompleted{
				Outcome: InteractionSucceeded, Stop: agent.StopEndTurn,
				Usage: usage, DurationMillis: 250,
			},
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
		{name: "message discarded", event: newTestEvent(
			EventMessageDiscarded, MessageDiscarded{Turn: 1},
		)},
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
		{name: "subagent created", event: newTestEvent(
			EventSubagentCreated,
			SubagentLifecycle{
				Role: subagent.RoleExplore, State: subagent.StateCreated,
				ChildSessionID: "child-1", ParentRunID: "run-1",
				Model: "openai/test", TaskPreview: "inspect runtime",
			},
		)},
		{name: "subagent started", event: newTestEvent(
			EventSubagentStarted,
			SubagentLifecycle{
				Role: subagent.RoleExplore, State: subagent.StateRunning,
				ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
				Model: "openai/test", TaskPreview: "inspect runtime",
			},
		)},
		{name: "subagent progress", event: newTestEvent(
			EventSubagentProgress,
			SubagentLifecycle{
				Role: subagent.RoleExplore, State: subagent.StateRunning,
				ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
				Model: "openai/test", TaskPreview: "inspect runtime",
				Activity: subagent.ActivitySummary{
					Action: subagent.ActivityActionSearch, Target: "Runtime in internal/coding",
				},
				Turns: 1, ToolCalls: 2, Usage: usage,
			},
		)},
		{name: "subagent completed", event: newTestEvent(
			EventSubagentCompleted,
			SubagentLifecycle{
				Role: subagent.RoleExplore, State: subagent.StateSucceeded,
				ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
				Model: "openai/test", TaskPreview: "inspect runtime", Code: "ok",
				Activity: subagent.ActivitySummary{
					Action: subagent.ActivityActionSearch, Target: "Runtime in internal/coding",
				},
				Stop: agent.StopEndTurn, Turns: 1, ToolCalls: 2, Usage: usage, DurationMillis: 25,
			},
		)},
		{name: "subagent failed", event: newTestEvent(
			EventSubagentFailed,
			SubagentLifecycle{
				Role: subagent.RolePlan, State: subagent.StateFailed,
				ChildSessionID: "child-2", ParentRunID: "run-1", ChildRunID: "child-run-2",
				Model: "openai/test", Code: "invalid_result", Turns: 1, Usage: usage,
			},
		)},
		{name: "subagent canceled", event: newTestEvent(
			EventSubagentCanceled,
			SubagentLifecycle{
				Role: subagent.RoleReview, State: subagent.StateCanceled,
				ChildSessionID: "child-3", ParentRunID: "run-1",
				Model: "openai/test", Code: "canceled", DurationMillis: 5,
			},
		)},
		{name: "subagent interrupted", event: newTestEvent(
			EventSubagentInterrupted,
			SubagentLifecycle{
				Role: subagent.RoleReview, State: subagent.StateInterrupted,
				ChildSessionID: "child-4", ParentRunID: "run-1", ChildRunID: "child-run-4",
				Model: "openai/test", Code: "process_interrupted", DurationMillis: 5,
			},
		)},
		{name: "Team lifecycle", event: newSessionEvent(
			EventTeamLifecycle,
			TeamLifecycle{
				TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1",
				AttemptID: "attempt-1", ChildSessionID: "child-1",
				State: TeamLifecycleCompleted, Turns: 2, ToolCalls: 3,
				Usage: usage, DurationMillis: 25,
			},
		)},
		{name: "Team control lifecycle", event: newSessionEvent(
			EventTeamControlLifecycle,
			TeamControlLifecycle{
				TeamID: "team-1", Revision: 3, CommandID: "control-1",
				Action: TeamControlMessage, MemberID: "worker-1", TaskID: "task-1",
				AttemptID: "attempt-1", OwnerGeneration: 2, State: TeamControlApplied,
			},
		)},
		{name: "Team integration lifecycle", event: newSessionEvent(
			EventTeamIntegrationLifecycle,
			TeamIntegrationLifecycle{
				TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationVerified,
				VerificationState: "passed", Attempts: 2,
				Files: 3, Added: 1, Changed: 1, Deleted: 1, Binary: 1,
			},
		)},
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
		{name: "question required", event: newInteractionEvent(
			EventQuestionRequired,
			QuestionRequired{Request: request, Count: len(request.Questions)},
		)},
		{name: "question resolved", event: newInteractionEvent(
			EventQuestionResolved,
			QuestionResolved{Resolution: resolution, AnswerCount: len(resolution.Answers)},
		)},
		{name: "question rejected", event: newInteractionEvent(
			EventQuestionRejected,
			QuestionRejected{RequestID: request.ID, SchemaDigest: request.SchemaDigest},
		)},
		{name: "Plan review required", event: newInteractionEvent(
			EventPlanReviewRequired,
			PlanReviewRequired{Request: planRequest},
		)},
		{name: "Plan review resolved", event: newInteractionEvent(
			EventPlanReviewResolved,
			PlanReviewResolved{
				RequestID: planRequest.ID,
				Revision:  planRequest.Revision,
				Decision:  planreview.DecisionApprove,
			},
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

func TestTeamLifecycleTelemetryOmitsRoutingIdentity(t *testing.T) {
	t.Parallel()

	event := newSessionEvent(EventTeamLifecycle, TeamLifecycle{
		TeamID: team.ID("team-secret"), MemberID: team.MemberID("member-secret"),
		TaskID: team.TaskID("task-secret"), AttemptID: team.AttemptID("attempt-secret"),
		ChildSessionID: "session-secret", State: TeamLifecycleCompleted,
		Turns: 2, ToolCalls: 4, Usage: TokenUsage{InputTokens: 10, OutputTokens: 5},
		DurationMillis: 100,
	})
	require.NoError(t, ValidateEvent(event))

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encoded, err := json.Marshal(telemetry)
	require.NoError(t, err)
	for _, secret := range []string{
		"team-secret", "member-secret", "task-secret", "attempt-secret", "session-secret",
	} {
		assert.NotContains(t, string(encoded), secret)
	}
	assert.Equal(t, "team_worker", telemetry.Agent)
	assert.Equal(t, "attempt", telemetry.TeamScope)
	assert.Equal(t, string(TeamLifecycleCompleted), telemetry.TeamState)
}

func TestValidateTeamLifecycleRejectsContentAndAccountingShapeViolations(t *testing.T) {
	t.Parallel()

	tests := []TeamLifecycle{
		{TeamID: "/private/team", State: TeamLifecycleProposed},
		{TeamID: "team-1", State: TeamLifecycleRunning},
		{TeamID: "team-1", MemberID: "worker-1", State: TeamLifecycleRunning},
		{
			TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1",
			AttemptID: "attempt-1", State: TeamLifecycleRunning,
			Usage: TokenUsage{InputTokens: 1},
		},
		{TeamID: "team-1", State: TeamLifecycleFailed},
		{
			TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1",
			AttemptID: "attempt-1", ChildSessionID: "/private/session",
			State: TeamLifecycleRunning,
		},
	}
	for _, payload := range tests {
		event := newSessionEvent(EventTeamLifecycle, payload)
		require.ErrorIs(t, ValidateEvent(event), ErrInvalidEvent)
	}
}

func TestTeamIntegrationTelemetryOmitsRoutingIdentity(t *testing.T) {
	t.Parallel()

	event := newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
		TeamID: "team-secret", IntegrationID: "integration-secret",
		State: TeamIntegrationVerificationFailed, VerificationState: "timeout",
		Attempts: 2, Files: 2, Changed: 2, Code: "verification_timeout",
	})
	require.NoError(t, ValidateEvent(event))

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encoded, err := json.Marshal(telemetry)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "team-secret")
	assert.NotContains(t, string(encoded), "integration-secret")
	assert.Equal(t, "team_integration", telemetry.Agent)
	assert.Equal(t, string(TeamIntegrationVerificationFailed), telemetry.IntegrationState)
	assert.Equal(t, "timeout", telemetry.IntegrationVerification)
	assert.True(t, telemetry.Failed)
}

func TestValidateTeamIntegrationLifecycleRejectsInvalidEvidence(t *testing.T) {
	t.Parallel()

	tests := []TeamIntegrationLifecycle{
		{TeamID: "team-1", IntegrationID: "", State: TeamIntegrationReady},
		{
			TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationReady,
			Files: 2, Changed: 1,
		},
		{
			TeamID: "team-1", IntegrationID: "int-1",
			State: TeamIntegrationVerificationFailed, VerificationState: "failed",
		},
		{
			TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationVerified,
			VerificationState: "unknown",
		},
	}
	for _, payload := range tests {
		event := newSessionEvent(EventTeamIntegrationLifecycle, payload)
		require.ErrorIs(t, ValidateEvent(event), ErrInvalidEvent)
	}
}

func TestSessionTreeSafeProjectionScrubsContentAndTelemetryIsContentFree(t *testing.T) {
	t.Parallel()

	const secret = "private-session-content"
	event := newSessionEvent(EventSessionTreeChanged, SessionTreeChanged{
		Tree: SessionTree{
			SessionID: "session-1", Name: secret, LeafID: "node-1", TotalNodes: 1,
			Nodes: []SessionNode{{
				ID: "node-1", Kind: SessionNodeMessage, CreatedAt: eventTestTime,
				Label: secret, Current: true, OnActivePath: true,
			}},
		},
		Transcript: []ai.Message{
			ai.UserText(secret),
			ai.ToolResultText("call-1", "shell", secret),
			ai.AssistantText("public answer"),
		},
	})

	safe, err := Project(event, DisclosureSafe)
	require.NoError(t, err)
	encoded, err := json.Marshal(safe)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), secret)
	assert.Contains(t, string(encoded), "public answer")

	telemetry, err := Telemetry(event)
	require.NoError(t, err)
	encodedTelemetry, err := json.Marshal(telemetry)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedTelemetry), secret)
	assert.Equal(t, 1, telemetry.Nodes)
}

func newSessionEvent(eventType EventType, payload EventPayload) Event {
	switch value := payload.(type) {
	case SessionOpened:
		if value.Mode == "" {
			value.Mode = ModeAgent
		}
		payload = value
	case InteractionStarted:
		if value.Mode == "" {
			value.Mode = ModeAgent
		}
		payload = value
	}

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
