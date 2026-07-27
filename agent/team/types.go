package team

import (
	"context"
	"slices"
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

const schemaVersion = 2

// ID identifies one durable Team aggregate.
type ID string

// MemberID identifies one logical Agent member within a Team.
type MemberID string

// TaskID identifies one Team task.
type TaskID string

// AttemptID identifies one external execution attempt for a task.
type AttemptID string

// MessageID identifies one immutable direct message.
type MessageID string

// CommandID is a stable idempotency key for one Team mutation.
type CommandID string

// EventID identifies one committed Team transition.
type EventID string

// Revision is an optimistic concurrency version.
type Revision uint64

// Status is the Team lifecycle state.
type Status string

// Team statuses.
const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// MemberStatus is a logical Team member's availability.
type MemberStatus string

// Member statuses.
const (
	MemberStatusActive   MemberStatus = "active"
	MemberStatusDisabled MemberStatus = "disabled"
)

// TaskStatus is a task's durable coordination state.
type TaskStatus string

// Task statuses.
const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusReady     TaskStatus = "ready"
	TaskStatusClaimed   TaskStatus = "claimed"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// AttemptStatus is one task attempt's durable state.
type AttemptStatus string

// Attempt statuses.
const (
	AttemptStatusRunning   AttemptStatus = "running"
	AttemptStatusCompleted AttemptStatus = "completed"
	AttemptStatusFailed    AttemptStatus = "failed"
	AttemptStatusCancelled AttemptStatus = "cancelled"
)

// AttemptOutcome is an accepted FinishTaskAttempt outcome.
type AttemptOutcome string

// FinishTaskAttempt outcomes.
const (
	AttemptOutcomeCompleted AttemptOutcome = "completed"
	AttemptOutcomeFailed    AttemptOutcome = "failed"
)

// ActorKind identifies the authority issuing a command.
type ActorKind string

// Actor kinds.
const (
	ActorKindCoordinator ActorKind = "coordinator"
	ActorKindMember      ActorKind = "member"
)

// Actor is durable audit identity. For ActorKindMember, ID is a MemberID.
type Actor struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
}

// CommandMetadata identifies and orders one mutation.
type CommandMetadata struct {
	ID               CommandID
	ExpectedRevision Revision
	Actor            Actor
}

// Limits are stored hard bounds for one Team. Zero fields use defaults.
type Limits struct {
	MaxMembers             int `json:"max_members"`
	MaxTasks               int `json:"max_tasks"`
	MaxDependenciesPerTask int `json:"max_dependencies_per_task"`
	MaxMessages            int `json:"max_messages"`
	MaxActiveTasks         int `json:"max_active_tasks"`
	MaxAttemptsPerTask     int `json:"max_attempts_per_task"`
	MaxArtifactsPerResult  int `json:"max_artifacts_per_result"`
	MaxJSONBytes           int `json:"max_json_bytes"`
}

// Artifact is an opaque coordinator-managed result reference. It must not contain
// credentials or raw secret material.
type Artifact struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Digest    string `json:"digest,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

// MemberSpec registers one coordinator-managed Agent resource with a Team.
type MemberSpec struct {
	ID                   MemberID `json:"id"`
	Name                 string   `json:"name"`
	Role                 string   `json:"role"`
	CapabilityProfileRef string   `json:"capability_profile_ref,omitempty"`
}

// Member is one durable logical Agent identity.
type Member struct {
	ID                   MemberID     `json:"id"`
	Name                 string       `json:"name"`
	Role                 string       `json:"role"`
	CapabilityProfileRef string       `json:"capability_profile_ref,omitempty"`
	Status               MemberStatus `json:"status"`
	MailboxDelivered     uint64       `json:"mailbox_delivered"`
	MailboxAcknowledged  uint64       `json:"mailbox_acknowledged"`
	RegisteredAt         time.Time    `json:"registered_at"`
	DisabledAt           time.Time    `json:"disabled_at,omitzero"`
}

// Attempt records one coordinator-started task execution.
type Attempt struct {
	ID             AttemptID       `json:"id"`
	Number         int             `json:"number"`
	Status         AttemptStatus   `json:"status"`
	MemberID       MemberID        `json:"member_id"`
	ContinuationID continuation.ID `json:"continuation_id"`
	Result         ai.JSON         `json:"result,omitempty"`
	Artifacts      []Artifact      `json:"artifacts,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at,omitzero"`
}

// Task is one immutable work definition plus mutable coordination state.
type Task struct {
	ID               TaskID     `json:"id"`
	Title            string     `json:"title"`
	Description      string     `json:"description,omitempty"`
	Payload          ai.JSON    `json:"payload,omitempty"`
	DependencyIDs    []TaskID   `json:"dependency_ids,omitempty"`
	AssignedMemberID MemberID   `json:"assigned_member_id,omitempty"`
	ClaimedMemberID  MemberID   `json:"claimed_member_id,omitempty"`
	Status           TaskStatus `json:"status"`
	AttemptLimit     int        `json:"attempt_limit"`
	Attempts         []Attempt  `json:"attempts,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// Message is one immutable, direct member message.
type Message struct {
	ID          MessageID `json:"id"`
	Sequence    uint64    `json:"sequence"`
	SenderID    MemberID  `json:"sender_id"`
	RecipientID MemberID  `json:"recipient_id"`
	TaskID      TaskID    `json:"task_id,omitempty"`
	ReplyToID   MessageID `json:"reply_to_id,omitempty"`
	Body        ai.JSON   `json:"body"`
	SentAt      time.Time `json:"sent_at"`
}

// Team is the latest full aggregate snapshot.
type Team struct {
	SchemaVersion       int        `json:"schema_version"`
	ID                  ID         `json:"id"`
	Revision            Revision   `json:"revision"`
	Status              Status     `json:"status"`
	Objective           string     `json:"objective"`
	LeadMemberID        MemberID   `json:"lead_member_id"`
	Members             []Member   `json:"members"`
	Tasks               []Task     `json:"tasks"`
	NextMessageSequence uint64     `json:"next_message_sequence"`
	Limits              Limits     `json:"limits"`
	Reason              string     `json:"reason,omitempty"`
	Output              ai.JSON    `json:"output,omitempty"`
	Artifacts           []Artifact `json:"artifacts,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// Cause identifies one committed domain transition.
type Cause string

// Transition causes.
const (
	CauseCreate               Cause = "create"
	CauseMemberRegistered     Cause = "member_registered"
	CauseMemberDisabled       Cause = "member_disabled"
	CauseMemberEnabled        Cause = "member_enabled"
	CauseTaskCreated          Cause = "task_created"
	CauseTaskAssigned         Cause = "task_assigned"
	CauseTaskUnassigned       Cause = "task_unassigned"
	CauseTaskClaimed          Cause = "task_claimed"
	CauseTaskReleased         Cause = "task_released"
	CauseTaskAttemptStarted   Cause = "task_attempt_started"
	CauseTaskAttemptCompleted Cause = "task_attempt_completed"
	CauseTaskAttemptFailed    Cause = "task_attempt_failed"
	CauseTaskRetried          Cause = "task_retried"
	CauseTaskCancelled        Cause = "task_cancelled"
	CauseMessageSent          Cause = "message_sent"
	CauseMessagesAcknowledged Cause = "messages_acknowledged"
	CauseTeamCompleted        Cause = "team_completed"
	CauseTeamFailed           Cause = "team_failed"
	CauseTeamCancelled        Cause = "team_cancelled"
)

// Transition is operational audit data for one Team revision.
type Transition struct {
	SchemaVersion int       `json:"schema_version"`
	ID            EventID   `json:"id"`
	TeamID        ID        `json:"team_id"`
	Revision      Revision  `json:"revision"`
	At            time.Time `json:"at"`
	Actor         Actor     `json:"actor"`
	CommandID     CommandID `json:"command_id"`
	CommandHash   string    `json:"command_hash"`
	Cause         Cause     `json:"cause"`
	TaskID        TaskID    `json:"task_id,omitempty"`
	MemberID      MemberID  `json:"member_id,omitempty"`
	AttemptID     AttemptID `json:"attempt_id,omitempty"`
	MessageID     MessageID `json:"message_id,omitempty"`
	From          Status    `json:"from,omitempty"`
	To            Status    `json:"to"`
	Reason        string    `json:"reason,omitempty"`
}

// Record is one full post-command coordination snapshot plus an optional
// immutable message delta.
type Record struct {
	SchemaVersion int        `json:"schema_version"`
	Team          Team       `json:"team"`
	Transition    Transition `json:"transition"`
	Message       *Message   `json:"message,omitempty"`
}

// ListOptions bounds one lexicographically ordered Store page.
type ListOptions struct {
	Limit  int
	Cursor string
}

// ListPage is one bounded page of current Team snapshots.
type ListPage struct {
	Teams      []Team
	NextCursor string
}

// MailboxOptions selects one recipient page after an exclusive sequence.
type MailboxOptions struct {
	AfterSequence uint64
	Limit         int
}

// MessagePage is one ordered recipient mailbox page.
type MessagePage struct {
	Messages  []Message
	NextAfter uint64
}

// ChangeOptions selects transitions after an exclusive Team revision.
type ChangeOptions struct {
	AfterRevision Revision
	Limit         int
}

// Change is one durable Team transition and its optional message delta.
type Change struct {
	Transition Transition
	Message    *Message
}

// ChangePage is one bounded page of Team changes.
type ChangePage struct {
	Changes   []Change
	NextAfter Revision
}

// Dispatch is immutable coordinator input for one committed task attempt.
type Dispatch struct {
	TeamID               ID
	TeamRevision         Revision
	Objective            string
	MemberID             MemberID
	MemberName           string
	MemberRole           string
	CapabilityProfileRef string
	TaskID               TaskID
	AttemptID            AttemptID
	ContinuationID       continuation.ID
	Title                string
	Description          string
	Payload              ai.JSON
	DependencyIDs        []TaskID
}

// TaskAttemptStart is the exact post-command Team and its Dispatch.
type TaskAttemptStart struct {
	Team     Team
	Dispatch Dispatch
}

// MessageSend is the exact post-command Team and committed Message.
type MessageSend struct {
	Team    Team
	Message Message
}

// ExecutionReader is the finite continuation lookup used for recovery
// inspection. continuation.Engine implements it.
type ExecutionReader interface {
	Get(context.Context, continuation.ID) (continuation.Execution, error)
}

// AttemptInspectionState classifies one referenced child execution.
type AttemptInspectionState string

// Attempt inspection states.
const (
	AttemptInspectionMissing     AttemptInspectionState = "missing"
	AttemptInspectionNonterminal AttemptInspectionState = "nonterminal"
	AttemptInspectionTerminal    AttemptInspectionState = "terminal"
)

// AttemptInspection is a read-only cross-store recovery projection.
type AttemptInspection struct {
	Dispatch  Dispatch
	State     AttemptInspectionState
	Execution *continuation.Execution
}

// CreateRequest creates one Team with exactly one lead member.
type CreateRequest struct {
	Command   CommandMetadata
	ID        ID
	Objective string
	Lead      MemberSpec
	Limits    Limits
}

// RegisterMemberRequest registers one coordinator-managed member resource.
type RegisterMemberRequest struct {
	Command CommandMetadata
	Member  MemberSpec
}

// DisableMemberRequest disables an idle non-lead member.
type DisableMemberRequest struct {
	Command  CommandMetadata
	MemberID MemberID
	Reason   string
}

// EnableMemberRequest re-enables one disabled member.
type EnableMemberRequest struct {
	Command  CommandMetadata
	MemberID MemberID
}

// CreateTaskRequest creates one immutable task definition.
type CreateTaskRequest struct {
	Command      CommandMetadata
	TaskID       TaskID
	Title        string
	Description  string
	Payload      ai.JSON
	Dependencies []TaskID
	AttemptLimit int
}

// AssignTaskRequest exclusively assigns an unclaimed task.
type AssignTaskRequest struct {
	Command  CommandMetadata
	TaskID   TaskID
	MemberID MemberID
}

// UnassignTaskRequest removes an unclaimed task assignment.
type UnassignTaskRequest struct {
	Command CommandMetadata
	TaskID  TaskID
}

// ClaimTaskRequest claims one ready task for its member actor.
type ClaimTaskRequest struct {
	Command CommandMetadata
	TaskID  TaskID
}

// ReleaseTaskRequest releases one unstarted claim.
type ReleaseTaskRequest struct {
	Command CommandMetadata
	TaskID  TaskID
	Reason  string
}

// StartTaskAttemptRequest durably binds one claimed task to a continuation.
type StartTaskAttemptRequest struct {
	Command        CommandMetadata
	TaskID         TaskID
	AttemptID      AttemptID
	ContinuationID continuation.ID
}

// FinishTaskAttemptRequest commits the current attempt outcome.
type FinishTaskAttemptRequest struct {
	Command        CommandMetadata
	TaskID         TaskID
	AttemptID      AttemptID
	ContinuationID continuation.ID
	Outcome        AttemptOutcome
	Result         ai.JSON
	Artifacts      []Artifact
	Reason         string
}

// RetryTaskRequest returns one failed task to pending or ready.
type RetryTaskRequest struct {
	Command CommandMetadata
	TaskID  TaskID
	Reason  string
}

// CancelTaskRequest explicitly cancels one non-completed task.
type CancelTaskRequest struct {
	Command CommandMetadata
	TaskID  TaskID
	Reason  string
}

// SendMessageRequest commits one immutable direct message.
type SendMessageRequest struct {
	Command     CommandMetadata
	MessageID   MessageID
	RecipientID MemberID
	TaskID      TaskID
	ReplyToID   MessageID
	Body        ai.JSON
}

// AcknowledgeMessagesRequest advances the member actor's mailbox cursor.
type AcknowledgeMessagesRequest struct {
	Command         CommandMetadata
	ThroughSequence uint64
}

// CompleteTeamRequest explicitly completes one fully settled Team.
type CompleteTeamRequest struct {
	Command   CommandMetadata
	Output    ai.JSON
	Artifacts []Artifact
	Reason    string
}

// FailTeamRequest explicitly fails a Team and cancels its active work.
type FailTeamRequest struct {
	Command CommandMetadata
	Reason  string
}

// CancelTeamRequest explicitly cancels a Team and its active work.
type CancelTeamRequest struct {
	Command CommandMetadata
	Reason  string
}

func cloneJSON(value ai.JSON) ai.JSON { return slices.Clone(value) }
