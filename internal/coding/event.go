//nolint:wsl_v5 // Closed event protocol validation keeps related checks adjacent.
package coding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	// EventSchema is the current Coding Agent event envelope schema.
	EventSchema = "pips.coding.event/v1alpha1"

	maxEventIDBytes      = 512
	maxEventTextBytes    = 1 << 20
	maxEventItems        = 4096
	maxEventDurationMS   = int64((7 * 24 * time.Hour) / time.Millisecond)
	maxDiagnosticMessage = 16 << 10
)

var (
	// ErrInvalidEvent means an event or payload violates the Coding event contract.
	ErrInvalidEvent = errors.New("coding event: invalid event")
	// ErrEventProtocol means an event cannot follow the preceding reducer state.
	ErrEventProtocol = errors.New("coding event: protocol violation")
	// ErrUnsafeEventEncoding prevents direct serialization that bypasses disclosure.
	ErrUnsafeEventEncoding = errors.New("coding event: disclosure projection required")
)

// EventType discriminates Coding Agent event payloads.
type EventType string

// Coding Agent event types.
const (
	EventSessionOpened         EventType = "session.opened"
	EventSessionClosed         EventType = "session.closed"
	EventSessionTreeChanged    EventType = "session.tree_changed"
	EventSessionNavigated      EventType = "session.navigated"
	EventSessionForked         EventType = "session.forked"
	EventCompactionStarted     EventType = "compaction.started"
	EventCompactionCompleted   EventType = "compaction.completed"
	EventInteractionStarted    EventType = "interaction.started"
	EventInteractionCompleted  EventType = "interaction.completed"
	EventRunStarted            EventType = "run.started"
	EventRunCompleted          EventType = "run.completed"
	EventTurnStarted           EventType = "turn.started"
	EventTurnCompleted         EventType = "turn.completed"
	EventMessageCommitted      EventType = "message.committed"
	EventMessageDelta          EventType = "message.delta"
	EventToolStarted           EventType = "tool.started"
	EventToolUpdated           EventType = "tool.updated"
	EventToolCompleted         EventType = "tool.completed"
	EventSubagentCreated       EventType = "subagent.created"
	EventSubagentStarted       EventType = "subagent.started"
	EventSubagentProgress      EventType = "subagent.progress"
	EventSubagentCompleted     EventType = "subagent.completed"
	EventSubagentFailed        EventType = "subagent.failed"
	EventSubagentCanceled      EventType = "subagent.canceled"
	EventSubagentInterrupted   EventType = "subagent.interrupted"
	EventApprovalRequired      EventType = "approval.required"
	EventApprovalUnknown       EventType = "approval.unknown"
	EventApprovalResolved      EventType = "approval.resolved"
	EventWorkspaceChanged      EventType = "workspace.changed"
	EventStatusChanged         EventType = "status.changed"
	EventIntegrationDiagnostic EventType = "integration.diagnostic"
	EventError                 EventType = "error"
)

// Event is one immutable-by-contract increment in a Runtime event stream.
// Serialize it only through [Project] or [MarshalEvent].
type Event struct {
	Schema        string       `json:"schema"`
	Sequence      uint64       `json:"sequence"`
	Time          time.Time    `json:"time"`
	SessionID     string       `json:"session_id"`
	InteractionID string       `json:"interaction_id,omitempty"`
	RunID         string       `json:"run_id,omitempty"`
	Type          EventType    `json:"type"`
	Payload       EventPayload `json:"-"`
}

// MarshalJSON rejects direct Event serialization so callers cannot bypass
// the disclosure projection required at persistence and process boundaries.
func (Event) MarshalJSON() ([]byte, error) {
	return nil, ErrUnsafeEventEncoding
}

// UnmarshalJSON performs strict event decoding.
func (e *Event) UnmarshalJSON(data []byte) error {
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		return err
	}

	*e = decoded

	return nil
}

// EventPayload is the sealed set of Coding event payloads.
type EventPayload interface {
	eventPayload()
}

// TokenUsage is stable provider-neutral token accounting for Coding events.
type TokenUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
}

// SessionOpened carries the initial durable session metadata.
type SessionOpened struct {
	Resumed  bool        `json:"resumed"`
	Provider ai.Provider `json:"provider,omitempty"`
	ModelID  string      `json:"model_id,omitempty"`
}

// SessionCloseReason classifies why a Runtime stopped owning a session.
type SessionCloseReason string

// Session close reasons.
const (
	SessionClosedNormally SessionCloseReason = "closed"
	SessionClosedFailed   SessionCloseReason = "failed"
	SessionClosedCanceled SessionCloseReason = "canceled"
)

// SessionClosed carries the final session ownership outcome.
type SessionClosed struct {
	Reason SessionCloseReason `json:"reason"`
}

// SessionTreeChanged replaces the reducer's durable bounded tree projection.
type SessionTreeChanged struct {
	Tree       SessionTree  `json:"tree"`
	Transcript []ai.Message `json:"transcript"`
}

// SessionNavigated records one successful same-file leaf change.
type SessionNavigated struct {
	FromID      string `json:"from_id,omitempty"`
	ToID        string `json:"to_id,omitempty"`
	WithSummary bool   `json:"with_summary"`
}

// SessionForked records lineage after a new durable Session is created.
type SessionForked struct {
	SourceSessionID string `json:"source_session_id"`
	TargetSessionID string `json:"target_session_id"`
	AtEntryID       string `json:"at_entry_id,omitempty"`
}

// CompactionStarted announces a manual or automatic compaction before model I/O.
type CompactionStarted struct {
	Mode    CompactionMode    `json:"mode"`
	Preview CompactionPreview `json:"preview"`
}

// CompactionCompleted announces one durably committed compaction.
type CompactionCompleted struct {
	Mode           CompactionMode `json:"mode"`
	TokensBefore   int            `json:"tokens_before"`
	TokensAfter    int            `json:"tokens_after"`
	FirstKeptID    string         `json:"first_kept_id"`
	DurationMillis int64          `json:"duration_ms"`
}

// InteractionStarted opens one user interaction, including approval continuations.
type InteractionStarted struct {
	Resumed bool `json:"resumed"`
}

// InteractionOutcome classifies a terminal user interaction.
type InteractionOutcome string

// Interaction outcomes.
const (
	InteractionSucceeded InteractionOutcome = "succeeded"
	InteractionFailed    InteractionOutcome = "failed"
	InteractionCanceled  InteractionOutcome = "canceled"
)

// InteractionCompleted closes one user interaction.
type InteractionCompleted struct {
	Outcome        InteractionOutcome `json:"outcome"`
	Usage          TokenUsage         `json:"usage"`
	DurationMillis int64              `json:"duration_ms"`
}

// RunStarted opens one Agent invocation within an interaction.
type RunStarted struct {
	ParentRunID string `json:"parent_run_id,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

// RunCompleted closes one successful Agent invocation.
type RunCompleted struct {
	Stop  agent.StopReason `json:"stop"`
	Turns int              `json:"turns"`
	Usage TokenUsage       `json:"usage"`
}

// TurnStarted opens one 1-based Agent model turn.
type TurnStarted struct {
	Turn int `json:"turn"`
}

// TurnCompleted closes one Agent model turn.
type TurnCompleted struct {
	Turn  int        `json:"turn"`
	Usage TokenUsage `json:"usage"`
}

// MessageCommitted carries one message durably appended to the Harness session.
type MessageCommitted struct {
	Message ai.Message `json:"message"`
}

// MessageDelta is one provider-neutral model streaming increment.
type MessageDelta struct {
	Kind          ai.StreamEventType `json:"kind"`
	Provider      ai.Provider        `json:"provider,omitempty"`
	ResponseID    string             `json:"response_id,omitempty"`
	Model         string             `json:"model,omitempty"`
	Text          string             `json:"text,omitempty"`
	Signature     string             `json:"signature,omitempty"`
	ToolCallIndex int                `json:"tool_call_index,omitempty"`
	ToolCallID    string             `json:"tool_call_id,omitempty"`
	ToolCallName  string             `json:"tool_call_name,omitempty"`
	Arguments     string             `json:"arguments,omitempty"`
	FinishReason  ai.FinishReason    `json:"finish_reason,omitempty"`
	Usage         *TokenUsage        `json:"usage,omitempty"`
}

// ToolCall is the bounded model request projected into tool lifecycle events.
type ToolCall struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Arguments ai.JSON `json:"arguments,omitempty"`
}

// ToolStarted announces a tool invocation before policy gating and execution.
type ToolStarted struct {
	Turn int      `json:"turn"`
	Call ToolCall `json:"call"`
}

// ToolUpdated carries best-effort progress for an active tool call.
type ToolUpdated struct {
	Turn   int        `json:"turn"`
	Call   ToolCall   `json:"call"`
	Update ai.Message `json:"update"`
}

// ToolCompleted carries the final tool-result message for one call.
type ToolCompleted struct {
	Turn   int        `json:"turn"`
	Call   ToolCall   `json:"call"`
	Result ai.Message `json:"result"`
}

// SubagentLifecycle is one content-bounded specialist lifecycle projection.
// Child transcript, Tool arguments, paths, and structured results are loaded
// from the child Session only and never copied into this payload.
type SubagentLifecycle struct {
	Role           subagent.Role            `json:"role"`
	State          subagent.State           `json:"state"`
	ChildSessionID string                   `json:"child_session_id"`
	ParentRunID    string                   `json:"parent_run_id"`
	ChildRunID     string                   `json:"child_run_id,omitempty"`
	Model          string                   `json:"model"`
	TaskPreview    string                   `json:"task_preview,omitempty"`
	Activity       subagent.ActivitySummary `json:"activity,omitzero"`
	Code           string                   `json:"code,omitempty"`
	Stop           agent.StopReason         `json:"stop,omitempty"`
	Turns          int                      `json:"turns"`
	ToolCalls      int                      `json:"tool_calls"`
	Usage          TokenUsage               `json:"usage"`
	DurationMillis int64                    `json:"duration_ms"`
}

// ApprovalRequired describes an exact pending operation for the approval overlay.
type ApprovalRequired struct {
	RequestID     string            `json:"request_id"`
	CallID        string            `json:"call_id"`
	Tool          string            `json:"tool"`
	Command       []string          `json:"command,omitempty"`
	CWD           string            `json:"cwd,omitempty"`
	Justification string            `json:"justification,omitempty"`
	Choices       []approval.Choice `json:"choices"`
}

// ApprovalUnknown describes an operation whose durable result is uncertain.
type ApprovalUnknown struct {
	RequestID   string            `json:"request_id"`
	CallID      string            `json:"call_id"`
	Tool        string            `json:"tool"`
	Fingerprint string            `json:"fingerprint"`
	Attempt     int               `json:"attempt"`
	Pending     bool              `json:"pending"`
	Reason      string            `json:"reason,omitempty"`
	Recoverable bool              `json:"recoverable"`
	Choices     []approval.Choice `json:"choices"`
}

// ApprovalResolved carries the explicit choice durably applied to a request.
type ApprovalResolved struct {
	RequestID string          `json:"request_id"`
	Choice    approval.Choice `json:"choice"`
}

// WorkspaceChange is one normalized workspace-relative path change.
type WorkspaceChange struct {
	Path         string       `json:"path"`
	PreviousPath string       `json:"previous_path,omitempty"`
	Kind         changes.Kind `json:"kind"`
}

// WorkspaceChanged carries a bounded change report for the completed interaction.
type WorkspaceChanged struct {
	Entries   []WorkspaceChange `json:"entries"`
	Diff      string            `json:"diff,omitempty"`
	Truncated bool              `json:"truncated"`
}

// Phase is the product Runtime state exposed to frontends.
type Phase string

// Runtime phases.
const (
	PhaseIdle    Phase = "idle"
	PhaseRunning Phase = "running"
	PhasePaused  Phase = "paused"
	PhaseClosing Phase = "closing"
	PhaseClosed  Phase = "closed"
)

// StatusChanged projects one Runtime phase transition.
type StatusChanged struct {
	Phase Phase `json:"phase"`
}

// IntegrationDiagnostic reports an optional integration failure or suppression.
type IntegrationDiagnostic struct {
	Component string `json:"component"`
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Disabled  bool   `json:"disabled"`
}

// RuntimeError reports a stable user-visible runtime error classification.
type RuntimeError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Fatal   bool   `json:"fatal"`
}

func (SessionOpened) eventPayload()         {}
func (SessionClosed) eventPayload()         {}
func (SessionTreeChanged) eventPayload()    {}
func (SessionNavigated) eventPayload()      {}
func (SessionForked) eventPayload()         {}
func (CompactionStarted) eventPayload()     {}
func (CompactionCompleted) eventPayload()   {}
func (InteractionStarted) eventPayload()    {}
func (InteractionCompleted) eventPayload()  {}
func (RunStarted) eventPayload()            {}
func (RunCompleted) eventPayload()          {}
func (TurnStarted) eventPayload()           {}
func (TurnCompleted) eventPayload()         {}
func (MessageCommitted) eventPayload()      {}
func (MessageDelta) eventPayload()          {}
func (ToolStarted) eventPayload()           {}
func (ToolUpdated) eventPayload()           {}
func (ToolCompleted) eventPayload()         {}
func (SubagentLifecycle) eventPayload()     {}
func (ApprovalRequired) eventPayload()      {}
func (ApprovalUnknown) eventPayload()       {}
func (ApprovalResolved) eventPayload()      {}
func (WorkspaceChanged) eventPayload()      {}
func (StatusChanged) eventPayload()         {}
func (IntegrationDiagnostic) eventPayload() {}
func (RuntimeError) eventPayload()          {}

// ValidateEvent verifies the envelope, payload discriminator, and bounded content.
func ValidateEvent(event Event) error {
	if event.Schema != EventSchema {
		return invalidEvent("unknown schema %q", event.Schema)
	}

	if event.Sequence == 0 {
		return invalidEvent("sequence must be positive")
	}

	if event.Time.IsZero() {
		return invalidEvent("time is required")
	}

	if _, offset := event.Time.Zone(); offset != 0 {
		return invalidEvent("time must use UTC")
	}

	if err := validateEventID("session id", event.SessionID, true); err != nil {
		return err
	}

	if err := validateEventID("interaction id", event.InteractionID, false); err != nil {
		return err
	}

	if err := validateEventID("run id", event.RunID, false); err != nil {
		return err
	}

	if err := validateEnvelopeIDs(event); err != nil {
		return err
	}

	return validatePayload(event.Type, event.Payload)
}

func validateEnvelopeIDs(event Event) error {
	switch event.Type {
	case EventSessionOpened, EventSessionClosed, EventSessionTreeChanged,
		EventSessionNavigated, EventSessionForked, EventCompactionStarted,
		EventCompactionCompleted:
		if event.InteractionID != "" || event.RunID != "" {
			return invalidEvent("session event has interaction or run id")
		}
	case EventInteractionStarted, EventInteractionCompleted,
		EventApprovalRequired, EventApprovalUnknown, EventApprovalResolved,
		EventWorkspaceChanged:
		if event.InteractionID == "" || event.RunID != "" {
			return invalidEvent("%s requires only an interaction id", event.Type)
		}
	case EventRunStarted, EventRunCompleted, EventTurnStarted, EventTurnCompleted,
		EventMessageCommitted, EventMessageDelta, EventToolStarted, EventToolUpdated,
		EventToolCompleted, EventSubagentCreated, EventSubagentStarted,
		EventSubagentProgress, EventSubagentCompleted, EventSubagentFailed,
		EventSubagentCanceled, EventSubagentInterrupted:
		if event.InteractionID == "" || event.RunID == "" {
			return invalidEvent("%s requires interaction and run ids", event.Type)
		}
	case EventStatusChanged, EventIntegrationDiagnostic, EventError:
		if event.RunID != "" && event.InteractionID == "" {
			return invalidEvent("run id requires an interaction id")
		}
	default:
		return invalidEvent("unknown type %q", event.Type)
	}

	return nil
}

//nolint:gocyclo,cyclop,funlen,maintidx // Closed taxonomy validation is deliberately exhaustive.
func validatePayload(eventType EventType, payload EventPayload) error {
	switch value := payload.(type) {
	case SessionOpened:
		if eventType != EventSessionOpened || !validProvider(value.Provider) ||
			!validIdentifierText(value.ModelID, maxEventIDBytes, true) ||
			(value.Provider == "") != (value.ModelID == "") {
			return invalidPayload(eventType, payload)
		}
	case SessionClosed:
		if eventType != EventSessionClosed || !validSessionCloseReason(value.Reason) {
			return invalidPayload(eventType, payload)
		}
	case SessionTreeChanged:
		if eventType != EventSessionTreeChanged || validateSessionTree(value.Tree) != nil ||
			len(value.Transcript) > maxEventItems {
			return invalidPayload(eventType, payload)
		}
		for _, message := range value.Transcript {
			if validateMessage(message) != nil {
				return invalidPayload(eventType, payload)
			}
		}
	case SessionNavigated:
		if eventType != EventSessionNavigated || validateOptionalID(value.FromID) != nil ||
			validateOptionalID(value.ToID) != nil {
			return invalidPayload(eventType, payload)
		}
	case SessionForked:
		if eventType != EventSessionForked ||
			validateEventID("source session id", value.SourceSessionID, true) != nil ||
			validateEventID("target session id", value.TargetSessionID, true) != nil ||
			validateOptionalID(value.AtEntryID) != nil || value.SourceSessionID == value.TargetSessionID {
			return invalidPayload(eventType, payload)
		}
	case CompactionStarted:
		if eventType != EventCompactionStarted || !validCompactionMode(value.Mode) ||
			validateCompactionPreview(value.Preview, true) != nil {
			return invalidPayload(eventType, payload)
		}
	case CompactionCompleted:
		if eventType != EventCompactionCompleted || !validCompactionMode(value.Mode) ||
			value.TokensBefore < 0 || value.TokensAfter < 0 ||
			value.TokensAfter > value.TokensBefore || validateEventID("first kept id", value.FirstKeptID, true) != nil ||
			value.DurationMillis < 0 || value.DurationMillis > maxEventDurationMS {
			return invalidPayload(eventType, payload)
		}
	case InteractionStarted:
		if eventType != EventInteractionStarted {
			return invalidPayload(eventType, payload)
		}
	case InteractionCompleted:
		if eventType != EventInteractionCompleted || !validInteractionOutcome(value.Outcome) ||
			value.DurationMillis < 0 || value.DurationMillis > maxEventDurationMS ||
			!validTokenUsage(value.Usage) {
			return invalidPayload(eventType, payload)
		}
	case RunStarted:
		if eventType != EventRunStarted ||
			validateOptionalID(value.ParentRunID) != nil ||
			!validIdentifierText(value.Agent, maxEventIDBytes, true) {
			return invalidPayload(eventType, payload)
		}
	case RunCompleted:
		if eventType != EventRunCompleted || !validStopReason(value.Stop) ||
			value.Turns < 0 || !validTokenUsage(value.Usage) {
			return invalidPayload(eventType, payload)
		}
	case TurnStarted:
		if eventType != EventTurnStarted || value.Turn < 1 {
			return invalidPayload(eventType, payload)
		}
	case TurnCompleted:
		if eventType != EventTurnCompleted || value.Turn < 1 || !validTokenUsage(value.Usage) {
			return invalidPayload(eventType, payload)
		}
	case MessageCommitted:
		if eventType != EventMessageCommitted || validateMessage(value.Message) != nil {
			return invalidPayload(eventType, payload)
		}
	case MessageDelta:
		if eventType != EventMessageDelta || validateMessageDelta(value) != nil {
			return invalidPayload(eventType, payload)
		}
	case ToolStarted:
		if eventType != EventToolStarted || value.Turn < 1 || validateToolCall(value.Call) != nil {
			return invalidPayload(eventType, payload)
		}
	case ToolUpdated:
		if eventType != EventToolUpdated || value.Turn < 1 || validateToolCall(value.Call) != nil ||
			validateToolMessage(value.Update, value.Call.ID, false) != nil {
			return invalidPayload(eventType, payload)
		}
	case ToolCompleted:
		if eventType != EventToolCompleted || value.Turn < 1 || validateToolCall(value.Call) != nil ||
			validateToolMessage(value.Result, value.Call.ID, true) != nil {
			return invalidPayload(eventType, payload)
		}
	case SubagentLifecycle:
		if validateSubagentLifecycle(eventType, value) != nil {
			return invalidPayload(eventType, payload)
		}
	case ApprovalRequired:
		if eventType != EventApprovalRequired || validateApprovalRequired(value) != nil {
			return invalidPayload(eventType, payload)
		}
	case ApprovalUnknown:
		if eventType != EventApprovalUnknown || validateApprovalUnknown(value) != nil {
			return invalidPayload(eventType, payload)
		}
	case ApprovalResolved:
		if eventType != EventApprovalResolved || validateEventID("request id", value.RequestID, true) != nil ||
			!validApprovalChoice(value.Choice) {
			return invalidPayload(eventType, payload)
		}
	case WorkspaceChanged:
		if eventType != EventWorkspaceChanged || validateWorkspaceChanged(value) != nil {
			return invalidPayload(eventType, payload)
		}
	case StatusChanged:
		if eventType != EventStatusChanged || !validPhase(value.Phase) {
			return invalidPayload(eventType, payload)
		}
	case IntegrationDiagnostic:
		if eventType != EventIntegrationDiagnostic || validateDiagnostic(value) != nil {
			return invalidPayload(eventType, payload)
		}
	case RuntimeError:
		if eventType != EventError || !validCode(value.Code) ||
			!validBoundedText(value.Message, maxDiagnosticMessage, true) {
			return invalidPayload(eventType, payload)
		}
	default:
		return invalidPayload(eventType, payload)
	}

	return nil
}

func validateSubagentLifecycle(eventType EventType, value SubagentLifecycle) error {
	if err := validateSubagentLifecycleFields(value); err != nil {
		return err
	}

	expected, err := expectedSubagentEvent(eventType, value.State)
	if err != nil {
		return err
	}
	if expected != eventType {
		return errors.New("subagent event type and state differ")
	}

	return validateSubagentStateFields(value)
}

//nolint:gocyclo // Closed lifecycle/action enums are validated together at the event boundary.
func validateSubagentLifecycleFields(value SubagentLifecycle) error {
	if validateEventID("child session id", value.ChildSessionID, true) != nil ||
		validateEventID("parent run id", value.ParentRunID, true) != nil ||
		validateOptionalID(value.ChildRunID) != nil ||
		!validIdentifierText(value.Model, maxEventIDBytes, false) ||
		!validBoundedText(value.TaskPreview, 1024, true) ||
		!validBoundedText(value.Activity.Target, 512, true) ||
		value.Turns < 0 || value.ToolCalls < 0 || !validTokenUsage(value.Usage) ||
		value.DurationMillis < 0 || value.DurationMillis > maxEventDurationMS {
		return errors.New("invalid subagent lifecycle fields")
	}
	switch value.Role {
	case subagent.RoleExplore, subagent.RolePlan, subagent.RoleReview:
	default:
		return errors.New("invalid subagent role")
	}
	if !validSubagentActivity(value.Activity) {
		return errors.New("invalid subagent activity")
	}

	return nil
}

func validSubagentActivity(value subagent.ActivitySummary) bool {
	if value.Action == "" {
		return value.Target == ""
	}

	switch value.Action {
	case subagent.ActivityActionRead, subagent.ActivityActionSearch,
		subagent.ActivityActionGlob, subagent.ActivityActionList:
		return value.Target != ""
	default:
		return false
	}
}

func expectedSubagentEvent(eventType EventType, state subagent.State) (EventType, error) {
	switch state {
	case subagent.StateCreated:
		return EventSubagentCreated, nil
	case subagent.StateRunning:
		if eventType == EventSubagentProgress {
			return EventSubagentProgress, nil
		}

		return EventSubagentStarted, nil
	case subagent.StateSucceeded:
		return EventSubagentCompleted, nil
	case subagent.StateFailed:
		return EventSubagentFailed, nil
	case subagent.StateCanceled:
		return EventSubagentCanceled, nil
	case subagent.StateInterrupted:
		return EventSubagentInterrupted, nil
	default:
		return "", errors.New("invalid subagent state")
	}
}

func validateSubagentStateFields(value SubagentLifecycle) error {
	if value.State == subagent.StateCreated {
		if value.ChildRunID != "" || value.Code != "" || value.DurationMillis != 0 {
			return errors.New("invalid created subagent")
		}
		return nil
	}
	if value.State == subagent.StateRunning {
		if value.ChildRunID == "" || value.Code != "" || value.DurationMillis != 0 {
			return errors.New("invalid running subagent")
		}
		return nil
	}
	if !validCode(value.Code) {
		return errors.New("terminal subagent requires a code")
	}
	if value.Stop != "" && !validStopReason(value.Stop) {
		return errors.New("invalid subagent stop reason")
	}

	return nil
}

//nolint:gocyclo // Flat graph invariants are validated together to stay auditable.
func validateSessionTree(tree SessionTree) error {
	if validateEventID("tree session id", tree.SessionID, true) != nil ||
		validateOptionalID(tree.LeafID) != nil ||
		!validBoundedText(tree.Name, maxDiagnosticMessage, true) ||
		tree.TotalNodes < 0 || tree.TotalNodes < len(tree.Nodes) || tree.MaxDepth < 0 ||
		len(tree.Nodes) > maxEventItems {
		return errors.New("invalid session tree")
	}
	seen := make(map[string]struct{}, len(tree.Nodes))
	current := 0
	for _, node := range tree.Nodes {
		if validateEventID("tree node id", node.ID, true) != nil ||
			validateOptionalID(node.ParentID) != nil || !validSessionNodeKind(node.Kind) ||
			node.CreatedAt.IsZero() || node.Depth < 0 || node.Depth > harness.DefaultTreeMaxDepth ||
			!validBoundedText(node.Label, maxDiagnosticMessage, true) {
			return errors.New("invalid session tree node")
		}
		if _, duplicate := seen[node.ID]; duplicate {
			return errors.New("duplicate session tree node")
		}
		if node.ParentID != "" {
			if _, ok := seen[node.ParentID]; !ok && !tree.Truncated {
				return errors.New("session tree parent is missing")
			}
		}
		seen[node.ID] = struct{}{}
		if node.Current {
			current++
			if node.ID != tree.LeafID || !node.OnActivePath {
				return errors.New("session tree current leaf is inconsistent")
			}
		}
	}
	if tree.LeafID != "" && !tree.Truncated && current != 1 {
		return errors.New("session tree current leaf is missing")
	}
	if tree.LeafID == "" && current != 0 {
		return errors.New("root session tree has a current node")
	}

	return nil
}

//nolint:gocyclo // Presence and numeric invariants form one immutable preview contract.
func validateCompactionPreview(value CompactionPreview, requireAvailable bool) error {
	if requireAvailable && !value.Available || value.EstimatedTokens < 0 || value.ThresholdTokens < 0 ||
		value.SummarizedMessages < 0 || value.KeptMessages < 0 ||
		!validBoundedText(value.DisabledReason, maxDiagnosticMessage, true) ||
		validateOptionalID(value.FirstKeptID) != nil ||
		!validIdentifierText(value.Token, 128, !value.Available) {
		return errors.New("invalid compaction preview")
	}
	if value.Available && (value.Token == "" || value.FirstKeptID == "" || value.DisabledReason != "") {
		return errors.New("available compaction preview is incomplete")
	}
	if !value.Available && (value.Token != "" || value.FirstKeptID != "") {
		return errors.New("disabled compaction preview carries a plan")
	}

	return nil
}

func validateEventID(name, value string, required bool) error {
	if value == "" && !required {
		return nil
	}

	if !validIdentifierText(value, maxEventIDBytes, false) {
		return invalidEvent("invalid %s", name)
	}

	return nil
}

func validateOptionalID(value string) error {
	return validateEventID("id", value, false)
}

func validBoundedText(value string, maximum int, allowEmpty bool) bool {
	if (!allowEmpty && strings.TrimSpace(value) == "") || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if character == '\x00' || unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}

	return true
}

func validIdentifierText(value string, maximum int, allowEmpty bool) bool {
	if !validBoundedText(value, maximum, allowEmpty) {
		return false
	}

	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validCode(value string) bool {
	if !validBoundedText(value, 128, false) {
		return false
	}

	for _, character := range value {
		if !unicode.IsLower(character) && !unicode.IsDigit(character) && character != '_' && character != '-' && character != '.' {
			return false
		}
	}

	return true
}

func validProvider(provider ai.Provider) bool {
	if provider == "" {
		return true
	}

	parsed, err := config.ParseProvider(string(provider))

	return err == nil && parsed == provider
}

func validSessionCloseReason(reason SessionCloseReason) bool {
	switch reason {
	case SessionClosedNormally, SessionClosedFailed, SessionClosedCanceled:
		return true
	default:
		return false
	}
}

func validInteractionOutcome(outcome InteractionOutcome) bool {
	switch outcome {
	case InteractionSucceeded, InteractionFailed, InteractionCanceled:
		return true
	default:
		return false
	}
}

func validStopReason(reason agent.StopReason) bool {
	switch reason {
	case agent.StopEndTurn, agent.StopMaxTurns, agent.StopBudget, agent.StopPaused,
		agent.StopWhen, agent.StopTerminated:
		return true
	default:
		return false
	}
}

func validTokenUsage(usage TokenUsage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 &&
		usage.ReasoningTokens >= 0 && usage.CachedInputTokens >= 0 &&
		usage.CacheWriteTokens >= 0
}

func validateMessage(message ai.Message) error {
	switch message.Role {
	case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleTool:
	default:
		return errors.New("invalid role")
	}

	if len(message.Parts) > maxEventItems {
		return errors.New("too many message parts")
	}

	return validateParts(message.Parts, 0)
}

//nolint:gocyclo // The sealed ai.Part taxonomy is exhaustively validated.
func validateParts(parts []ai.Part, depth int) error {
	if depth > 8 {
		return errors.New("part nesting is too deep")
	}

	for _, part := range parts {
		switch value := part.(type) {
		case ai.TextPart:
			if !validBoundedText(value.Text, maxEventTextBytes, true) {
				return errors.New("invalid text part")
			}
		case ai.ImagePart:
			if !validMediaSource(value.Source) {
				return errors.New("image is too large")
			}
		case ai.FilePart:
			if !validMediaSource(value.Source) ||
				!validBoundedText(value.Name, maxEventIDBytes, true) {
				return errors.New("file is too large")
			}
		case ai.ReasoningPart:
			if !validBoundedText(value.Text, maxEventTextBytes, true) ||
				!validBoundedText(value.Signature, maxEventTextBytes, true) {
				return errors.New("invalid reasoning part")
			}
		case ai.ToolCallPart:
			if validateToolCall(toolCallFromAI(value)) != nil {
				return errors.New("invalid tool call part")
			}
		case ai.ToolResultPart:
			if validateEventID("tool call id", value.ToolCallID, true) != nil ||
				!validIdentifierText(value.Name, maxEventIDBytes, false) ||
				len(value.Content) > maxEventItems || validateParts(value.Content, depth+1) != nil {
				return errors.New("invalid tool result part")
			}
		default:
			return fmt.Errorf("unknown part type %T", part)
		}
	}

	return nil
}

func validMediaSource(source ai.MediaSource) bool {
	if len(source.Data) > maxEventTextBytes ||
		!validBoundedText(source.ID, maxEventTextBytes, true) ||
		!validBoundedText(source.URL, maxEventTextBytes, true) ||
		!validIdentifierText(source.MIMEType, maxEventIDBytes, true) {
		return false
	}

	set := 0

	for _, present := range []bool{source.ID != "", source.URL != "", len(source.Data) > 0} {
		if present {
			set++
		}
	}

	return set == 1 && (len(source.Data) == 0 || source.MIMEType != "")
}

//nolint:gocyclo // StreamEvent variants share bounded fields but have distinct required fields.
func validateMessageDelta(delta MessageDelta) error {
	if !validBoundedText(delta.Text, maxEventTextBytes, true) ||
		!validBoundedText(delta.Signature, maxEventTextBytes, true) ||
		!validBoundedText(delta.Arguments, maxEventTextBytes, true) ||
		validateOptionalID(delta.ResponseID) != nil || validateOptionalID(delta.ToolCallID) != nil ||
		!validIdentifierText(delta.Model, maxEventIDBytes, true) ||
		!validIdentifierText(delta.ToolCallName, maxEventIDBytes, true) || delta.ToolCallIndex < 0 ||
		delta.Usage != nil && !validTokenUsage(*delta.Usage) {
		return errors.New("invalid delta field")
	}

	switch delta.Kind {
	case ai.StreamMessageStart:
		if !validProvider(delta.Provider) {
			return errors.New("invalid message-start provider")
		}
	case ai.StreamTextDelta, ai.StreamReasoningDelta, ai.StreamToolCallDelta,
		ai.StreamToolCallEnd, ai.StreamMessageEnd:
	case ai.StreamToolCallStart:
		if delta.ToolCallID == "" || delta.ToolCallName == "" {
			return errors.New("tool-call start is incomplete")
		}
	default:
		return errors.New("unknown delta kind")
	}

	return nil
}

func validateToolCall(call ToolCall) error {
	if validateEventID("tool call id", call.ID, true) != nil ||
		!validIdentifierText(call.Name, maxEventIDBytes, false) || len(call.Arguments) > maxEventTextBytes {
		return errors.New("invalid tool call")
	}

	if len(call.Arguments) > 0 {
		arguments := bytes.TrimSpace(call.Arguments)
		if !json.Valid(arguments) || len(arguments) < 2 || arguments[0] != '{' {
			return errors.New("tool arguments must be a JSON object")
		}
	}

	return nil
}

func validateToolMessage(message ai.Message, callID string, terminal bool) error {
	if err := validateMessage(message); err != nil || message.Role != ai.RoleTool {
		return errors.New("invalid tool message")
	}

	if !terminal {
		return nil
	}

	if len(message.Parts) != 1 {
		return errors.New("terminal tool message must contain one result")
	}

	result, ok := message.Parts[0].(ai.ToolResultPart)
	if !ok || result.ToolCallID != callID {
		return errors.New("tool result does not match call")
	}

	return nil
}

func validateApprovalRequired(value ApprovalRequired) error {
	if validateEventID("request id", value.RequestID, true) != nil ||
		validateEventID("call id", value.CallID, true) != nil ||
		!validIdentifierText(value.Tool, maxEventIDBytes, false) ||
		!validBoundedText(value.CWD, maxEventTextBytes, true) ||
		!validBoundedText(value.Justification, maxDiagnosticMessage, true) ||
		len(value.Command) > maxEventItems || len(value.Choices) == 0 || len(value.Choices) > 8 {
		return errors.New("invalid approval request")
	}

	for _, argument := range value.Command {
		if !validBoundedText(argument, maxEventTextBytes, true) {
			return errors.New("invalid approval command")
		}
	}

	return validateApprovalChoices(value.Choices)
}

func validateApprovalUnknown(value ApprovalUnknown) error {
	if validateEventID("request id", value.RequestID, true) != nil ||
		validateEventID("call id", value.CallID, true) != nil ||
		!validIdentifierText(value.Tool, maxEventIDBytes, false) ||
		!validIdentifierText(value.Fingerprint, 128, false) || value.Attempt < 1 ||
		!validBoundedText(value.Reason, maxDiagnosticMessage, true) ||
		len(value.Choices) == 0 || len(value.Choices) > 8 {
		return errors.New("invalid unknown approval")
	}

	return validateApprovalChoices(value.Choices)
}

func validateApprovalChoices(choices []approval.Choice) error {
	seen := make(map[approval.Choice]struct{}, len(choices))
	for _, choice := range choices {
		if !validApprovalChoice(choice) {
			return errors.New("invalid approval choice")
		}

		if _, exists := seen[choice]; exists {
			return errors.New("duplicate approval choice")
		}

		seen[choice] = struct{}{}
	}

	return nil
}

func validApprovalChoice(choice approval.Choice) bool {
	switch choice {
	case approval.ChoiceAllowOnce, approval.ChoiceAllowSession, approval.ChoiceDeny,
		approval.ChoiceRetry, approval.ChoiceMarkFailed, approval.ChoiceAcknowledge:
		return true
	default:
		return false
	}
}

func validateWorkspaceChanged(value WorkspaceChanged) error {
	if len(value.Entries) > maxEventItems || !validBoundedText(value.Diff, maxEventTextBytes, true) {
		return errors.New("invalid workspace report")
	}

	seen := make(map[string]struct{}, len(value.Entries))
	for _, entry := range value.Entries {
		if !validWorkspacePath(entry.Path) || entry.PreviousPath != "" && !validWorkspacePath(entry.PreviousPath) ||
			!validChangeKind(entry.Kind) {
			return errors.New("invalid workspace change")
		}

		if entry.Kind == changes.KindRenamed {
			if entry.PreviousPath == "" || entry.PreviousPath == entry.Path {
				return errors.New("invalid rename")
			}
		} else if entry.PreviousPath != "" {
			return errors.New("unexpected previous path")
		}

		if _, duplicate := seen[entry.Path]; duplicate {
			return errors.New("duplicate workspace path")
		}

		seen[entry.Path] = struct{}{}
	}

	return nil
}

func validWorkspacePath(value string) bool {
	return validBoundedText(value, maxEventTextBytes, false) && fs.ValidPath(value) &&
		value != "." && !strings.Contains(value, "\\")
}

func validChangeKind(kind changes.Kind) bool {
	switch kind {
	case changes.KindAdded, changes.KindModified, changes.KindDeleted, changes.KindRenamed,
		changes.KindUntracked, changes.KindConflict:
		return true
	default:
		return false
	}
}

func validPhase(phase Phase) bool {
	switch phase {
	case PhaseIdle, PhaseRunning, PhasePaused, PhaseClosing, PhaseClosed:
		return true
	default:
		return false
	}
}

func validateDiagnostic(value IntegrationDiagnostic) error {
	if !validCode(value.Component) || !validCode(value.Code) ||
		!validBoundedText(value.Message, maxDiagnosticMessage, true) {
		return errors.New("invalid diagnostic")
	}

	return nil
}

func invalidPayload(eventType EventType, payload EventPayload) error {
	return invalidEvent("type %q does not accept payload %T", eventType, payload)
}

func invalidEvent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, args...))
}
