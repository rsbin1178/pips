package workflow

import (
	"context"
	"sync"
	"time"
)

type runInterruptContextKey struct{}

type runInterruptOptions struct {
	timeout    time.Duration
	hasTimeout bool
}

// RunInterruptOption configures one external host interruption request.
type RunInterruptOption func(*runInterruptOptions)

type runInterruptState struct {
	requested chan struct{}
	once      sync.Once
	mu        sync.RWMutex
	options   runInterruptOptions
}

// WithRunInterrupt enables a resumable host interruption signal without
// canceling the parent context. The returned function is safe to call more than
// once; only its first call takes effect.
func WithRunInterrupt(
	parent context.Context,
) (context.Context, func(...RunInterruptOption)) {
	state := &runInterruptState{requested: make(chan struct{})}
	ctx := context.WithValue(parent, runInterruptContextKey{}, state)

	interrupt := func(options ...RunInterruptOption) {
		state.once.Do(func() {
			config := runInterruptOptions{}

			for _, option := range options {
				if option != nil {
					option(&config)
				}
			}

			state.mu.Lock()
			state.options = config
			state.mu.Unlock()
			close(state.requested)
		})
	}

	return ctx, interrupt
}

// WithRunInterruptTimeout bounds how long the scheduler waits for in-flight
// invocations to settle. Zero and negative durations request immediate
// cooperative cancellation of unfinished leaf invocations.
func WithRunInterruptTimeout(timeout time.Duration) RunInterruptOption {
	return func(options *runInterruptOptions) {
		options.timeout = timeout
		options.hasTimeout = true
	}
}

func runInterruptFromContext(ctx context.Context) *runInterruptState {
	state, _ := ctx.Value(runInterruptContextKey{}).(*runInterruptState)

	return state
}

func (s *runInterruptState) snapshot() runInterruptOptions {
	if s == nil {
		return runInterruptOptions{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.options
}
