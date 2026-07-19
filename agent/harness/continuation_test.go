package harness_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type continuationControllerFunc func(context.Context, continuation.DecisionRequest) (continuation.Decision, error)

func (function continuationControllerFunc) Decide(
	ctx context.Context,
	request continuation.DecisionRequest,
) (continuation.Decision, error) {
	return function(ctx, request)
}

func TestContinuationWorkerRunsOneHarnessPromptPerAttempt(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		textResponse("first answer", 7),
		textResponse("second answer", 11),
	)
	session := buildSession(t)
	h, err := harness.New(model, session)
	require.NoError(t, err)

	worker, err := harness.NewContinuationWorker(h,
		func(_ context.Context, request continuation.WorkRequest) ([]ai.Message, error) {
			return []ai.Message{ai.UserText(string(request.Input))}, nil
		},
	)
	require.NoError(t, err)

	store, err := continuation.NewMemoryStore()
	require.NoError(t, err)
	engine, err := continuation.New(store)
	require.NoError(t, err)

	workerRef := continuation.HandlerRef{Kind: "harness", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "test", Version: "v1"}
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID: "harness-execution", Target: continuation.Target{Kind: "harness_session", ID: session.Metadata().ID},
		Worker: workerRef, Controller: controllerRef, Input: ai.JSON(`"first prompt"`),
	})
	require.NoError(t, err)

	controller := continuationControllerFunc(func(
		_ context.Context,
		request continuation.DecisionRequest,
	) (continuation.Decision, error) {
		var evidence struct {
			Stop      agent.StopReason `json:"stop"`
			Text      string           `json:"text"`
			LeafAfter string           `json:"leaf_after"`
		}
		require.NoError(t, json.Unmarshal(request.Work.Value, &evidence))
		require.Equal(t, agent.StopEndTurn, evidence.Stop)
		require.NotEmpty(t, evidence.LeafAfter)

		if request.Attempt == 1 {
			assert.Equal(t, "first answer", evidence.Text)

			return continuation.Decision{
				Action: continuation.ActionContinue, NextInput: ai.JSON(`"second prompt"`),
				Progress: continuation.ProgressChanged,
			}, nil
		}

		assert.Equal(t, "second answer", evidence.Text)

		return continuation.Decision{Action: continuation.ActionComplete}, nil
	})
	handlers := continuation.Handlers{
		WorkerRef: workerRef, Worker: worker,
		ControllerRef: controllerRef, Controller: controller,
	}

	execution, err = engine.Advance(t.Context(), execution.ID, execution.Revision, handlers)
	require.NoError(t, err)
	execution, err = engine.Advance(t.Context(), execution.ID, execution.Revision, handlers)
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusCompleted, execution.Status)
	assert.Len(t, model.Requests(), 2)
	assert.Len(t, session.Entries(), 4)
	assert.Equal(t, 2, execution.Accounting.Turns)
	assert.Equal(t, 28, execution.Accounting.Tokens())
}

func TestContinuationWorkerPropagatesCancellationAndRestoresHarness(t *testing.T) {
	t.Parallel()

	model := newBlockingModel()
	session := buildSession(t)
	h, err := harness.New(model, session)
	require.NoError(t, err)
	worker, err := harness.NewContinuationWorker(h,
		func(context.Context, continuation.WorkRequest) ([]ai.Message, error) {
			return []ai.Message{ai.UserText("block")}, nil
		},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	type workerOutcome struct {
		result continuation.WorkResult
		err    error
	}

	outcome := make(chan workerOutcome, 1)

	go func() {
		result, runErr := worker.Run(ctx, continuation.WorkRequest{})
		outcome <- workerOutcome{result: result, err: runErr}
	}()

	<-model.started
	cancel()

	completed := <-outcome
	require.ErrorIs(t, completed.err, context.Canceled)
	assert.Equal(t, harness.PhaseIdle, h.Phase())
	assert.Equal(t, continuation.ProgressChanged, completed.result.Progress)
}

func TestContinuationWorkerPreservesPausedStopReason(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", callResponse("c1", "add", `{"a":1,"b":1}`))
	session := buildSession(t)
	h, err := harness.New(model, session,
		harness.WithTools(addTool()),
		harness.WithAgentOptions(agent.WithBeforeTool(
			func(context.Context, agent.ToolCallInfo) agent.Decision {
				return agent.Decision{Action: agent.Pause}
			},
		)),
	)
	require.NoError(t, err)
	worker, err := harness.NewContinuationWorker(h,
		func(context.Context, continuation.WorkRequest) ([]ai.Message, error) {
			return []ai.Message{ai.UserText("pause")}, nil
		},
	)
	require.NoError(t, err)

	result, err := worker.Run(t.Context(), continuation.WorkRequest{})
	require.NoError(t, err)

	var evidence struct {
		Stop agent.StopReason `json:"stop"`
	}
	require.NoError(t, json.Unmarshal(result.Value, &evidence))
	assert.Equal(t, agent.StopPaused, evidence.Stop)
}
