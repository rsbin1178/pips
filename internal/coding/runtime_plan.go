//nolint:wsl_v5 // Runtime lease checks and plan transitions stay adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/planmode"
)

// PlanDocument is the bounded plan-file view exposed to frontends.
type PlanDocument struct {
	Exists  bool   `json:"exists"`
	Content string `json:"content,omitempty"`
	Size    int64  `json:"size"`
}

// PlanState reports the current plan-mode state. It implements the plan
// tooling Service contract.
func (r *Runtime) PlanState() planmode.State {
	if r == nil {
		return planmode.StateInactive
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.planMode
}

// EnterPlanMode executes a direct enter_plan_mode call. Intercepted calls are
// paused for approval; this path answers calls that need no approval.
func (r *Runtime) EnterPlanMode(ctx context.Context) (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return "", fmt.Errorf("%w: Team Worker has no plan mode", ErrRuntimeInvalid)
	}
	if r.PlanState().Plan() {
		return "Plan mode is already active.", nil
	}
	if err := r.transitionPlanMode(ctx, planmode.StateActive, "", nil); err != nil {
		return "", err
	}

	return planmode.EnterResult, nil
}

// ExitPlanMode executes a direct exit_plan_mode call. Gated calls are paused
// for the plan approval view, so this path only rejects mistaken calls.
func (r *Runtime) ExitPlanMode(context.Context) (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}

	return "", fmt.Errorf("%w: exit_plan_mode requires an active plan decision", ErrRuntimeInvalid)
}

// ReadPlanDocument returns the current session plan-file content without
// disclosing more than the file itself.
func (r *Runtime) ReadPlanDocument(ctx context.Context) (PlanDocument, error) {
	if r == nil {
		return PlanDocument{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return PlanDocument{}, fmt.Errorf("%w: Team Worker has no Plan document", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return PlanDocument{}, stateError("read Plan document", phase, ErrRuntimeClosed)
	}

	store := r.planStore
	r.mu.Unlock()

	if store == nil {
		return PlanDocument{}, fmt.Errorf("%w: Plan storage unavailable", ErrRuntimeInvalid)
	}

	document, err := store.Read(ctx)
	if errors.Is(err, planmode.ErrNotFound) {
		return PlanDocument{}, nil
	}
	if err != nil {
		return PlanDocument{}, fmt.Errorf("%w: Plan document unavailable", ErrRuntimeInvalid)
	}

	return PlanDocument{
		Exists:  true,
		Content: document.Content,
		Size:    document.Size,
	}, nil
}

// PlanDocumentPath returns the session plan-file path for frontend
// presentation. It does not create or read the file.
func (r *Runtime) PlanDocumentPath() (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return "", fmt.Errorf("%w: Team Worker has no Plan document", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return "", stateError("locate Plan document", phase, ErrRuntimeClosed)
	}

	store := r.planStore
	r.mu.Unlock()

	if store == nil {
		return "", fmt.Errorf("%w: Plan storage unavailable", ErrRuntimeInvalid)
	}

	return store.Path(), nil
}

// activatePendingPlanMode promotes Pending to Active when the first prompt of
// a plan-mode interaction starts.
func (r *Runtime) activatePendingPlanMode(ctx context.Context, emitter *eventEmitter) error {
	state := r.PlanState()
	if state != planmode.StatePending {
		return nil
	}

	next, err := state.Activate()
	if err != nil {
		return err
	}

	return r.transitionPlanMode(ctx, next, "", emitter)
}

// transitionPlanMode persists and publishes one plan-mode transition. The
// emitter is nil for transitions that must not publish (direct tool paths).
func (r *Runtime) transitionPlanMode(
	ctx context.Context,
	next planmode.State,
	interactionID string,
	emitter *eventEmitter,
) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	if !next.Valid() {
		return fmt.Errorf("%w: invalid plan mode state", ErrRuntimeInvalid)
	}

	previous := r.PlanState()
	if previous == next {
		return nil
	}

	r.mu.Lock()
	store := r.planStore
	closed := r.closed || r.closing
	r.mu.Unlock()

	if closed {
		return ErrRuntimeClosed
	}
	if store == nil {
		return fmt.Errorf("%w: Plan storage unavailable", ErrRuntimeInvalid)
	}

	if err := store.SaveState(context.WithoutCancel(ctx), next); err != nil {
		return fmt.Errorf("coding runtime: persist plan mode: %w", err)
	}

	if next == planmode.StateActive && previous != planmode.StateActive {
		// Entering plan mode seeds an empty plan file without ever truncating
		// an existing plan.
		if _, err := store.Seed(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("coding runtime: seed plan file: %w", err)
		}
	}

	returning := false
	if next == planmode.StateActive && previous != planmode.StateActive {
		if document, readErr := store.Read(ctx); readErr == nil {
			returning = strings.TrimSpace(document.Content) != ""
		}
	}

	exited := previous.GateArmed() && next == planmode.StateInactive

	r.mu.Lock()
	r.planMode = next
	if returning {
		r.planReturning = true
	}
	if exited {
		r.planExited = true
	}
	r.mu.Unlock()

	if emitter == nil {
		return nil
	}

	if err := emitter.emit(
		interactionID,
		"",
		EventPlanModeChanged,
		PlanModeChanged{State: next},
	); err != nil {
		return err
	}

	if previous.Plan() == next.Plan() {
		return nil
	}

	mode := ModeAgent
	if next.Plan() {
		mode = ModePlan
	}

	return emitter.emit("", "", EventModeChanged, ModeChanged{Mode: mode})
}

// applyConfiguredMode maps the configured operating mode onto the persisted
// plan-mode state. A plan configuration toggles plan mode on; agent turns it
// off.
func applyConfiguredMode(state planmode.State, mode OperatingMode) (planmode.State, error) {
	switch mode {
	case ModePlan:
		return state.ToggleOn()
	case ModeAgent:
		return state.ToggleOff(false)
	default:
		return state, fmt.Errorf("%w: unsupported operating mode %q", ErrRuntimeInvalid, mode)
	}
}
