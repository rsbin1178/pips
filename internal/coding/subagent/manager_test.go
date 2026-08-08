//nolint:wsl_v5 // Concurrent execution fixtures keep actions beside assertions.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerRunsEachRoleWithExactReadOnlyCatalog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role Role
		json string
	}{
		{RoleExplore, `{"summary":"located code","evidence":[],"unknowns":[]}`},
		{RolePlan, `{"summary":"plan","assumptions":[],"steps":[],"risks":[],"verification":[]}`},
		{RoleReview, `{"summary":"reviewed","findings":[],"residual_risks":[]}`},
	}
	for _, test := range tests {
		t.Run(string(test.role), func(t *testing.T) {
			t.Parallel()

			model := &testModel{responses: []*ai.Response{responseText(test.json)}}
			fixture := newManagerFixture(t, model)
			events := make([]Event, 0, 3)
			execution, err := fixture.manager.Start(
				t.Context(),
				Request{Role: test.role, Task: "Inspect the implementation."},
				func(_ context.Context, event Event) error {
					events = append(events, event)

					return nil
				},
			)
			require.NoError(t, err)
			result, err := execution.Wait(t.Context())
			require.NoError(t, err)
			assert.Equal(t, OutcomeSucceeded, result.Outcome)
			assert.Equal(t, "ok", result.Code)
			assert.NotEmpty(t, result.ChildSessionID)
			assert.IsType(t, expectedResult(test.role), result.Value)

			requests := model.Requests()
			require.Len(t, requests, 1)
			assert.Equal(t, []string{readToolName, "ls", "glob", "grep"}, toolNames(requests[0].Tools))
			require.NotNil(t, requests[0].ResponseFormat)
			assert.True(t, requests[0].ResponseFormat.Strict)
			require.NotNil(t, requests[0].MaxTokens)
			assert.Equal(t, DefaultLimits().MaxOutputTokens, *requests[0].MaxTokens)
			require.GreaterOrEqual(t, len(events), 4)
			assert.Equal(t, StateCreated, events[0].State)
			assert.Equal(t, StateRunning, events[1].State)

			for _, event := range events[2 : len(events)-1] {
				assert.True(t, event.Progress)
				assert.Equal(t, StateRunning, event.State)
			}

			assert.Equal(t, StateSucceeded, events[len(events)-1].State)

			summaries, err := fixture.manager.List(t.Context())
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			assert.Equal(t, StateSucceeded, summaries[0].State)
			detail, err := fixture.manager.Inspect(t.Context(), result.ChildSessionID)
			require.NoError(t, err)
			assert.Equal(t, result.ChildSessionID, detail.Summary.ChildSessionID)
			assert.IsType(t, expectedResult(test.role), detail.Result)
			require.Len(t, detail.Transcript, 2)
			assert.Equal(t, ai.RoleUser, detail.Transcript[0].Role)
			assert.Equal(t, ai.RoleAssistant, detail.Transcript[1].Role)
		})
	}
}

func TestManagerLifecycleAddsContextAndContinuesChild(t *testing.T) {
	t.Parallel()

	model := &testModel{responses: []*ai.Response{
		responseText(`{"summary":"first","evidence":[],"unknowns":[]}`),
		responseText(`{"summary":"second","evidence":[],"unknowns":[]}`),
	}}
	var (
		starts []LifecycleStart
		stops  []LifecycleStop
	)
	fixture := newManagerFixtureWithConfig(t, model, ExecutionOptions{}, func(config *Config) {
		config.Lifecycle = Lifecycle{
			BeforeStart: func(_ context.Context, value LifecycleStart) string {
				starts = append(starts, value)

				return "trusted child context"
			},
			BeforeStop: func(_ context.Context, value LifecycleStop) LifecycleStopDecision {
				stops = append(stops, value)
				if len(stops) == 1 {
					return LifecycleStopDecision{Continue: true, Reason: "Take one more focused pass."}
				}

				return LifecycleStopDecision{}
			},
		}
	})

	execution, err := fixture.manager.Start(t.Context(), Request{
		Role: RoleExplore, Task: "Inspect lifecycle behavior.",
	}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	require.Len(t, starts, 1)
	assert.Equal(t, result.ChildSessionID, starts[0].ChildSessionID)
	require.Len(t, stops, 2)
	assert.False(t, stops[0].StopHookActive)
	assert.True(t, stops[1].StopHookActive)
	assert.Contains(t, stops[0].LastAssistantMessage, `"summary":"first"`)

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.Contains(t, requests[0].System, "trusted child context")
	assert.Contains(t, messageText(requests[1].Messages[len(requests[1].Messages)-1]), "Take one more focused pass.")
}

func TestSummarizeToolActivityExposesOnlyBoundedSemanticFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call ai.ToolCallPart
		want ActivitySummary
	}{
		{
			name: "read",
			call: ai.ToolCallPart{Name: "read", Args: ai.JSON(`{"path":"internal/coding/runtime.go","token":"secret"}`)},
			want: ActivitySummary{Action: ActivityActionRead, Target: "internal/coding/runtime.go"},
		},
		{
			name: "search",
			call: ai.ToolCallPart{Name: "grep", Args: ai.JSON(`{"pattern":"SubagentState","path":"internal/coding"}`)},
			want: ActivitySummary{Action: ActivityActionSearch, Target: "SubagentState in internal/coding"},
		},
		{
			name: "control characters",
			call: ai.ToolCallPart{Name: "read", Args: ai.JSON("{\"path\":\"a\\n\\u0001b\"}")},
			want: ActivitySummary{Action: ActivityActionRead, Target: "a b"},
		},
		{
			name: "unknown tool",
			call: ai.ToolCallPart{Name: "shell", Args: ai.JSON(`{"command":"print secret"}`)},
		},
		{
			name: "outside workspace",
			call: ai.ToolCallPart{Name: "read", Args: ai.JSON(`{"path":"safe/../../secret"}`)},
		},
		{
			name: "invalid arguments",
			call: ai.ToolCallPart{Name: "read", Args: ai.JSON(`{`)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, summarizeToolActivity(test.call))
		})
	}
}

func TestSummarizeToolActivityBoundsMultibyteTarget(t *testing.T) {
	t.Parallel()

	summary := summarizeToolActivity(ai.ToolCallPart{
		Name: readToolName,
		Args: ai.JSON(fmt.Sprintf(`{"path":"%s"}`, strings.Repeat("🙂", 200))),
	})

	assert.Equal(t, ActivityActionRead, summary.Action)
	assert.LessOrEqual(t, len(summary.Target), 512)
	assert.LessOrEqual(t, utf8.RuneCountInString(summary.Target), 160)
	assert.True(t, utf8.ValidString(summary.Target))
	assert.True(t, strings.HasSuffix(summary.Target, "…"))
}

func TestRunTrackerProjectsDefensiveLiveActivity(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.July, 23, 10, 0, 0, 0, time.UTC)
	tracker := newRunTracker(startedAt, DefaultLimits().MaxActivityTools)
	call := ai.ToolCallPart{
		ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"internal/coding/runtime.go"}`),
	}

	events := []agent.Event{
		{Type: agent.EventRunStart, RunID: "run-1", Time: startedAt},
		{Type: agent.EventTurnStart, RunID: "run-1", Turn: 1, Time: startedAt.Add(time.Second)},
		{
			Type: agent.EventToolStart, RunID: "run-1", Turn: 1,
			Time: startedAt.Add(2 * time.Second), Call: &call,
		},
		{
			Type: agent.EventToolUpdate, RunID: "run-1", Turn: 1,
			Time: startedAt.Add(3 * time.Second), Call: &call,
			Update: []ai.Part{ai.TextPart{Text: "reading"}},
		},
	}
	for _, event := range events {
		trackChildEvent(tracker, event)
	}

	activity := tracker.activitySnapshot()
	assert.Equal(t, ActivityPhaseWorking, activity.Phase)
	assert.Equal(t, "run-1", activity.RunID)
	assert.Equal(t, 1, activity.Turn)
	assert.Equal(t, uint64(5), activity.Revision)
	require.Len(t, activity.Tools, 1)
	assert.Equal(t, ToolStatusRunning, activity.Tools[0].Status)
	assert.Equal(t, "reading", messageText(activity.Tools[0].Update))

	activity.Tools[0].Call.Args[0] = '['
	activity.Tools[0].Update.Parts[0] = ai.TextPart{Text: "mutated"}

	second := tracker.activitySnapshot()
	assert.JSONEq(t, `{"path":"internal/coding/runtime.go"}`, string(second.Tools[0].Call.Args))
	assert.Equal(t, "reading", messageText(second.Tools[0].Update))
}

func TestRunTrackerBoundsActivityWithoutFreezingCurrentAction(t *testing.T) {
	t.Parallel()

	tracker := newRunTracker(time.Now().UTC(), 1)
	for index, target := range []string{"first.txt", "second.txt"} {
		call := ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(fmt.Sprintf(`{"path":%q}`, target)),
		}
		trackChildEvent(tracker, agent.Event{
			Type: agent.EventToolStart, RunID: "run-1", Turn: index + 1,
			Time: time.Now().UTC(), Call: &call,
		})
	}

	snapshot, activity := tracker.snapshot()
	require.Len(t, activity.Tools, 1)
	assert.Equal(t, "first.txt", summarizeToolActivity(activity.Tools[0].Call).Target)
	assert.Equal(t, "second.txt", snapshot.activity.Target)
	assert.Equal(t, ActivityPhaseWorking, activity.Phase)
}

func TestManagerInspectOverlaysInFlightToolActivity(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{
		ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"sentinel.txt"}`),
	}
	model := &testModel{responses: []*ai.Response{
		responseToolCall(call),
		responseText(`{"summary":"done","evidence":[],"unknowns":[]}`),
	}}
	fixture := newManagerFixture(t, model)
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
	))

	entered := make(chan struct{})
	release := make(chan struct{})

	var (
		enteredOnce sync.Once
		releaseOnce sync.Once
	)

	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	fixture.manager.config.AgentObservers = []func(context.Context, agent.Event){
		func(_ context.Context, event agent.Event) {
			if event.Type != agent.EventToolStart {
				return
			}

			enteredOnce.Do(func() { close(entered) })
			<-release
		},
	}

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Read the sentinel."}, nil,
	)
	require.NoError(t, err)
	<-entered

	detail, err := fixture.manager.Inspect(t.Context(), execution.child.Metadata().ID)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, detail.Summary.State)
	assert.Equal(t, 1, detail.Summary.Turns)
	assert.Equal(t, 1, detail.Summary.ToolCalls)
	assert.Positive(t, detail.Summary.Duration)
	assert.Equal(t, ActivityPhaseWorking, detail.Activity.Phase)
	require.Len(t, detail.Activity.Tools, 1)
	assert.Equal(t, ToolStatusRunning, detail.Activity.Tools[0].Status)
	assert.Equal(t, "call-1", detail.Activity.Tools[0].Call.ID)

	releaseOnce.Do(func() { close(release) })

	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)

	replayed, err := fixture.manager.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, Activity{}, replayed.Activity)
	assert.Equal(t, StateSucceeded, replayed.Summary.State)
}

func TestManagerReservesFinalTurnWithoutTools(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTurns = 3
	first := ai.ToolCallPart{
		ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"first.txt"}`),
	}
	second := ai.ToolCallPart{
		ID: "call-2", Name: readToolName, Args: ai.JSON(`{"path":"second.txt"}`),
	}
	model := &testModel{responses: []*ai.Response{
		responseToolCall(first),
		responseToolCall(second),
		responseText(`{"summary":"done","evidence":[],"unknowns":[]}`),
	}}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{Limits: limits})
	require.NoError(t, os.WriteFile(filepath.Join(fixture.root, "first.txt"), []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixture.root, "second.txt"), []byte("second"), 0o600))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Read both files."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 3, result.Turns)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.NotEmpty(t, requests[0].Tools)
	assert.NotEmpty(t, requests[1].Tools)
	assert.Empty(t, requests[2].Tools)
	assert.Equal(t, finalizationInstruction, messageText(requests[2].Messages[len(requests[2].Messages)-1]))
}

//nolint:wsl_v5 // Scripted turns and filesystem fixtures intentionally stay adjacent.
func TestManagerDefaultAllowsWorkBeyondFormerExecutionBudgets(t *testing.T) {
	t.Parallel()

	const workingTurns = 65
	responses := make([]*ai.Response, 0, workingTurns+1)
	for index := range workingTurns {
		response := responseToolCall(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(fmt.Sprintf(`{"path":"file-%d.txt"}`, index+1)),
		})
		response.Usage = ai.Usage{InputTokens: 2_000, OutputTokens: 20}
		responses = append(responses, response)
	}
	finalResponse := responseText(`{"summary":"done","evidence":[],"unknowns":[]}`)
	finalResponse.Usage = ai.Usage{InputTokens: 2_000, OutputTokens: 20}
	responses = append(responses, finalResponse)

	model := &testModel{responses: responses}
	fixture := newManagerFixture(t, model)
	for index := range workingTurns {
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.root, fmt.Sprintf("file-%d.txt", index+1)),
			[]byte("content"),
			0o600,
		))
	}

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect every fixture."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.Equal(t, workingTurns+1, result.Turns)
	assert.Greater(t, result.ToolCalls, 64)
	assert.Greater(t, result.Usage.InputTokens+result.Usage.OutputTokens, 120_000)

	requests := model.Requests()
	require.Len(t, requests, workingTurns+1)
	assert.NotEmpty(t, requests[workingTurns].Tools)
}

func TestManagerStopsAfterOneFailedNoProgressConvergenceTurn(t *testing.T) {
	t.Parallel()

	limit := DefaultLimits().RepeatedToolCallLimit
	responses := make([]*ai.Response, 0, limit+2)
	for index := range limit + 2 {
		responses = append(responses, responseToolCall(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(`{"path":"sentinel.txt"}`),
		}))
	}
	model := &testModel{responses: responses}
	fixture := newManagerFixture(t, model)
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
	))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Avoid looping."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	assert.Equal(t, "no_progress", result.Code)
	assert.Equal(t, agent.StopWhen, result.Stop)

	requests := model.Requests()
	require.Len(t, requests, limit+2)
	assert.Empty(t, requests[len(requests)-1].Tools)
}

func TestManagerClassifiesMalformedConvergenceAsNoProgress(t *testing.T) {
	t.Parallel()

	limit := DefaultLimits().RepeatedToolCallLimit
	responses := make([]*ai.Response, 0, limit+2)
	for index := range limit + 1 {
		responses = append(responses, responseToolCall(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(`{"path":"sentinel.txt"}`),
		}))
	}
	responses = append(responses, responseText(`{"summary":`))
	fixture := newManagerFixture(t, &testModel{responses: responses})
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
	))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Avoid looping."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	assert.Equal(t, "no_progress", result.Code)
}

func TestManagerCompactsChildContextBetweenToolTurns(t *testing.T) {
	t.Parallel()

	settings := harness.CompactionSettings{
		ContextTokens: 800, ReserveTokens: 100, KeepRecentTokens: 250, SummaryTokens: 64,
	}
	responses := []*ai.Response{
		responseToolCall(ai.ToolCallPart{
			ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"large.txt"}`),
		}),
		responseToolCall(ai.ToolCallPart{
			ID: "call-2", Name: readToolName, Args: ai.JSON(`{"path":"large.txt"}`),
		}),
		responseText(`{"summary":"done","evidence":[],"unknowns":[]}`),
	}
	responses[0].Usage = ai.Usage{InputTokens: 100, OutputTokens: 20}
	responses[1].Usage = ai.Usage{InputTokens: 500, OutputTokens: 20}

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		model := &testModel{responses: slices.Clone(responses)}
		summarizer := &testModel{responses: []*ai.Response{responseText("compact summary")}}
		fixture := newManagerFixtureWithConfig(
			t, model, ExecutionOptions{}, func(config *Config) {
				config.Compaction = &settings
				config.SummaryModel = summarizer
			},
		)
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.root, "large.txt"), []byte(strings.Repeat("word", 200)), 0o600,
		))

		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Read the large file twice."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.NoError(t, err)
		assert.Equal(t, OutcomeSucceeded, result.Outcome)
		require.Len(t, summarizer.Requests(), 1)
		require.Len(t, model.Requests(), 3)
		assert.Contains(t, requestText(model.Requests()[2]), harness.CompactionPrefix)
		assert.Equal(t, 1, countChildCompactions(execution.child.Session().Path()))
	})

	t.Run("summary failure stops before another child request", func(t *testing.T) {
		t.Parallel()

		model := &testModel{responses: slices.Clone(responses)}
		summarizer := &testModel{}
		fixture := newManagerFixtureWithConfig(
			t, model, ExecutionOptions{}, func(config *Config) {
				config.Compaction = &settings
				config.SummaryModel = summarizer
			},
		)
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.root, "large.txt"), []byte(strings.Repeat("word", 200)), 0o600,
		))

		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Read the large file twice."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.Error(t, err)
		assert.Equal(t, OutcomeFailed, result.Outcome)
		assert.Equal(t, "execution_failed", result.Code)
		assert.Len(t, model.Requests(), 2)
		assert.Zero(t, countChildCompactions(execution.child.Session().Path()))
	})

	t.Run("cancellation interrupts summary", func(t *testing.T) {
		t.Parallel()

		model := &testModel{responses: slices.Clone(responses)}
		summarizer := &blockingTestModel{entered: make(chan struct{})}
		fixture := newManagerFixtureWithConfig(
			t, model, ExecutionOptions{}, func(config *Config) {
				config.Compaction = &settings
				config.SummaryModel = summarizer
			},
		)
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.root, "large.txt"), []byte(strings.Repeat("word", 200)), 0o600,
		))

		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Read the large file twice."}, nil,
		)
		require.NoError(t, err)
		<-summarizer.entered
		execution.Cancel()
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
		assert.Equal(t, "canceled", result.Code)
		assert.Len(t, model.Requests(), 2)
		assert.Zero(t, countChildCompactions(execution.child.Session().Path()))
	})
}

//nolint:wsl_v5 // Scripted repeated calls and finalization assertions form one scenario.
func TestManagerWarnsThenFinalizesAfterRepeatedToolCalls(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTurns = 6
	repeatLimit := DefaultLimits().RepeatedToolCallLimit
	responses := make([]*ai.Response, 0, repeatLimit+2)
	for index := range repeatLimit + 1 {
		responses = append(responses, responseToolCall(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index+1), Name: readToolName,
			Args: ai.JSON(`{"path":"sentinel.txt"}`),
		}))
	}
	responses = append(
		responses,
		responseText(`{"summary":"bounded","evidence":[],"unknowns":[]}`),
	)

	model := &testModel{responses: responses}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{Limits: limits})
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
	))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Avoid looping."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.Equal(t, repeatLimit+2, result.Turns)

	requests := model.Requests()
	require.Len(t, requests, repeatLimit+2)
	assert.NotEmpty(t, requests[repeatLimit].Tools)
	assert.Empty(t, requests[len(requests)-1].Tools)
	assert.Equal(
		t,
		finalizationInstruction,
		messageText(requests[len(requests)-1].Messages[len(requests[len(requests)-1].Messages)-1]),
	)
}

//nolint:wsl_v5 // Scripted repeated calls and recovery assertions form one scenario.
func TestManagerRecoversWhenToolCallChangesAfterRepeatWarning(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTurns = 7
	repeatLimit := limits.RepeatedToolCallLimit
	responses := make([]*ai.Response, 0, repeatLimit+2)
	for index := range repeatLimit {
		responses = append(responses, responseToolCall(ai.ToolCallPart{
			ID: fmt.Sprintf("repeat-%d", index+1), Name: readToolName,
			Args: ai.JSON(`{"path":"sentinel.txt"}`),
		}))
	}
	responses = append(
		responses,
		responseToolCall(ai.ToolCallPart{
			ID: "different", Name: readToolName,
			Args: ai.JSON(`{"path":"recovery.txt"}`),
		}),
		responseText(`{"summary":"recovered","evidence":[],"unknowns":[]}`),
	)

	model := &testModel{responses: responses}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{Limits: limits})
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(fixture.root, "recovery.txt"), []byte("content"), 0o600,
	))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Recover from a repeated read."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.Equal(t, repeatLimit+2, result.Turns)

	requests := model.Requests()
	require.Len(t, requests, repeatLimit+2)
	for _, request := range requests {
		assert.NotEmpty(t, request.Tools)
		assert.NotEqual(
			t, finalizationInstruction,
			messageText(request.Messages[len(request.Messages)-1]),
		)
	}
}

func TestManagerOmitsNativeSchemaForModelWithoutStructuredOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		role        Role
		text        string
		schemaField string
		want        any
	}{
		{
			name:        "explore Chinese text",
			role:        RoleExplore,
			text:        "兼容模型返回的自然语言总结。",
			schemaField: `"evidence"`,
			want: ExploreResult{
				Summary:  "兼容模型返回的自然语言总结。",
				Evidence: []Evidence{},
				Unknowns: []string{},
			},
		},
		{
			name:        "plan sentence",
			role:        RolePlan,
			text:        "plan subagent 工作正常",
			schemaField: `"steps"`,
			want: PlanResult{
				Summary:      "plan subagent 工作正常",
				Assumptions:  []string{},
				Steps:        []PlanStep{},
				Risks:        []string{},
				Verification: []string{},
			},
		},
		{
			name:        "review sentence",
			role:        RoleReview,
			text:        "review subagent 工作正常",
			schemaField: `"findings"`,
			want: ReviewResult{
				Summary:       "review subagent 工作正常",
				Findings:      []ReviewFinding{},
				ResidualRisks: []string{},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			capabilities := ai.Capabilities{Text: true, Tools: true}
			model := &testModel{
				responses:            []*ai.Response{responseText(test.text)},
				capabilities:         &capabilities,
				rejectResponseFormat: true,
			}
			fixture := newManagerFixture(t, model)
			execution, err := fixture.manager.Start(
				t.Context(),
				Request{Role: test.role, Task: "Inspect the implementation."},
				nil,
			)
			require.NoError(t, err)

			result, err := execution.Wait(t.Context())
			require.NoError(t, err)
			assert.Equal(t, OutcomeSucceeded, result.Outcome)
			assert.Equal(t, test.want, result.Value)

			requests := model.Requests()
			require.Len(t, requests, 1)
			assert.Nil(t, requests[0].ResponseFormat)
			assert.Equal(
				t,
				[]string{readToolName, "ls", "glob", "grep"},
				toolNames(requests[0].Tools),
			)
			assert.Contains(t, requests[0].System, "Native structured output is unavailable")
			assert.Contains(t, requests[0].System, test.schemaField)
			require.NotNil(t, requests[0].MaxTokens)
			assert.Equal(t, DefaultLimits().MaxOutputTokens, *requests[0].MaxTokens)
		})
	}
}

func TestManagerIsSerialCancelableAndLeavesWorkspaceUnchanged(t *testing.T) {
	t.Parallel()

	model := &blockingTestModel{entered: make(chan struct{})}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{MaxConcurrent: 1})
	path := filepath.Join(fixture.root, "sentinel.txt")
	require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0o600))

	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Wait."}, nil,
	)
	require.NoError(t, err)
	<-model.entered

	_, err = fixture.manager.Start(
		t.Context(), Request{Role: RolePlan, Task: "Second."}, nil,
	)
	require.ErrorIs(t, err, ErrBusy)

	execution.Cancel()
	result, err := execution.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, OutcomeCanceled, result.Outcome)
	assert.Equal(t, "canceled", result.Code)

	content, err := fs.ReadFile(fixture.tree.FileSystem(), "sentinel.txt")
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(content))
}

func TestManagerEnforcesConcurrentCapacity(t *testing.T) {
	t.Parallel()

	model := &blockingTestModel{entered: make(chan struct{})}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{MaxConcurrent: 4})
	executions := make([]*Execution, 0, 4)
	for index := range 4 {
		execution, err := fixture.manager.Start(t.Context(), Request{
			Role: RoleExplore, Task: fmt.Sprintf("Wait %d.", index),
		}, nil)
		require.NoError(t, err)
		executions = append(executions, execution)
	}

	_, err := fixture.manager.Start(
		t.Context(), Request{Role: RolePlan, Task: "Over capacity."}, nil,
	)
	require.ErrorIs(t, err, ErrCapacity)

	for _, execution := range executions {
		execution.Cancel()
	}
	for _, execution := range executions {
		result, waitErr := execution.Wait(t.Context())
		require.ErrorIs(t, waitErr, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
	}
}

func TestManagerWaitReconstructsCompletedChild(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{responses: []*ai.Response{
		responseText(`{"summary":"done","evidence":[],"unknowns":[]}`),
	}})
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	completed, err := execution.Wait(t.Context())
	require.NoError(t, err)

	reconstructed, err := fixture.manager.Wait(t.Context(), completed.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, completed.ChildSessionID, reconstructed.ChildSessionID)
	assert.Equal(t, OutcomeSucceeded, reconstructed.Outcome)
	assert.IsType(t, ExploreResult{}, reconstructed.Value)
}

func TestManagerCancelRootOnlyCancelsMatchingBackgroundChildren(t *testing.T) {
	t.Parallel()

	model := &blockingTestModel{entered: make(chan struct{})}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{MaxConcurrent: 4})
	start := func(root, callID string) *Execution {
		execution, err := fixture.manager.Start(t.Context(), Request{
			Role: RoleExplore, Task: "Wait.", Delivery: DeliveryBackground,
			Ownership: Ownership{
				ParentInteractionID: root, ParentRunID: "parent-run",
				ParentToolCallID: callID, RootInteractionID: root,
			},
		}, nil)
		require.NoError(t, err)

		return execution
	}

	first := start("root-1", "call-1")
	second := start("root-1", "call-2")
	other := start("root-2", "call-3")
	<-model.entered

	assert.Equal(t, 2, fixture.manager.CancelRoot("root-1"))
	for _, execution := range []*Execution{first, second} {
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
	}
	select {
	case <-other.done:
		require.FailNow(t, "unrelated child was canceled")
	case <-time.After(25 * time.Millisecond):
	}
	require.NoError(t, fixture.manager.Cancel(t.Context(), other.child.Metadata().ID))
	_, err := other.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
}

func TestSpawnToolSurvivesCallContextAndEnforcesRootLimit(t *testing.T) {
	t.Parallel()

	model := &blockingTestModel{entered: make(chan struct{})}
	fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{
		MaxConcurrent: 4, MaxSpawnedPerRootInteraction: 2,
	})
	tool := fixture.manager.SpawnToolFor(Ownership{
		ParentInteractionID: "interaction-1", RootInteractionID: "interaction-1",
	}, nil)

	callCtx, cancelCall := context.WithCancel(t.Context())
	parts, err := tool.Exec(callCtx, agent.ToolCall{
		ID: "spawn-1", Name: SpawnToolName,
		Args: ai.JSON(`{"role":"explore","task":"Wait for cancellation."}`),
	})
	require.NoError(t, err)
	cancelCall()

	var first spawnResult
	require.NoError(t, json.Unmarshal([]byte(messageText(ai.Message{
		Role: ai.RoleTool, Parts: parts,
	})), &first))
	assert.Equal(t, SpawnResultSchema, first.Schema)
	firstExecution := fixture.manager.activeExecution(first.AgentID)
	require.NotNil(t, firstExecution)
	select {
	case <-firstExecution.done:
		require.FailNow(t, "background execution was canceled with its Tool context")
	case <-time.After(25 * time.Millisecond):
	}

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-2", Name: SpawnToolName,
		Args: ai.JSON(`{"role":"plan","task":"Wait too."}`),
	})
	require.NoError(t, err)
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "spawn-3", Name: SpawnToolName,
		Args: ai.JSON(`{"role":"review","task":"Exceed the root limit."}`),
	})
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestRunSubagentToolHasStableSchemaAndEnvelope(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{responses: []*ai.Response{
		responseText(`{"summary":"ok","evidence":[],"unknowns":[]}`),
	}})
	tool := fixture.manager.Tool(nil)
	declaration := tool.Decl()
	assert.Equal(t, ToolName, declaration.Name)
	assert.Equal(t, []string{"task"}, declaration.InputSchema.Required)
	assert.Equal(t, []any{"explore", "plan", "review"}, declaration.InputSchema.Properties["role"].Enum)
	assert.Equal(t, []any{"explore", "plan", "review"}, declaration.InputSchema.Properties["agent_id"].Enum)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: ToolName,
		Args: ai.JSON(`{"role":"explore","task":"Inspect."}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, text.Text, `"schema":"`+ResultSchema+`"`)
	assert.Contains(t, text.Text, `"outcome":"succeeded"`)
}

func TestManagerInspectUsesPersistedLimitsAndRejectsOtherParent(t *testing.T) {
	t.Parallel()

	model := &testModel{responses: []*ai.Response{
		responseText(`{"summary":"persisted result","evidence":[],"unknowns":[]}`),
	}}
	persistedLimits := DefaultLimits()
	persistedLimits.MaxTurns = 4
	persistedLimits.MaxTokens = 20_000
	persistedLimits.MaxToolCalls = 4
	persistedLimits.MaxDuration = time.Minute
	fixture := newManagerFixtureWithOptions(
		t, model, ExecutionOptions{Limits: persistedLimits},
	)
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	require.NoError(t, fixture.manager.Close(t.Context()))

	narrow := DefaultLimits()
	narrow.MaxResultBytes = 1
	narrow.MaxFieldBytes = 1
	reopened, err := New(Config{
		Repository: fixture.repository, Parent: fixture.parent, Tree: fixture.tree,
		Model: model, Options: ExecutionOptions{Limits: narrow},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
	detail, err := reopened.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, err)
	assert.IsType(t, ExploreResult{}, detail.Result)

	otherParent, err := fixture.repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   fixture.parent.Metadata().WorkspaceID,
		WorkspacePath: fixture.parent.Metadata().WorkspacePath,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherParent.Close()) })

	other, err := New(Config{
		Repository: fixture.repository, Parent: otherParent, Tree: fixture.tree, Model: model,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close(context.Background())) })
	_, err = other.Inspect(t.Context(), result.ChildSessionID)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestManagerFailsMalformedResultWithDurableReference(t *testing.T) {
	t.Parallel()

	capabilities := ai.Capabilities{Text: true, Tools: true}
	fixture := newManagerFixture(t, &testModel{
		responses:    []*ai.Response{responseText(`{"summary":`)},
		capabilities: &capabilities,
	})
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
	)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	assert.Equal(t, "invalid_result", result.Code)
	assert.NotEmpty(t, result.ChildSessionID)

	detail, inspectErr := fixture.manager.Inspect(t.Context(), result.ChildSessionID)
	require.NoError(t, inspectErr)
	assert.Equal(t, StateFailed, detail.Summary.State)
	assert.Nil(t, detail.Result)
}

func TestManagerKeepsValidExploreResultWhenOneEvidenceItemIsInvalid(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{responses: []*ai.Response{responseText(
		`{"summary":"Located the runtime.","evidence":[` +
			`{"path":"internal/coding/runtime.go","start_line":1,"end_line":4,"claim":"Defines the runtime."},` +
			`{"path":"internal/coding/open.go","start_line":20,"end_line":0,"claim":"Invalid range."}` +
			`],"unknowns":[]}`,
	)}})
	execution, err := fixture.manager.Start(
		t.Context(), Request{Role: RoleExplore, Task: "Locate the runtime."}, nil,
	)
	require.NoError(t, err)

	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	assert.Equal(t, "ok", result.Code)

	value, ok := result.Value.(ExploreResult)
	require.True(t, ok)
	require.Len(t, value.Evidence, 1)
	assert.Equal(t, "internal/coding/runtime.go", value.Evidence[0].Path)
	assert.Equal(t, []string{exploreEvidenceOmissionNotice}, value.Unknowns)
}

func TestManagerReportsWallTimeAndCumulativeTokenBudgets(t *testing.T) {
	t.Parallel()

	t.Run("wall time", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxDuration = time.Millisecond
		fixture := newManagerFixtureWithOptions(
			t,
			&blockingTestModel{entered: make(chan struct{})},
			ExecutionOptions{Limits: limits},
		)
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Wait."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, OutcomeCanceled, result.Outcome)
		assert.Equal(t, "deadline_exceeded", result.Code)
	})

	t.Run("cumulative tokens", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxTokens = 100
		limits.MaxOutputTokens = 100
		fixture := newManagerFixtureWithOptions(
			t,
			&testModel{responses: []*ai.Response{
				responseText(`{"summary":"over budget","evidence":[],"unknowns":[]}`),
			}},
			ExecutionOptions{Limits: limits},
		)
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.Error(t, err)
		assert.Equal(t, OutcomeFailed, result.Outcome)
		assert.Equal(t, "max_tokens", result.Code)
		assert.Nil(t, result.Value)
	})
}

func TestManagerDefaultRunHasNoDeadlineAndExplicitToolBudgetStillStops(t *testing.T) {
	t.Parallel()

	t.Run("default lifetime", func(t *testing.T) {
		t.Parallel()

		model := &contextInspectingModel{inner: &testModel{responses: []*ai.Response{
			responseText(`{"summary":"done","evidence":[],"unknowns":[]}`),
		}}}
		fixture := newManagerFixture(t, model)
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
		)
		require.NoError(t, err)
		_, err = execution.Wait(t.Context())
		require.NoError(t, err)
		assert.False(t, model.SawDeadline())
	})

	t.Run("explicit tool budget", func(t *testing.T) {
		t.Parallel()

		limits := DefaultLimits()
		limits.MaxToolCalls = 1
		model := &testModel{responses: []*ai.Response{
			responseToolCall(ai.ToolCallPart{
				ID: "call-1", Name: readToolName, Args: ai.JSON(`{"path":"sentinel.txt"}`),
			}),
			responseToolCall(ai.ToolCallPart{
				ID: "call-2", Name: readToolName, Args: ai.JSON(`{"path":"sentinel.txt"}`),
			}),
		}}
		fixture := newManagerFixtureWithOptions(t, model, ExecutionOptions{Limits: limits})
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.root, "sentinel.txt"), []byte("content"), 0o600,
		))
		execution, err := fixture.manager.Start(
			t.Context(), Request{Role: RoleExplore, Task: "Inspect."}, nil,
		)
		require.NoError(t, err)
		result, err := execution.Wait(t.Context())
		require.Error(t, err)
		assert.Equal(t, OutcomeFailed, result.Outcome)
		assert.Equal(t, "max_tool_calls", result.Code)
		assert.Equal(t, agent.StopWhen, result.Stop)
	})
}

type managerFixture struct {
	root       string
	manager    *Manager
	repository *session.Repository
	parent     *session.Handle
	tree       *workspace.Tree
}

func newManagerFixture(t *testing.T, model ai.LanguageModel) managerFixture {
	t.Helper()

	return newManagerFixtureWithOptions(t, model, ExecutionOptions{})
}

func newManagerFixtureWithOptions(
	t *testing.T,
	model ai.LanguageModel,
	options ExecutionOptions,
) managerFixture {
	t.Helper()

	return newManagerFixtureWithConfig(t, model, options, nil)
}

func newManagerFixtureWithConfig(
	t *testing.T,
	model ai.LanguageModel,
	options ExecutionOptions,
	configure func(*Config),
) managerFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(root, 0o700))
	value, err := workspace.Open(root)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(value)
	require.NoError(t, err)
	repository, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: value.Identity().Key(), WorkspacePath: value.Root(),
	})
	require.NoError(t, err)
	managerConfig := Config{
		Repository: repository, Parent: parent, Tree: tree, Model: model,
		Options: options,
	}
	if configure != nil {
		configure(&managerConfig)
	}
	manager, err := New(managerConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		require.NoError(t, manager.Close(ctx))
		require.NoError(t, parent.Close())
		require.NoError(t, tree.Close())
	})

	return managerFixture{
		root: root, manager: manager, repository: repository, parent: parent, tree: tree,
	}
}

type testModel struct {
	mu                   sync.Mutex
	responses            []*ai.Response
	requests             []ai.Request
	capabilities         *ai.Capabilities
	rejectResponseFormat bool
}

func (m *testModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, request)
	if m.rejectResponseFormat && request.ResponseFormat != nil {
		return nil, errors.New("test model rejects response format")
	}

	if len(m.responses) == 0 {
		return nil, errors.New("test model exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *testModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageStart, Provider: response.Provider, Model: response.Model}, nil)

		for index, part := range response.Message.Parts {
			switch value := part.(type) {
			case ai.TextPart:
				text := value
				yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: text.Text}, nil)
			case ai.ToolCallPart:
				yield(ai.StreamEvent{
					Type: ai.StreamToolCallStart, ToolCallIndex: index,
					ToolCallID: value.ID, ToolCallName: value.Name,
				}, nil)
				yield(ai.StreamEvent{
					Type: ai.StreamToolCallDelta, ToolCallIndex: index,
					ArgsDelta: string(value.Args),
				}, nil)
				yield(ai.StreamEvent{
					Type: ai.StreamToolCallEnd, ToolCallIndex: index,
				}, nil)
			}
		}

		usage := response.Usage
		yield(ai.StreamEvent{
			Type: ai.StreamMessageEnd, FinishReason: response.FinishReason, Usage: &usage,
		}, nil)
	}
}

func (*testModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*testModel) ModelID() string       { return "subagent-test" }
func (m *testModel) Capabilities() ai.Capabilities {
	if m.capabilities != nil {
		return *m.capabilities
	}

	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

func (m *testModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.requests)
}

type blockingTestModel struct {
	once    sync.Once
	entered chan struct{}
}

type contextInspectingModel struct {
	inner       *testModel
	mu          sync.Mutex
	sawDeadline bool
}

func (m *contextInspectingModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	m.recordDeadline(ctx)

	return m.inner.Generate(ctx, request)
}

func (m *contextInspectingModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	m.recordDeadline(ctx)

	return m.inner.Stream(ctx, request)
}

func (m *contextInspectingModel) Provider() ai.Provider         { return m.inner.Provider() }
func (m *contextInspectingModel) ModelID() string               { return m.inner.ModelID() }
func (m *contextInspectingModel) Capabilities() ai.Capabilities { return m.inner.Capabilities() }

func (m *contextInspectingModel) recordDeadline(ctx context.Context) {
	_, deadline := ctx.Deadline()
	m.mu.Lock()
	m.sawDeadline = m.sawDeadline || deadline
	m.mu.Unlock()
}

func (m *contextInspectingModel) SawDeadline() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sawDeadline
}

func (m *blockingTestModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.entered) })
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingTestModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		_, err := m.Generate(ctx, request)
		yield(ai.StreamEvent{}, err)
	}
}

func (*blockingTestModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (*blockingTestModel) ModelID() string               { return "subagent-blocking" }
func (*blockingTestModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func responseText(text string) *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "subagent-test",
		Message: ai.AssistantText(text), FinishReason: ai.FinishStop,
		Usage: ai.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

func responseToolCall(call ai.ToolCallPart) *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "subagent-test",
		Message:      ai.Message{Role: ai.RoleAssistant, Parts: []ai.Part{call}},
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

func toolNames(tools []ai.Tool) []string {
	values := make([]string, len(tools))
	for index, tool := range tools {
		values[index] = tool.Name
	}

	return values
}

func requestText(request ai.Request) string {
	var result strings.Builder
	for _, message := range request.Messages {
		result.WriteString(messageText(message))
	}

	return result.String()
}

func countChildCompactions(entries []harness.Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.Kind == harness.KindCompaction {
			count++
		}
	}

	return count
}

func expectedResult(role Role) any {
	switch role {
	case RoleExplore:
		return ExploreResult{}
	case RolePlan:
		return PlanResult{}
	default:
		return ReviewResult{}
	}
}

var (
	_ ai.LanguageModel = (*testModel)(nil)
	_ ai.LanguageModel = (*blockingTestModel)(nil)
)
