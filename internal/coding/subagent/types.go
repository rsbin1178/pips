// Package subagent owns durable, read-only specialist executions for the
// Coding application. It deliberately does not expand the generic agent API.
package subagent

import (
	"context"
	"errors"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

var (
	// ErrInvalid means configuration, request, or durable data is invalid.
	ErrInvalid = errors.New("coding subagent: invalid input")
	// ErrBusy preserves the single-slot compatibility error when a manager is
	// configured with MaxConcurrent equal to one.
	ErrBusy = errors.New("coding subagent: busy")
	// ErrCapacity means the Runtime's concurrent child limit is exhausted.
	ErrCapacity = errors.New("coding subagent: capacity exhausted")
	// ErrSpawnLimit means one root interaction exhausted its child budget.
	ErrSpawnLimit = errors.New("coding subagent: spawn limit exhausted")
	// ErrClosed means the manager no longer accepts executions.
	ErrClosed = errors.New("coding subagent: closed")
	// ErrInvalidResult means a specialist returned malformed or unsafe output.
	ErrInvalidResult = errors.New("coding subagent: invalid result")
)

// Role selects one isolated read-only specialist behavior and output schema.
type Role string

const (
	// RoleExplore gathers evidence from the workspace.
	RoleExplore Role = "explore"
	// RolePlan produces an evidence-backed implementation plan.
	RolePlan Role = "plan"
	// RoleReview reports evidence-backed defects and risks.
	RoleReview Role = "review"
)

// Outcome is the durable terminal classification of an execution.
type Outcome string

const (
	// OutcomeSucceeded means a validated result was committed.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeFailed means execution ended without a usable result.
	OutcomeFailed Outcome = "failed"
	// OutcomeCanceled means cancellation or a deadline stopped execution.
	OutcomeCanceled Outcome = "canceled"
	// OutcomeInterrupted means restart reconciliation found no terminal record.
	OutcomeInterrupted Outcome = "interrupted"
)

// State is the durable lifecycle state of an execution.
type State string

const (
	// StateCreated means durable child ownership has been established.
	StateCreated State = "created"
	// StateRunning means the child Agent run has started.
	StateRunning State = "running"
	// StateSucceeded means the child returned a validated result.
	StateSucceeded State = "succeeded"
	// StateFailed means the child ended without a usable result.
	StateFailed State = "failed"
	// StateCanceled means cancellation or a deadline stopped the child.
	StateCanceled State = "canceled"
	// StateInterrupted means reconciliation closed an orphan execution.
	StateInterrupted State = "interrupted"
)

// Delivery selects whether the parent Tool waits for the child or returns
// immediately while the Runtime retains execution ownership.
type Delivery string

const (
	// DeliveryForeground is the existing run_subagent behavior.
	DeliveryForeground Delivery = "foreground"
	// DeliveryBackground is used by spawn_agent.
	DeliveryBackground Delivery = "background"
)

// Ownership is the immutable parent location of one child execution.
type Ownership struct {
	ParentSessionID     string
	ParentInteractionID string
	ParentRunID         string
	ParentToolCallID    string
	RootInteractionID   string
}

// Request is the compatibility admission shape for one builtin specialist
// delegation. AgentID is the primary selector; Role remains a temporary wire
// alias for explore, plan, and review. Custom definitions are admitted only
// through a Runtime-compiled ExecutionPlan.
type Request struct {
	AgentID   string
	Role      Role
	Task      string
	Ownership Ownership
	Delivery  Delivery
}

// Lifecycle provides Runtime-owned child lifecycle callbacks. Manager only
// transports bounded context and continuation requests; it neither discovers
// nor executes user hook configuration.
type Lifecycle struct {
	BeforeStart func(context.Context, LifecycleStart) string
	BeforeStop  func(context.Context, LifecycleStop) LifecycleStopDecision
}

// LifecycleStart describes a child after its durable session exists and before
// its harness begins its first run.
type LifecycleStart struct {
	ChildSessionID string
	Identity       AgentIdentity
	Role           Role
	Task           string
	Ownership      Ownership
}

// LifecycleStop describes one clean child-agent stop before Manager commits
// its terminal child record.
type LifecycleStop struct {
	ChildSessionID       string
	Identity             AgentIdentity
	Role                 Role
	Ownership            Ownership
	StopHookActive       bool
	LastAssistantMessage string
}

// LifecycleStopDecision asks Manager to issue one follow-up prompt to the
// already-open child harness. A false Continue leaves the child terminal.
type LifecycleStopDecision struct {
	Continue bool
	Reason   string
}

// Result is the terminal execution result returned to the parent Tool.
type Result struct {
	Identity       AgentIdentity
	Role           Role
	ChildSessionID string
	ChildRunID     string
	Outcome        Outcome
	Code           string
	Stop           agent.StopReason
	Turns          int
	ToolCalls      int
	Usage          ai.Usage
	Duration       time.Duration
	Value          any
}

// Event is one content-bounded child lifecycle update.
type Event struct {
	Progress            bool
	State               State
	Identity            AgentIdentity
	Role                Role
	ChildSessionID      string
	ParentSessionID     string
	ParentInteractionID string
	ParentRunID         string
	ParentToolCallID    string
	RootInteractionID   string
	Delivery            Delivery
	ChildRunID          string
	Model               string
	TaskPreview         string
	Activity            ActivitySummary
	Code                string
	Stop                agent.StopReason
	Turns               int
	ToolCalls           int
	Usage               ai.Usage
	Duration            time.Duration
	Time                time.Time
	// Result is present only on an in-process terminal callback. It is never
	// copied into the parent lifecycle event or telemetry projection.
	Result *Result
}

// Observer synchronously receives child lifecycle events. Implementations
// must return quickly, must not retain mutable input references, and return an
// error when the parent event stream can no longer accept the lifecycle.
type Observer func(context.Context, Event) error

// AgentEvent associates one raw child Agent event with its stable child
// Session and immutable ownership.
type AgentEvent struct {
	ChildSessionID string
	Ownership      Ownership
	Task           string
	Event          agent.Event
}

// AgentEventObserver receives raw child events for ordinary Session-state
// projection. Returning an error cancels that child execution.
type AgentEventObserver func(context.Context, AgentEvent) error

// Summary is the durable current-parent list projection.
type Summary struct {
	ChildSessionID string
	Ownership      Ownership
	Delivery       Delivery
	Identity       AgentIdentity
	Role           Role
	State          State
	TaskPreview    string
	Model          string
	CreatedAt      time.Time
	Duration       time.Duration
	Turns          int
	ToolCalls      int
	Usage          ai.Usage
	Code           string
}

// ActivityPhase describes the currently observable child execution phase.
type ActivityPhase string

const (
	// ActivityPhaseUnknown means live phase data is unavailable.
	ActivityPhaseUnknown ActivityPhase = ""
	// ActivityPhaseStarting means the child run is being initialized.
	ActivityPhaseStarting ActivityPhase = "starting"
	// ActivityPhaseThinking means the child is between observable Tool calls.
	ActivityPhaseThinking ActivityPhase = "thinking"
	// ActivityPhaseWorking means the child is executing an observable Tool.
	ActivityPhaseWorking ActivityPhase = "working"
	// ActivityPhaseFinalizing means the child is validating its final result.
	ActivityPhaseFinalizing ActivityPhase = "finalizing"
)

// ActivityAction is one sanitized semantic action suitable for parent UI
// projection. It deliberately excludes raw Tool arguments and results.
type ActivityAction string

const (
	// ActivityActionRead means the child is reading a workspace file.
	ActivityActionRead ActivityAction = "read"
	// ActivityActionSearch means the child is searching workspace text.
	ActivityActionSearch ActivityAction = "search"
	// ActivityActionGlob means the child is matching workspace paths.
	ActivityActionGlob ActivityAction = "glob"
	// ActivityActionList means the child is listing a workspace directory.
	ActivityActionList ActivityAction = "list"
)

// ActivitySummary is a bounded, content-safe description of the child's
// latest observable action. Target is a workspace-relative path or pattern.
type ActivitySummary struct {
	Action ActivityAction `json:"action"`
	Target string         `json:"target"`
}

// ToolStatus is the observable lifecycle of one child Tool call.
type ToolStatus string

const (
	// ToolStatusUnknown means Tool lifecycle data is unavailable.
	ToolStatusUnknown ToolStatus = ""
	// ToolStatusRunning means the Tool has started but has not completed.
	ToolStatusRunning ToolStatus = "running"
	// ToolStatusCompleted means the Tool has produced its terminal result.
	ToolStatusCompleted ToolStatus = "completed"
)

// ToolActivity is one ordered, bounded child Tool observation.
type ToolActivity struct {
	RunID  string
	Turn   int
	Call   ai.ToolCallPart
	Status ToolStatus
	Update []ai.Part
	Result ai.ToolResultPart
}

// Activity is an immutable point-in-time view of an active child execution.
// It is presentation state and is not persisted in the parent lifecycle.
type Activity struct {
	Revision  uint64
	Phase     ActivityPhase
	StartedAt time.Time
	UpdatedAt time.Time
	RunID     string
	Turn      int
	Tools     []ToolActivity
}

// Detail extends Summary with durable replay data and optional live activity.
type Detail struct {
	Summary    Summary
	Plan       ExecutionPlan
	Transcript []ai.Message
	Activity   Activity
	Result     any
}

// Evidence ties one Explore claim to an observed workspace range.
type Evidence struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Claim     string `json:"claim"`
}

// ExploreResult is the structured output of the Explore specialist.
type ExploreResult struct {
	Summary  string     `json:"summary"`
	Evidence []Evidence `json:"evidence"`
	Unknowns []string   `json:"unknowns"`
}

// PlanStep is one implementation step in a Plan result.
type PlanStep struct {
	Title     string   `json:"title"`
	Files     []string `json:"files"`
	Rationale string   `json:"rationale"`
}

// PlanResult is the structured output of the Plan specialist.
type PlanResult struct {
	Summary      string     `json:"summary"`
	Assumptions  []string   `json:"assumptions"`
	Steps        []PlanStep `json:"steps"`
	Risks        []string   `json:"risks"`
	Verification []string   `json:"verification"`
}

// ReviewFinding is one evidence-backed Review issue.
type ReviewFinding struct {
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Evidence       string `json:"evidence"`
	Recommendation string `json:"recommendation"`
}

// ReviewResult is the structured output of the Review specialist.
type ReviewResult struct {
	Summary       string          `json:"summary"`
	Findings      []ReviewFinding `json:"findings"`
	ResidualRisks []string        `json:"residual_risks"`
}
