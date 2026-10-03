//nolint:wsl_v5 // Lifecycle guards and durable boundary checks stay adjacent.
package coding

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/goal"
)

// Goal control errors.
var (
	ErrGoalExists     = errors.New("coding goal: clear the existing Goal before replacing it")
	ErrGoalMissing    = errors.New("coding goal: no Goal to resume")
	ErrGoalIncomplete = errors.New("coding goal: completion is not verified")
)

// GoalRequest is a session-scoped completion condition and optional cumulative
// observed token allowance. Zero MaxTokens leaves the 25-Work bound in force.
type GoalRequest struct {
	Condition string
	MaxTokens int
}

// Validate permits CLI preflight without opening or trusting a workspace.
func (request GoalRequest) Validate() error {
	if request.MaxTokens < 0 {
		return fmt.Errorf("%w: negative Goal token budget", ErrRuntimeInvalid)
	}
	if err := ValidatePromptText(request.Condition); err != nil {
		return err
	}
	_, err := goal.Prepare(request.Condition)
	return err
}

// GoalStatus is the session Goal lifecycle projection.
type GoalStatus string

// Goal lifecycle states.
const (
	GoalReady       GoalStatus = "ready"
	GoalRunning     GoalStatus = "running"
	GoalAssessing   GoalStatus = "assessing"
	GoalVerifying   GoalStatus = "verifying"
	GoalWaiting     GoalStatus = "waiting"
	GoalPaused      GoalStatus = "paused"
	GoalBlocked     GoalStatus = "blocked"
	GoalInterrupted GoalStatus = "interrupted"
	GoalCompleted   GoalStatus = "completed"
	GoalCancelled   GoalStatus = "cancelled"
	GoalFailed      GoalStatus = "failed"
	GoalLimited     GoalStatus = "limited"
)

// GoalState is a defensive projection, never the control authority. Tokens
// includes successful Work/check usage and separately observed failed checks;
// it is not a claim of complete provider billing.
type GoalState struct {
	ID                string     `json:"id,omitempty"`
	Condition         string     `json:"condition,omitempty"`
	Status            GoalStatus `json:"status,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	Attempts          int        `json:"attempts"`
	Evaluations       int        `json:"evaluations"`
	Tokens            int        `json:"tokens"`
	MaxTokens         int        `json:"max_tokens"`
	FailedCheckTokens int        `json:"failed_check_tokens"`
	Gaps              []string   `json:"gaps,omitempty"`
	References        []string   `json:"references,omitempty"`
	Revision          uint64     `json:"revision,omitempty"`
}

// Active reports a nonterminal Goal, including one requiring explicit resume.
func (state GoalState) Active() bool {
	return state.ID != "" && state.Status != GoalCompleted && state.Status != GoalCancelled &&
		state.Status != GoalFailed && state.Status != GoalLimited
}

// Completed reports independently verified completion, not interaction success.
func (state GoalState) Completed() bool { return state.ID != "" && state.Status == GoalCompleted }

// Clone returns an independently owned projection.
func (state GoalState) Clone() GoalState {
	state.Gaps = slices.Clone(state.Gaps)
	state.References = slices.Clone(state.References)
	return state
}

// NonInteractiveError rejects every unverified nonempty Goal outcome.
func (state GoalState) NonInteractiveError() error {
	if state.ID == "" || state.Completed() {
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrGoalIncomplete, state.Status, state.Reason)
}

// GoalChanged replaces the session-local Goal projection. Empty State records
// an explicit clear. Events do not carry raw evidence or become control records.
type GoalChanged struct {
	State    GoalState `json:"state"`
	Redacted bool      `json:"redacted,omitempty"`
}

func (GoalChanged) eventPayload() {}

// EventGoalChanged replaces the session Goal projection.
const EventGoalChanged EventType = "goal.changed"

//nolint:gocyclo // Validate every independent bounded protocol field.
func validateGoalState(state GoalState) error {
	if state.ID == "" {
		if state.Status != "" || state.Condition != "" || state.Revision != 0 || state.Attempts != 0 || state.Tokens != 0 || state.Evaluations != 0 || state.MaxTokens != 0 || state.FailedCheckTokens != 0 || state.Reason != "" || len(state.Gaps) != 0 || len(state.References) != 0 {
			return invalidEvent("invalid empty Goal")
		}
		return nil
	}
	if validateEventID("goal id", state.ID, true) != nil || state.Revision == 0 || state.Attempts < 0 || state.Evaluations < 0 || state.Tokens < 0 || state.MaxTokens < 0 || state.FailedCheckTokens < 0 {
		return invalidEvent("invalid Goal metadata")
	}
	if _, err := goal.Prepare(state.Condition); err != nil {
		return invalidEvent("invalid Goal condition")
	}
	if len(state.Reason) > 4096 || !utf8.ValidString(state.Reason) || len(state.Gaps) > 16 || len(state.References) > 32 {
		return invalidEvent("oversized Goal state")
	}
	for _, gap := range state.Gaps {
		if strings.TrimSpace(gap) == "" || len(gap) > 4096 {
			return invalidEvent("invalid Goal gap")
		}
	}
	for _, reference := range state.References {
		if validateEventID("goal reference", reference, true) != nil {
			return invalidEvent("invalid Goal reference")
		}
	}
	switch state.Status {
	case GoalReady, GoalRunning, GoalAssessing, GoalVerifying, GoalWaiting, GoalPaused, GoalBlocked,
		GoalInterrupted, GoalCompleted, GoalCancelled, GoalFailed, GoalLimited:
		return nil
	default:
		return invalidEvent("unknown Goal status")
	}
}
