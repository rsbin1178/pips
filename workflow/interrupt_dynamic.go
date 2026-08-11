package workflow

import (
	"context"
	"errors"
	"slices"
	"time"
)

type invocationInterruptKey struct{}

type invocationInterruptContext struct {
	address          NodeAddress
	previous         *dynamicInterruptCheckpoint
	target           *ResumeTarget
	descendantTarget bool
}

type dynamicInterruptError struct {
	points []dynamicInterruptCheckpoint
	local  *dynamicLocalCheckpoint
}

type dynamicInterruptCheckpoint struct {
	ID      string      `json:"id"`
	Address NodeAddress `json:"address"`
	Info    Value       `json:"info"`
	State   *Value      `json:"state,omitempty"`
}

type dynamicLocalCheckpoint struct {
	Info  Value  `json:"info"`
	State *Value `json:"state,omitempty"`
}

func (e *dynamicInterruptError) Error() string {
	return ErrInterrupted.Error()
}

func (e *dynamicInterruptError) Unwrap() error {
	return ErrInterrupted
}

// Interrupt suspends the current invocation with caller-facing information.
func Interrupt(ctx context.Context, info Value) error {
	return newDynamicInterrupt(ctx, info, nil)
}

// StatefulInterrupt suspends the current invocation and persists private local
// state for GetInterruptState on a targeted resume.
func StatefulInterrupt(ctx context.Context, info, state Value) error {
	if !state.IsValid() {
		return errors.New("workflow: invalid interrupt state")
	}

	cloned := state

	return newDynamicInterrupt(ctx, info, &cloned)
}

// CompositeInterrupt propagates descendant interruption errors while saving
// local composite state. Ordinary errors are rejected.
func CompositeInterrupt(
	ctx context.Context,
	info Value,
	state Value,
	causes ...error,
) error {
	control, ok := ctx.Value(invocationInterruptKey{}).(*invocationInterruptContext)
	if !ok || control == nil {
		return errors.New("workflow: interrupt context is unavailable")
	}

	if !info.IsValid() || !state.IsValid() {
		return errors.New("workflow: invalid composite interrupt value")
	}

	if len(causes) == 0 {
		return errors.New("workflow: composite interrupt requires a descendant")
	}

	points := make([]dynamicInterruptCheckpoint, 0, len(causes))
	for _, cause := range causes {
		var interruption *dynamicInterruptError
		if !errors.As(cause, &interruption) {
			return errors.New("workflow: composite interrupt contains a non-interrupt error")
		}

		points = mergeDynamicInterrupts(points, interruption.points)
	}

	localState := state

	return &dynamicInterruptError{
		points: points,
		local:  &dynamicLocalCheckpoint{Info: info, State: &localState},
	}
}

// GetInterruptState reports state saved by the previously interrupted direct
// invocation.
func GetInterruptState(ctx context.Context) (
	wasInterrupted bool,
	hasState bool,
	state Value,
) {
	control, ok := ctx.Value(invocationInterruptKey{}).(*invocationInterruptContext)
	if !ok || control == nil || control.previous == nil {
		return false, false, Value{}
	}

	if control.previous.State == nil {
		return true, false, Value{}
	}

	return true, true, *control.previous.State
}

// GetResumeContext reports whether the current invocation is a direct target
// or an ancestor of one. Only a direct target receives data.
func GetResumeContext(ctx context.Context) (
	isResumeTarget bool,
	hasData bool,
	data Value,
) {
	control, ok := ctx.Value(invocationInterruptKey{}).(*invocationInterruptContext)
	if !ok || control == nil {
		return false, false, Value{}
	}

	if control.target != nil {
		if control.target.Data.IsValid() {
			return true, true, control.target.Data
		}

		return true, false, Value{}
	}

	return control.descendantTarget, false, Value{}
}

func newDynamicInterrupt(ctx context.Context, info Value, state *Value) error {
	control, ok := ctx.Value(invocationInterruptKey{}).(*invocationInterruptContext)
	if !ok || control == nil {
		return errors.New("workflow: interrupt context is unavailable")
	}

	if !info.IsValid() {
		return errors.New("workflow: invalid interrupt info")
	}

	id, err := randomRunID(time.Time{})
	if err != nil {
		return errors.New("workflow: create interrupt id")
	}

	point := dynamicInterruptCheckpoint{
		ID: id, Address: cloneNodeAddress(control.address), Info: info,
	}

	if state != nil {
		cloned := *state
		point.State = &cloned
	}

	return &dynamicInterruptError{points: []dynamicInterruptCheckpoint{point}}
}

func newInvocationInterruptContext(
	runtime *nodeRuntime,
) *invocationInterruptContext {
	control := &invocationInterruptContext{address: NodeAddress{
		NodeID: runtime.nodeID,
		Scope:  slices.Clone(runtime.execution.scope),
	}}

	if runtime.resume == nil {
		return control
	}

	for index := range runtime.resume.Dynamic {
		point := &runtime.resume.Dynamic[index]

		target, targeted := runtime.execution.state.resumeTargets[point.ID]
		if !targeted {
			continue
		}

		control.descendantTarget = true
		if nodeAddressEqual(point.Address, control.address) {
			cloned := cloneDynamicInterrupt(*point)
			control.previous = &cloned
			targetCopy := target
			control.target = &targetCopy
		}
	}

	if runtime.resume.Local != nil {
		control.previous = &dynamicInterruptCheckpoint{
			Address: cloneNodeAddress(control.address),
			Info:    runtime.resume.Local.Info,
			State:   cloneValuePointer(runtime.resume.Local.State),
		}
	}

	return control
}

func remainingDynamicInterrupts(
	resume *pausedNodeCheckpoint,
	targets map[string]ResumeTarget,
) []dynamicInterruptCheckpoint {
	if resume == nil {
		return nil
	}

	remaining := make([]dynamicInterruptCheckpoint, 0, len(resume.Dynamic))
	for _, interruption := range resume.Dynamic {
		if _, targeted := targets[interruption.ID]; !targeted {
			remaining = append(remaining, cloneDynamicInterrupt(interruption))
		}
	}

	return remaining
}

func mergeDynamicInterrupts(
	left []dynamicInterruptCheckpoint,
	right []dynamicInterruptCheckpoint,
) []dynamicInterruptCheckpoint {
	merged := cloneDynamicInterrupts(left)

	seen := make(map[string]struct{}, len(left)+len(right))
	for _, interruption := range merged {
		seen[interruption.ID] = struct{}{}
	}

	for _, interruption := range right {
		if _, duplicate := seen[interruption.ID]; duplicate {
			continue
		}

		seen[interruption.ID] = struct{}{}
		merged = append(merged, cloneDynamicInterrupt(interruption))
	}

	return merged
}

func nodeAddressEqual(left, right NodeAddress) bool {
	return left.NodeID == right.NodeID && slices.Equal(left.Scope, right.Scope)
}

func cloneDynamicInterrupts(
	interruptions []dynamicInterruptCheckpoint,
) []dynamicInterruptCheckpoint {
	cloned := make([]dynamicInterruptCheckpoint, len(interruptions))
	for index, interruption := range interruptions {
		cloned[index] = cloneDynamicInterrupt(interruption)
	}

	return cloned
}

func cloneDynamicInterrupt(
	interruption dynamicInterruptCheckpoint,
) dynamicInterruptCheckpoint {
	interruption.Address = cloneNodeAddress(interruption.Address)
	interruption.State = cloneValuePointer(interruption.State)

	return interruption
}

func cloneValuePointer(value *Value) *Value {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

func cloneDynamicLocal(local *dynamicLocalCheckpoint) *dynamicLocalCheckpoint {
	if local == nil {
		return nil
	}

	return &dynamicLocalCheckpoint{Info: local.Info, State: cloneValuePointer(local.State)}
}
