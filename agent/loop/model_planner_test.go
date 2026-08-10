package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loopModel struct {
	request  ai.Request
	response *ai.Response
	err      error
}

func (model *loopModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	model.request = request

	return model.response, model.err
}

func (model *loopModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*loopModel) Provider() ai.Provider         { return "loop-test" }
func (*loopModel) ModelID() string               { return "loop-test-1" }
func (*loopModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func TestModelPlannerUsesStructuredToolFreeRequestAndReportsUsage(t *testing.T) {
	t.Parallel()

	model := &loopModel{response: &ai.Response{
		Message: ai.AssistantText(`{"stop":false,"delay_seconds":300,"reason":"deployment is still running"}`),
		Usage:   ai.Usage{InputTokens: 10, OutputTokens: 4},
	}}
	planner, err := NewModelPlanner(model,
		WithDelayBounds(time.Minute, 10*time.Minute),
		WithMaxTokens(100),
	)
	require.NoError(t, err)

	plan, err := planner.Plan(t.Context(), PlanRequest{
		Iteration: 2, WorkInput: ai.JSON(`{"prompt":"check deploy"}`),
		Evidence:   ai.JSON(`{"status":"running"}`),
		Limits:     continuation.Limits{MaxTokens: 50},
		Accounting: continuation.Accounting{Usage: ai.Usage{InputTokens: 30}},
		Previous:   &PlanRecord{After: time.Minute, Reason: "building"},
	})
	require.NoError(t, err)
	assert.False(t, plan.Stop)
	assert.Equal(t, 5*time.Minute, plan.After)
	assert.Equal(t, "deployment is still running", plan.Reason)
	assert.Equal(t, model.response.Usage, plan.Usage)
	assert.Empty(t, model.request.Tools)
	assert.Equal(t, ai.Ptr(0.0), model.request.Temperature)
	require.NotNil(t, model.request.MaxTokens)
	assert.Equal(t, 20, *model.request.MaxTokens)
	require.NotNil(t, model.request.ResponseFormat)
	assert.True(t, model.request.ResponseFormat.Strict)
	assert.Equal(t, "loop_plan", model.request.ResponseFormat.Name)
	system, conversation, err := model.request.Messages.SplitSystem()
	require.NoError(t, err)
	assert.Contains(t, ai.JoinSystemText(system), "untrusted data")
	require.Len(t, conversation, 1)
}

func TestModelPlannerStopsWithZeroDelay(t *testing.T) {
	t.Parallel()

	model := &loopModel{response: &ai.Response{
		Message: ai.AssistantText(`{"stop":true,"delay_seconds":0,"reason":"deployment completed"}`),
	}}
	planner, err := NewModelPlanner(model)
	require.NoError(t, err)

	plan, err := planner.Plan(t.Context(), PlanRequest{
		Iteration: 1, Evidence: ai.JSON(`{"status":"complete"}`),
	})
	require.NoError(t, err)
	assert.True(t, plan.Stop)
	assert.Zero(t, plan.After)
}

func TestModelPlannerRejectsInvalidOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
	}{
		{name: "stop with delay", output: `{"stop":true,"delay_seconds":60,"reason":"done"}`},
		{name: "zero delay", output: `{"stop":false,"delay_seconds":0,"reason":"wait"}`},
		{name: "below bound", output: `{"stop":false,"delay_seconds":59,"reason":"wait"}`},
		{name: "above bound", output: `{"stop":false,"delay_seconds":3601,"reason":"wait"}`},
		{name: "missing reason", output: `{"stop":false,"delay_seconds":60,"reason":""}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := &loopModel{response: &ai.Response{Message: ai.AssistantText(test.output)}}
			planner, err := NewModelPlanner(model)
			require.NoError(t, err)

			_, err = planner.Plan(t.Context(), PlanRequest{Iteration: 1, Evidence: ai.JSON(`{}`)})
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestModelPlannerReturnsObservedUsageOnDecodeError(t *testing.T) {
	t.Parallel()

	model := &loopModel{response: &ai.Response{
		Message: ai.AssistantText(`not-json`), Usage: ai.Usage{InputTokens: 7, OutputTokens: 2},
	}}
	planner, err := NewModelPlanner(model)
	require.NoError(t, err)

	plan, err := planner.Plan(t.Context(), PlanRequest{Iteration: 1, Evidence: ai.JSON(`{}`)})
	require.Error(t, err)
	assert.Equal(t, model.response.Usage, plan.Usage)
}

func TestModelPlannerPropagatesModelError(t *testing.T) {
	t.Parallel()

	modelFailure := errors.New("model unavailable")
	model := &loopModel{err: modelFailure}
	planner, err := NewModelPlanner(model)
	require.NoError(t, err)

	_, err = planner.Plan(t.Context(), PlanRequest{Iteration: 1, Evidence: ai.JSON(`{}`)})
	require.ErrorIs(t, err, modelFailure)
}

func TestModelPlannerRejectsInvalidRequestMetadata(t *testing.T) {
	t.Parallel()

	model := &loopModel{}
	planner, err := NewModelPlanner(model)
	require.NoError(t, err)

	tests := []struct {
		name    string
		request PlanRequest
	}{
		{name: "zero iteration", request: PlanRequest{}},
		{
			name: "invalid previous",
			request: PlanRequest{
				Iteration: 1, Previous: &PlanRecord{After: -time.Second, Reason: "bad"},
			},
		},
		{
			name: "negative accounting",
			request: PlanRequest{
				Iteration: 1, Accounting: continuation.Accounting{Attempts: -1},
			},
		},
		{
			name: "negative token limit",
			request: PlanRequest{
				Iteration: 1, Limits: continuation.Limits{MaxTokens: -1},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := planner.Plan(t.Context(), test.request)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestNewModelPlannerValidatesOptions(t *testing.T) {
	t.Parallel()

	_, err := NewModelPlanner(nil)
	require.ErrorIs(t, err, ErrInvalid)

	model := &loopModel{}
	_, err = NewModelPlanner(model, nil)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = NewModelPlanner(model, WithMaxTokens(0))
	require.ErrorIs(t, err, ErrInvalid)

	for _, bounds := range [][2]time.Duration{
		{0, time.Minute},
		{time.Minute, time.Second},
		{time.Second + time.Nanosecond, time.Minute},
	} {
		_, err = NewModelPlanner(model, WithDelayBounds(bounds[0], bounds[1]))
		require.ErrorIs(t, err, ErrInvalid)
	}
}
