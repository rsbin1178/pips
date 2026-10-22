package coding

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	codingmodel "github.com/rsbin1178/pips/internal/coding/model"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type emptyRuntimeModel struct {
	mu       sync.Mutex
	calls    int
	empty    int
	startEOF bool
}

func (m *emptyRuntimeModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("emptyRuntimeModel: Generate is not scripted")
}

func (m *emptyRuntimeModel) Stream(context.Context, ai.Request) ai.Stream {
	m.mu.Lock()
	m.calls++
	attempt := m.calls
	m.mu.Unlock()

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, ID: "response-runtime", Provider: m.Provider(), Model: m.ModelID(),
		}, nil) {
			return
		}

		if m.empty < 0 || attempt <= m.empty {
			if !m.startEOF {
				yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
			}

			return
		}

		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "recovered answer"}, nil) {
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *emptyRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *emptyRuntimeModel) ModelID() string       { return "empty-runtime" }
func (m *emptyRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *emptyRuntimeModel) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

func TestRuntimeRecoversRejectedResponseAndPersistsSafeDiagnostic(t *testing.T) {
	t.Parallel()

	for _, startEOF := range []bool{false, true} {
		name := "empty_completed"
		if startEOF {
			name = "start_only_eof"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			base := &emptyRuntimeModel{empty: 1, startEOF: startEOF}
			model := ai.Chain(codingmodel.RequireOutput(base), retry.New(
				retry.WithMaxAttempts(3), retry.WithBaseDelay(0),
			))
			runtime := openTestRuntime(t, model)
			events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
			assertEventSequence(t, events)
			assert.Equal(t, 2, base.attempts())

			snapshot := runtime.Snapshot()
			assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
			assert.Equal(t, agent.StopEndTurn, snapshot.Interaction.Stop)
			assert.Nil(t, snapshot.LastError)
			require.Len(t, snapshot.Transcript, 2, "failed candidates never commit an empty answer")
			assert.Empty(t, snapshot.Draft)
			assert.False(t, snapshot.Retry.Active)

			kind := "empty_response"
			if startEOF {
				kind = "incomplete_stream"
			}

			require.Equal(t, 1, countDiagnostic(events, "model", kind))
			diagnostic := persistedModelDiagnostic(t, runtime)
			assert.Equal(t, kind, diagnostic.Code)
			require.NotNil(t, diagnostic.Response)
			assert.Equal(t, !startEOF, diagnostic.Response.TerminalObserved)
			assert.Equal(t, 1, diagnostic.Response.Attempt)
			assert.Equal(t, 2, diagnostic.Response.MaxRetries)
			assert.Equal(t, "response-runtime", diagnostic.Response.ResponseID)
			assert.Zero(t, diagnostic.Response.TextBytes)

			for _, event := range events {
				if notice, ok := event.Payload.(ModelRetry); ok {
					require.NotNil(t, notice.Response)
					assert.Equal(t, kind, notice.Response.Kind)
				}
			}
		})
	}
}

func TestRuntimeEmptyResponseExhaustionFailsWithoutCommittingAnswer(t *testing.T) {
	t.Parallel()

	base := &emptyRuntimeModel{empty: -1}
	model := ai.Chain(codingmodel.RequireOutput(base), retry.New(
		retry.WithMaxAttempts(3), retry.WithBaseDelay(0),
	))
	runtime := openTestRuntime(t, model)
	events, runErr := collectRuntimeEventsAndError(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	require.ErrorIs(t, runErr, ai.ErrEmptyResponse)
	assertEventSequence(t, events)
	assert.Equal(t, 3, base.attempts(), "Agent recovery must not multiply the middleware budget")
	assert.NotContains(t, eventTypes(events), EventRunCompleted)

	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.Equal(t, InteractionFailed, snapshot.Interaction.Outcome)
	require.Len(t, snapshot.Transcript, 1)
	assert.Empty(t, snapshot.Draft)
	require.NotNil(t, snapshot.LastError)
	assert.Equal(t, "model_response_empty", snapshot.LastError.Code)
	require.NotNil(t, snapshot.LastError.Response)
	assert.True(t, snapshot.LastError.Response.TerminalObserved)
	assert.Equal(t, "empty_response", snapshot.LastError.Response.Kind)

	clone := snapshot.Clone()

	clone.LastError.Response.TextBytes = 91
	for index := range clone.Diagnostics {
		if clone.Diagnostics[index].Response != nil {
			clone.Diagnostics[index].Response.TextBytes = 91
		}
	}

	assert.Zero(t, runtime.Snapshot().LastError.Response.TextBytes)
	assert.Zero(t, snapshot.LastError.Response.TextBytes)

	assert.Equal(t, 2, countDiagnostic(events, "model", "empty_response"))
	diagnostic := persistedModelDiagnostic(t, runtime)
	assert.Equal(t, 2, diagnostic.Response.Attempt)
}

func TestRuntimeCancellationDuringEmptyResponseRecoveryStopsRetries(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	base := &emptyRuntimeModel{empty: -1}
	model := ai.Chain(codingmodel.RequireOutput(base), retry.New(
		retry.WithMaxAttempts(3), retry.WithSleep(func(ctx context.Context, _ time.Duration) error {
			cancel()

			return ctx.Err()
		}),
	))
	runtime := openTestRuntime(t, model)
	_, runErr := collectRuntimeEventsAndError(t, runtime.Prompt(ctx, ai.UserText("hello")))
	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, 1, base.attempts())

	snapshot := runtime.Snapshot()
	assert.Equal(t, InteractionCanceled, snapshot.Interaction.Outcome)
	require.NotNil(t, snapshot.LastError)
	assert.Equal(t, "run_canceled", snapshot.LastError.Code)
	assert.Nil(t, snapshot.LastError.Response)
	require.Len(t, snapshot.Transcript, 1)
}

func persistedModelDiagnostic(t *testing.T, runtime *Runtime) IntegrationDiagnostic {
	t.Helper()

	records, err := session.ReadJournal(filepath.Join(runtime.handle.Dir(), session.JournalFileName), 0)
	require.NoError(t, err)

	var result IntegrationDiagnostic

	for _, record := range records {
		if record.Type != string(EventIntegrationDiagnostic) {
			continue
		}

		var diagnostic IntegrationDiagnostic
		require.NoError(t, json.Unmarshal(record.Payload, &diagnostic))

		if diagnostic.Component == "model" {
			result = diagnostic
		}
	}

	require.Equal(t, "model", result.Component, "recovery diagnostics survive successful retries")

	return result
}
