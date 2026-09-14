// Package planmode owns the plan-mode state machine, the session-bound plan
// file, and the model-facing reminder text.
package planmode

import (
	"errors"
	"fmt"
)

// State is the plan-mode state machine value.
//
//	Inactive    -- normal operating mode; no gate
//	Pending     -- the user toggled plan mode on; no prompt has been sent yet
//	Active      -- the edit gate is armed and the plan reminder is injected
//	ExitPending -- the user toggled plan mode off while a turn is in flight;
//	               the gate holds until the turn completes
type State string

// Plan-mode states.
const (
	StateInactive    State = "inactive"
	StatePending     State = "pending"
	StateActive      State = "active"
	StateExitPending State = "exit_pending"
)

// ErrInvalid reports a malformed state or transition.
var ErrInvalid = errors.New("coding plan mode: invalid value")

// Valid reports whether the value is a known state.
func (s State) Valid() bool {
	switch s {
	case StateInactive, StatePending, StateActive, StateExitPending:
		return true
	default:
		return false
	}
}

// Plan reports whether the state is reported as the plan operating mode.
// Pending and ExitPending both keep the plan flag while they are observable.
func (s State) Plan() bool {
	switch s {
	case StatePending, StateActive, StateExitPending:
		return true
	default:
		return false
	}
}

// GateArmed reports whether file edits outside the plan file are rejected.
func (s State) GateArmed() bool {
	return s == StateActive || s == StateExitPending
}

// Durable returns the state persisted across restarts. Transient states
// (Pending, ExitPending) collapse to Inactive because they depend on
// in-flight interactions.
func (s State) Durable() State {
	if s == StateActive {
		return StateActive
	}

	return StateInactive
}

// ToggleOn is the user-initiated entry transition (/plan, Shift+Tab, ACP
// set_mode plan). From ExitPending it re-arms the active state.
func (s State) ToggleOn() (State, error) {
	switch s {
	case StateInactive:
		return StatePending, nil
	case StatePending:
		return StatePending, nil
	case StateActive:
		return StateActive, nil
	case StateExitPending:
		return StateActive, nil
	default:
		return "", invalidState(s)
	}
}

// ToggleOff is the user-initiated exit transition. inFlight marks that a turn
// is currently running, which defers the exit to the end of the turn.
func (s State) ToggleOff(inFlight bool) (State, error) {
	switch s {
	case StateInactive, StatePending:
		return StateInactive, nil
	case StateActive:
		if inFlight {
			return StateExitPending, nil
		}

		return StateInactive, nil
	case StateExitPending:
		return StateExitPending, nil
	default:
		return "", invalidState(s)
	}
}

// Activate promotes Pending to Active when the first prompt of the plan-mode
// interaction starts. Other states pass through unchanged.
func (s State) Activate() (State, error) {
	switch s {
	case StatePending, StateActive:
		return StateActive, nil
	case StateInactive, StateExitPending:
		return s, nil
	default:
		return "", invalidState(s)
	}
}

// EnterApproved applies an approved enter_plan_mode call, which skips Pending.
func (s State) EnterApproved() (State, error) {
	if !s.Valid() {
		return "", invalidState(s)
	}

	return StateActive, nil
}

// ExitApproved applies an approved or abandoned exit_plan_mode call. Plan mode
// is disabled immediately, even mid-turn.
func (s State) ExitApproved() (State, error) {
	if !s.Valid() {
		return "", invalidState(s)
	}

	return StateInactive, nil
}

// TurnComplete settles ExitPending once its in-flight turn finishes.
func (s State) TurnComplete() (State, error) {
	switch s {
	case StateExitPending:
		return StateInactive, nil
	case StateInactive, StatePending, StateActive:
		return s, nil
	default:
		return "", invalidState(s)
	}
}

func invalidState(state State) error {
	return fmt.Errorf("%w: unknown plan mode state %q", ErrInvalid, string(state))
}
