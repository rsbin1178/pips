//nolint:wsl_v5 // Keep scripted model setup adjacent to behavioral assertions.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type goalTestModel struct {
	*runtimeModel
	muGoal            sync.Mutex
	outcomes          []string
	evaluationError   bool
	verifierInvalid   bool
	blockEvaluation   chan struct{}
	evaluationStarted chan struct{}
	evaluated         int
	verifications     int
}

func newGoalTestModel(work ...*ai.Response) *goalTestModel {
	return &goalTestModel{runtimeModel: newRuntimeModel(work...)}
}

func (m *goalTestModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

//nolint:nestif // The scripted model distinguishes independent request roles.
func (m *goalTestModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if request.ResponseFormat != nil && request.ResponseFormat.Name == "goal_evaluation" {
		m.muGoal.Lock()
		m.evaluated++
		failure := m.evaluationError
		outcome := "complete"
		if len(m.outcomes) > 0 {
			outcome = m.outcomes[0]
			m.outcomes = m.outcomes[1:]
		}
		m.muGoal.Unlock()
		if m.evaluationStarted != nil {
			select {
			case m.evaluationStarted <- struct{}{}:
			default:
			}
		}
		if m.blockEvaluation != nil {
			select {
			case <-m.blockEvaluation:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if failure {
			return &ai.Response{Usage: ai.Usage{InputTokens: 5, OutputTokens: 2}}, errors.New("assessment unavailable")
		}
		return runtimeTextResponse(`{"outcome":"` + outcome + `","reason":"inspect actual implementation"}`), nil
	}
	if strings.Contains(requestSystemText(request), "independent Coding Goal evidence auditor") {
		if request.ResponseFormat == nil {
			for _, message := range request.Messages {
				if _, ok := message.(ai.ToolMessage); ok {
					return runtimeTextResponse("evidence read"), nil
				}
			}
			return runtimeToolResponse("audit-read", "read", `{"path":"proof.txt"}`), nil
		}
		m.muGoal.Lock()
		m.verifications++
		invalid := m.verifierInvalid
		m.muGoal.Unlock()
		if invalid {
			return runtimeTextResponse(`{"verified":true,"reason":"looks good","gaps":[],"references":[]}`), nil
		}
		references := []string{"read:audit-read"}
		for _, message := range request.Messages {
			var payload struct {
				Evidence goalflow.Evidence `json:"evidence"`
			}
			if json.Unmarshal([]byte(runtimeMessageText(message)), &payload) == nil {
				for _, record := range payload.Evidence.Records {
					references = append(references, record.ID)
				}
			}
		}
		data, _ := json.Marshal(goalflow.Verdict{Verified: true, Reason: "actual file matches captured read", Gaps: []string{}, References: references})
		return runtimeTextResponse(string(data)), nil
	}
	return m.runtimeModel.Generate(ctx, request)
}

func (m *goalTestModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func TestGoalRequestValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		request GoalRequest
	}{
		{name: "empty", request: GoalRequest{}},
		{name: "oversize", request: GoalRequest{Condition: strings.Repeat("界", 4001)}},
		{name: "negative budget", request: GoalRequest{Condition: "done", MaxTokens: -1}},
		{name: "invalid text", request: GoalRequest{Condition: "bad\x00condition"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Error(t, test.request.Validate())
		})
	}
	require.NoError(t, (GoalRequest{Condition: strings.Repeat("界", 4000)}).Validate())
}

func TestRuntimeGoalContinuesThenVerifiesActualEvidence(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(
		runtimeTextResponse("need to inspect"),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.outcomes = []string{"continue", "complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))
	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)
	assert.Equal(t, 2, state.Evaluations)
	assert.Positive(t, state.Tokens)
	assert.Equal(t, 1, model.verifications)
	assertEventSequence(t, events)
	assert.Contains(t, eventTypes(events), EventGoalChanged)
	assert.NotEmpty(t, runtime.Snapshot().SyntheticMessages)
	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.True(t, requestContainsText(requests[1], "Independent evidence gaps"))
}

func TestRuntimeGoalDecisionRetryDoesNotReplayWork(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	model := newGoalTestModel(runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	model.evaluationError = true
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.Error(t, err)
	first := runtime.GoalSnapshot()
	require.Equal(t, GoalInterrupted, first.Status)
	assert.Equal(t, 7, first.FailedCheckTokens)
	execution, err := runtime.goalControl.engine.Get(t.Context(), continuation.ID(first.ID))
	require.NoError(t, err)
	attemptID := execution.CurrentAttempt.ID
	sessionID := runtime.Snapshot().SessionID
	require.NoError(t, runtime.Close(t.Context()))
	reopenedModel := newGoalTestModel()
	reopened := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, reopenedModel)
	assert.Equal(t, GoalPaused, reopened.GoalSnapshot().Status)
	assert.Empty(t, reopenedModel.Requests())
	_, err = collectRuntimeResult(reopened.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.True(t, reopened.GoalSnapshot().Completed())
	assert.Equal(t, first.Attempts, reopened.GoalSnapshot().Attempts)
	assert.Equal(t, 7, reopened.GoalSnapshot().FailedCheckTokens)
	after, err := reopened.goalControl.engine.Get(t.Context(), continuation.ID(first.ID))
	require.NoError(t, err)
	require.NotNil(t, after.LastAttempt)
	assert.Equal(t, attemptID, after.LastAttempt.ID)
	assert.Empty(t, reopenedModel.Requests())
}

func TestRuntimeGoalRejectsUnsupportedAndUnprovenCompletion(t *testing.T) {
	t.Parallel()
	t.Run("unsupported", func(t *testing.T) {
		t.Parallel()
		runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
		require.Error(t, err)
		assert.Empty(t, runtime.GoalSnapshot().ID)
	})
	for _, name := range []string{"self report", "empty verification", "failed evidence", "truncated evidence"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			work := []*ai.Response{}
			for attempt := range 4 {
				if name != "self report" {
					work = append(work, runtimeToolResponse(fmt.Sprintf("proof-read-%d", attempt), "read", `{"path":"proof.txt"}`))
				}
				work = append(work, runtimeTextResponse("I claim everything is verified"))
			}
			model := newGoalTestModel(work...)
			model.verifierInvalid = name == "empty verification"
			runtime := openTestRuntime(t, model)
			content := "proof\n"
			if name == "truncated evidence" {
				content = strings.Repeat("proof line\n", 2000)
			}
			if name != "failed evidence" {
				require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte(content), 0o600))
			}
			_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "prove implementation"}))
			require.NoError(t, err)
			assert.False(t, runtime.GoalSnapshot().Completed())
			assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
			assert.NotEmpty(t, runtime.GoalSnapshot().Gaps)
			assert.GreaterOrEqual(t, runtime.GoalSnapshot().Attempts, 3)
		})
	}
}

func TestRuntimeGoalNoProgressAndBudget(t *testing.T) {
	t.Parallel()
	t.Run("three empty assessments", func(t *testing.T) {
		t.Parallel()
		model := newGoalTestModel(runtimeTextResponse("claim1"), runtimeTextResponse("claim2"), runtimeTextResponse("claim3"))
		model.outcomes = []string{"continue", "continue", "continue"}
		runtime := openTestRuntime(t, model)
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
		require.NoError(t, err)
		assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
		assert.Equal(t, 3, runtime.GoalSnapshot().Evaluations)
		assert.Equal(t, 3, runtime.GoalSnapshot().Attempts)
	})
	t.Run("work token overshoot", func(t *testing.T) {
		t.Parallel()
		model := newGoalTestModel(runtimeTextResponse("candidate"))
		runtime := openTestRuntime(t, model)
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done", MaxTokens: 1}))
		require.NoError(t, err)
		assert.Equal(t, GoalLimited, runtime.GoalSnapshot().Status)
		assert.Equal(t, 1, runtime.GoalSnapshot().Attempts)
		assert.Equal(t, 0, model.evaluated)
	})
}

func TestRuntimeGoalApprovalResolutionKeepsExactPendingCalls(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(
		runtimeToolResponse("pending-shell", "shell", `{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`),
		runtimeTextResponse("denied"), runtimeTextResponse("no evidence"), runtimeTextResponse("still none"), runtimeTextResponse("no new evidence"),
	)
	model.outcomes = []string{"continue", "continue", "continue", "continue"}
	runtime := openTestRuntime(t, model)
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "run command"}))
	require.NoError(t, err)
	assert.Equal(t, GoalBlocked, runtime.GoalSnapshot().Status)
	assert.Equal(t, 0, model.evaluated)
	paused := runtime.Snapshot()
	require.NotNil(t, paused.Approval.Required)
	require.NoError(t, runtime.PauseGoal(t.Context()))
	_, err = collectRuntimeResult(runtime.Resolve(t.Context(), approval.Resolution{RequestID: paused.Approval.Required.RequestID, Choice: approval.ChoiceDeny}))
	require.ErrorIs(t, err, ErrRuntimePending)
	assert.Len(t, model.Requests(), 1)
	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.Equal(t, GoalBlocked, runtime.GoalSnapshot().Status)
	_, err = collectRuntimeResult(runtime.Resolve(t.Context(), approval.Resolution{RequestID: paused.Approval.Required.RequestID, Choice: approval.ChoiceDeny}))
	require.NoError(t, err)
	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.Equal(t, 5, runtime.GoalSnapshot().Attempts)
	requests := model.Requests()
	require.Len(t, requests, 5)
	assert.Equal(t, 1, countUserMessages(requests[1].Messages))
	results := 0
	for _, entry := range runtime.session.Entries() {
		if message, ok := entry.Message.(ai.ToolMessage); ok {
			for _, part := range message.Parts {
				if part.ToolCallID == "pending-shell" {
					results++
				}
			}
		}
	}
	assert.Equal(t, 1, results)
}

func TestRuntimeGoalWaitUsesNotificationAndCannotRearmPausedGoal(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeTextResponse("waiting for child"), runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	runtime := openTestRuntime(t, model)
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"})(func(event Event, err error) bool {
		require.NoError(t, err)
		if event.Type == EventInteractionCompleted {
			require.NoError(t, runtime.notifications.Enqueue(testAgentNotification(runtime, "s-goal-child", event.InteractionID)))
		}
		return true
	})
	require.Equal(t, GoalWaiting, runtime.GoalSnapshot().Status)
	assert.Equal(t, 0, model.evaluated)
	require.NoError(t, runtime.PauseGoal(t.Context()))
	runtime.deliverNotificationBatch(t.Context())
	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.Len(t, model.Requests(), 1)
	_, err := collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	runtime.deliverNotificationBatch(t.Context())
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())
	assert.Equal(t, 2, runtime.GoalSnapshot().Attempts)
	pending, err := runtime.notifications.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestRuntimeGoalDoesNotStartWork26(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel()
	runtime := openTestRuntime(t, model)
	for index := range 25 {
		name := fmt.Sprintf("proof-%d.txt", index)
		require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), name), []byte(name), 0o600))
		model.responses = append(model.responses, runtimeToolResponse(fmt.Sprintf("call-%d", index), "read", fmt.Sprintf(`{"path":%q}`, name)), runtimeTextResponse("continue"))
		model.outcomes = append(model.outcomes, "continue")
	}
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "keep inspecting"}))
	require.NoError(t, err)
	assert.Equal(t, GoalLimited, runtime.GoalSnapshot().Status)
	assert.Equal(t, 25, runtime.GoalSnapshot().Attempts)
	assert.Equal(t, 25, runtime.GoalSnapshot().Evaluations)
	assert.Len(t, model.Requests(), 50)
}

func TestRuntimeGoalPauseDuringAssessmentStartsNoAdditionalWork(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeTextResponse("candidate"))
	model.evaluationStarted = make(chan struct{}, 1)
	model.blockEvaluation = make(chan struct{})
	runtime := openTestRuntime(t, model)
	done := make(chan error, 1)
	go func() {
		_, err := collectRuntimeResult(runtime.StartGoal(context.Background(), GoalRequest{Condition: "done"}))
		done <- err
	}()
	<-model.evaluationStarted
	require.NoError(t, runtime.PauseGoal(t.Context()))
	<-done
	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.Len(t, model.Requests(), 1)
	assert.Equal(t, 1, runtime.GoalSnapshot().Attempts)
	require.NoError(t, runtime.ClearGoal(t.Context()))
	assert.Empty(t, runtime.GoalSnapshot().ID)
}
