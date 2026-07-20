//go:build !darwin && !linux

package execution

import "context"

func (e *Executor) run(context.Context, *Plan, Sink) (Result, error) {
	return Result{}, ErrUnsupportedPlatform
}
