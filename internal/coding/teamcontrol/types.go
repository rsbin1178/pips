// Package teamcontrol persists operator control intent for Coding Teams.
package teamcontrol

import (
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
)

// Action identifies one operator control operation.
type Action string

// Supported operator control actions.
const (
	ActionMessage          Action = "message"
	ActionFollowUp         Action = "follow_up"
	ActionInterruptAttempt Action = "interrupt_attempt"
	ActionCancelTask       Action = "cancel_task"
	ActionRetryTask        Action = "retry_task"
	ActionCancelTeam       Action = "cancel_team"
	ActionResolveApproval  Action = "resolve_approval"
	ActionResolveQuestion  Action = "resolve_question"
	ActionRejectQuestion   Action = "reject_question"
)

// State is the durable lifecycle of one operator command.
type State string

// Operator command states.
const (
	StatePending         State = "pending"
	StateApplying        State = "applying"
	StateApplied         State = "applied"
	StateRejected        State = "rejected"
	StateStale           State = "stale"
	StateDeliveryUnknown State = "delivery_unknown"
)

// Target carries stable logical identity supplied by the operator.
type Target struct {
	TeamID            team.ID        `json:"team_id"`
	MemberID          team.MemberID  `json:"member_id,omitempty"`
	TaskID            team.TaskID    `json:"task_id,omitempty"`
	ExpectedAttemptID team.AttemptID `json:"expected_attempt_id,omitempty"`
	OwnerGeneration   uint64         `json:"owner_generation,omitempty"`
}

// ResolvedTarget is the exact execution identity selected before a side effect.
type ResolvedTarget struct {
	TeamID          team.ID         `json:"team_id"`
	MemberID        team.MemberID   `json:"member_id,omitempty"`
	TaskID          team.TaskID     `json:"task_id,omitempty"`
	AttemptID       team.AttemptID  `json:"attempt_id,omitempty"`
	ContinuationID  continuation.ID `json:"continuation_id,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	OwnerGeneration uint64          `json:"owner_generation,omitempty"`
}

// Command is immutable operator intent.
type Command struct {
	ID        team.CommandID `json:"id"`
	Action    Action         `json:"action"`
	Target    Target         `json:"target"`
	Text      string         `json:"text,omitempty"`
	Payload   ai.JSON        `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Entry is the latest durable state of one operator command.
type Entry struct {
	Command   Command         `json:"command"`
	State     State           `json:"state"`
	Resolved  *ResolvedTarget `json:"resolved,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Revision is an optimistic control-journal version.
type Revision uint64

// Mutation identifies one idempotent control-journal transition.
type Mutation struct {
	ID               team.CommandID `json:"id"`
	ExpectedRevision Revision       `json:"expected_revision"`
}

// Record is one committed control-journal transition.
type Record struct {
	Revision Revision `json:"revision"`
	Entry    Entry    `json:"entry"`
}

// ListOptions selects a bounded transition page after an exclusive revision.
type ListOptions struct {
	AfterRevision Revision
	Limit         int
}

// Page is one ordered control-journal transition page.
type Page struct {
	Records   []Record
	NextAfter Revision
}

// Limits bound a private control journal.
type Limits struct {
	MaxFileBytes   int64
	MaxRecordBytes int
	MaxRecords     int
	MaxTextBytes   int
	MaxListResults int
}

// DefaultLimits returns production control-journal bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes: 16 << 20, MaxRecordBytes: 1 << 20, MaxRecords: 10_000,
		MaxTextBytes: 32 << 10, MaxListResults: 1_000,
	}
}

// InterruptedDisposition describes safe restart handling for an applying entry.
type InterruptedDisposition string

// Interrupted command dispositions.
const (
	DispositionNone            InterruptedDisposition = "none"
	DispositionReconcile       InterruptedDisposition = "reconcile"
	DispositionDeliveryUnknown InterruptedDisposition = "delivery_unknown"
)
