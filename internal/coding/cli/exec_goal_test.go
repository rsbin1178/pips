package cli

import (
	"bytes"
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecGoalCommandRoutesGoalAndBudget(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirAllPrivate(workspacePath))

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	dependencies := Dependencies{
		Paths: layout,
		LookupEnv: func(name string) (string, bool) {
			if name == config.ModelEnv {
				return "openai/test-model", true
			}

			return "", false
		},
		WorkingDir: func() (string, error) { return workspacePath, nil },
	}
	runtime := &goalExecRuntime{
		fakeExecRuntime: successfulFakeRuntime("session-1"),
		goal:            coding.GoalState{ID: "goal-1", Status: "completed"},
	}
	command := newExecCommand(dependencies, &rootFlags{workspace: "."},
		func(context.Context, coding.OpenOptions) (execRuntime, error) { return runtime, nil })
	output := new(bytes.Buffer)

	command.SetIn(strings.NewReader(""))
	command.SetOut(output)
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{"--goal", "--goal-budget", "1200", "all tests pass"})

	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Equal(t, coding.GoalRequest{Condition: "all tests pass", MaxTokens: 1200}, runtime.request)
	assert.Equal(t, 1, runtime.goalCalls)
	assert.Zero(t, runtime.promptCalls, "goal mode must not also submit an ordinary prompt")
	assert.Equal(t, 1, runtime.closeCalls)
	assert.Equal(t, "verified answer\n", output.String())
}

func TestExecGoalRejectsNonCompletedGoalEvenAfterSuccessfulInteraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		goal coding.GoalState
	}{
		{name: "missing"},
		{name: "paused", goal: coding.GoalState{ID: "goal-1", Status: "paused"}},
		{name: "limited", goal: coding.GoalState{ID: "goal-1", Status: "limited"}},
		{name: "blocked", goal: coding.GoalState{ID: "goal-1", Status: "blocked"}},
		{name: "interrupted", goal: coding.GoalState{ID: "goal-1", Status: "interrupted"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			runtime := &goalExecRuntime{fakeExecRuntime: successfulFakeRuntime("session-1"), goal: test.goal}
			presenter := new(recordingPresenter)
			err := runExecRequest(t.Context(), runtime, presenter, "tests pass", &coding.GoalRequest{Condition: "tests pass"})
			require.ErrorContains(t, err, "goal did not complete")
			assert.Equal(t, 1, runtime.closeCalls)
			assert.Zero(t, presenter.finalCalls)
		})
	}
}

func TestExecGoalInvalidFlagsFailBeforeOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "budget without goal", args: []string{"--goal-budget", "100", "tests"}},
		{name: "negative budget", args: []string{"--goal", "--goal-budget", "-1", "tests"}},
		{name: "explicit zero budget", args: []string{"--goal", "--goal-budget", "0", "tests"}},
		{name: "condition too long", args: []string{"--goal", strings.Repeat("字", 4001)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			opened := false
			command := newExecCommand(Dependencies{}, &rootFlags{},
				func(context.Context, coding.OpenOptions) (execRuntime, error) {
					opened = true

					return nil, nil
				})
			command.SetIn(strings.NewReader(""))
			command.SetOut(new(bytes.Buffer))
			command.SetErr(new(bytes.Buffer))
			command.SetArgs(test.args)
			require.ErrorIs(t, command.ExecuteContext(t.Context()), ErrUsage)
			assert.False(t, opened)
		})
	}
}

func TestExecGoalUnsupportedRuntimeFailsAndCloses(t *testing.T) {
	t.Parallel()

	runtime := successfulFakeRuntime("session-1")
	err := runExecRequest(t.Context(), runtime, new(recordingPresenter), "tests", &coding.GoalRequest{Condition: "tests"})
	require.ErrorContains(t, err, "does not support goals")
	assert.Equal(t, 1, runtime.closeCalls)
	assert.Zero(t, runtime.promptCalls)
}

type goalExecRuntime struct {
	*fakeExecRuntime
	request   coding.GoalRequest
	goal      coding.GoalState
	goalCalls int
}

func (r *goalExecRuntime) StartGoal(_ context.Context, request coding.GoalRequest) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {
		r.goalCalls++
		r.request = request
		r.state.Goal = r.goal
		r.state.Interaction.Outcome = coding.InteractionSucceeded
		r.state.Transcript = append(r.state.Transcript, ai.AssistantText("verified answer"))
	}
}
