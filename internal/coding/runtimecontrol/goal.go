package runtimecontrol

import (
	"context"
	"fmt"
	"iter"

	"github.com/rsbin1178/pips/internal/coding"
)

type goalRuntime interface {
	StartGoal(context.Context, coding.GoalRequest) iter.Seq2[coding.Event, error]
	ResumeGoal(context.Context) iter.Seq2[coding.Event, error]
	PauseGoal(context.Context) error
	ClearGoal(context.Context) error
}

// StartGoal starts a bounded goal under the ordinary Runtime replacement lease.
func (c *Controller) StartGoal(ctx context.Context, request coding.GoalRequest) iter.Seq2[coding.Event, error] {
	return c.sequence(func(runtime runtimeInstance) iter.Seq2[coding.Event, error] {
		goals, ok := runtime.(goalRuntime)
		if !ok {
			return errorSequence(fmt.Errorf("%w: runtime does not expose goals", ErrInvalid))
		}

		return goals.StartGoal(ctx, request)
	})
}

// ResumeGoal explicitly resumes the current session's retained goal.
func (c *Controller) ResumeGoal(ctx context.Context) iter.Seq2[coding.Event, error] {
	return c.sequence(func(runtime runtimeInstance) iter.Seq2[coding.Event, error] {
		goals, ok := runtime.(goalRuntime)
		if !ok {
			return errorSequence(fmt.Errorf("%w: runtime does not expose goals", ErrInvalid))
		}

		return goals.ResumeGoal(ctx)
	})
}

// PauseGoal stops autonomous work without discarding the durable goal.
func (c *Controller) PauseGoal(ctx context.Context) error {
	return c.controlGoal(ctx, false)
}

// ClearGoal cancels the goal without deleting its audit history.
func (c *Controller) ClearGoal(ctx context.Context) error {
	return c.controlGoal(ctx, true)
}

func (c *Controller) controlGoal(ctx context.Context, clearGoal bool) error {
	return c.withRuntime(func(runtime runtimeInstance) error {
		goals, ok := runtime.(goalRuntime)
		if !ok {
			return fmt.Errorf("%w: runtime does not expose goals", ErrInvalid)
		}
		defer c.syncRuntimeState(runtime)

		if clearGoal {
			return goals.ClearGoal(ctx)
		}

		return goals.PauseGoal(ctx)
	})
}
