package runtimecontrol

import (
	"context"
	"iter"
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalControlsHoldRuntimeLease(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	deps := fixture.dependencies()
	open := deps.openRuntime

	var runtime *goalControlRuntime

	deps.openRuntime = func(ctx context.Context, options coding.OpenOptions) (runtimeInstance, error) {
		opened, err := open(ctx, options)
		if err != nil {
			return nil, err
		}

		runtime = &goalControlRuntime{runtimeInstance: opened}

		return runtime, nil
	}
	controller, err := newController(t.Context(), fixture.options, deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, controller.Close(t.Context())) })

	runtime.onStart = func() {
		require.ErrorIs(t, controller.NewSession(t.Context()), ErrBusy)
		assert.NoError(t, controller.PauseGoal(t.Context()), "pause must be callable while the goal sequence holds a lease")
	}

	request := coding.GoalRequest{Condition: "tests pass", MaxTokens: 1200}
	for _, eventErr := range controller.StartGoal(t.Context(), request) {
		require.NoError(t, eventErr)
	}

	assert.Equal(t, request, runtime.request)
	assert.Equal(t, 1, runtime.starts)
	assert.Equal(t, 1, runtime.pauses)

	for _, eventErr := range controller.ResumeGoal(t.Context()) {
		require.NoError(t, eventErr)
	}

	require.NoError(t, controller.ClearGoal(t.Context()))
	assert.Equal(t, 1, runtime.resumes)
	assert.Equal(t, 1, runtime.clears)
	assert.Zero(t, controller.active)
}

func TestGoalControlsRejectUnsupportedRuntime(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, controller.Close(t.Context())) })

	for _, sequence := range []iter.Seq2[coding.Event, error]{
		controller.StartGoal(t.Context(), coding.GoalRequest{Condition: "tests"}),
		controller.ResumeGoal(t.Context()),
	} {
		count := 0
		for _, eventErr := range sequence {
			count++

			require.ErrorIs(t, eventErr, ErrInvalid)
		}

		assert.Equal(t, 1, count)
	}

	require.ErrorIs(t, controller.PauseGoal(t.Context()), ErrInvalid)
	require.ErrorIs(t, controller.ClearGoal(t.Context()), ErrInvalid)
	assert.Zero(t, controller.active)
}

type goalControlRuntime struct {
	runtimeInstance
	request coding.GoalRequest
	onStart func()
	starts  int
	resumes int
	pauses  int
	clears  int
}

func (r *goalControlRuntime) StartGoal(_ context.Context, request coding.GoalRequest) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {
		r.request = request

		r.starts++
		if r.onStart != nil {
			r.onStart()
		}
	}
}

func (r *goalControlRuntime) ResumeGoal(context.Context) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) { r.resumes++ }
}

func (r *goalControlRuntime) PauseGoal(context.Context) error {
	r.pauses++

	return nil
}

func (r *goalControlRuntime) ClearGoal(context.Context) error {
	r.clears++

	return nil
}
