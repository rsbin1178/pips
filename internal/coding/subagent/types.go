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
	// ErrBusy means the manager already owns its single child execution.
	ErrBusy = errors.New("coding subagent: busy")
	// ErrClosed means the manager no longer accepts executions.
	ErrClosed = errors.New("coding subagent: closed")
	// ErrInvalidResult means a specialist returned malformed or unsafe output.
	ErrInvalidResult = errors.New("coding subagent: invalid result")
)

// Role selects one bounded specialist behavior and output schema.
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

// Request describes one bounded specialist delegation.
type Request struct {
	Role Role
	Task string
}

// Result is the terminal execution result returned to the parent Tool.
type Result struct {
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
	Progress        bool
	State           State
	Role            Role
	ChildSessionID  string
	ParentSessionID string
	ParentRunID     string
	ChildRunID      string
	Model           string
	TaskPreview     string
	Code            string
	Stop            agent.StopReason
	Turns           int
	ToolCalls       int
	Usage           ai.Usage
	Duration        time.Duration
	Time            time.Time
}

// Observer synchronously receives child lifecycle events. Implementations
// must return quickly, must not retain mutable input references, and return an
// error when the parent event stream can no longer accept the lifecycle.
type Observer func(context.Context, Event) error

// Summary is the durable current-parent list projection.
type Summary struct {
	ChildSessionID string
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

// Detail extends Summary with the validated role result and transcript.
type Detail struct {
	Summary    Summary
	Transcript []ai.Message
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
