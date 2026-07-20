// Package approval binds durable user decisions to exact coding operations.
package approval

import (
	"context"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
)

// Handler prepares one controlled tool and renders bounded execution results.
// A Handler is intentionally not an agent.Tool.
type Handler interface {
	Decl() ai.Tool
	Operation(context.Context, agent.ToolCall) (execution.OperationSpec, error)
	Render(execution.Result, error) ([]ai.Part, error)
}

// Journal is the durable session surface required by Controller.
type Journal interface {
	Path() []harness.Entry
	Pending() ([]ai.ToolCallPart, error)
	AppendCustom(string, ai.JSON) (string, error)
}

// Resolver durably answers selected pending calls.
type Resolver interface {
	ResolveToolCalls(...agent.ToolResolution) error
}

// PendingRunner executes an uncontrolled pending call with the application's
// immutable tool/hook snapshot.
type PendingRunner interface {
	RunPending(context.Context, agent.ToolCall, execution.Sink) ([]ai.Part, error)
}

// Kind classifies the action required before conversation continuation.
type Kind string

// Controller states.
const (
	StateReady   Kind = "ready"
	StateReview  Kind = "review"
	StateUnknown Kind = "unknown"
)

// State is one immutable projection of durable approval state.
type State struct {
	Kind    Kind
	Review  *Review
	Unknown *Unknown
}

// NonInteractiveError projects a durable state into the fail-fast contract
// used by non-interactive callers. Interactive callers should render Review
// or Unknown and pass an explicit Resolution instead.
func (s State) NonInteractiveError() error {
	switch s.Kind {
	case StateReady:
		return nil
	case StateReview:
		return ErrApprovalRequired
	case StateUnknown:
		return ErrOutcomeUnknown
	default:
		return ErrJournalCorrupt
	}
}

// Review describes a current pending operation requiring a user decision.
type Review struct {
	RequestID string
	Call      agent.ToolCall
	Operation execution.Operation
}

// Unknown describes an operation that may have started without a durable
// result. Pending false means the original arguments are unavailable.
type Unknown struct {
	RequestID   string
	CallID      string
	Tool        string
	Fingerprint execution.Fingerprint
	Attempt     int
	Pending     bool
	Reason      string
	Recoverable bool
}

// Choice is an explicit user response to Review or Unknown state.
type Choice string

// Supported user choices.
const (
	ChoiceAllowOnce    Choice = "allow_once"
	ChoiceAllowSession Choice = "allow_session"
	ChoiceDeny         Choice = "deny"
	ChoiceRetry        Choice = "retry"
	ChoiceMarkFailed   Choice = "mark_failed"
	ChoiceAcknowledge  Choice = "acknowledge"
)

// Resolution applies one choice to the currently displayed request.
type Resolution struct {
	RequestID string
	Choice    Choice
}
