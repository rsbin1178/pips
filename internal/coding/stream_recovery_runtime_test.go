package coding

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interruptedRuntimeModel streams a partial answer and then fails once, so the
// Coding recovery path can be observed end to end: the second attempt answers.
type interruptedRuntimeModel struct {
	mu      sync.Mutex
	calls   int
	failure error
	reply   string
}

func (m *interruptedRuntimeModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("interruptedRuntimeModel: Generate is not scripted")
}

func (m *interruptedRuntimeModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	m.mu.Lock()
	m.calls++
	attempt := m.calls
	m.mu.Unlock()

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp-1"}, nil) {
			return
		}

		if attempt == 1 {
			if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "half"}, nil) {
				return
			}

			yield(ai.StreamEvent{}, m.failure)

			return
		}

		yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: m.reply}, nil)
		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *interruptedRuntimeModel) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

func (m *interruptedRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *interruptedRuntimeModel) ModelID() string       { return "interrupted-runtime" }
func (m *interruptedRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

// TestRuntimeReissuesInterruptedStreamAndReportsTheWait covers the failure this
// change was written for: the provider truncates the response body, the turn is
// re-issued, and the frontend is told about the wait instead of appearing slow.
func TestRuntimeReissuesInterruptedStreamAndReportsTheWait(t *testing.T) {
	t.Parallel()

	model := &interruptedRuntimeModel{failure: io.ErrUnexpectedEOF, reply: "recovered"}

	runtime := openTestRuntime(t, model)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assertEventSequence(t, events)

	var notices []ModelRetry

	for _, event := range events {
		if retry, ok := event.Payload.(ModelRetry); ok {
			notices = append(notices, retry)
		}
	}

	require.Len(t, notices, 1)
	assert.Equal(t, 1, notices[0].Attempt, "the notice counts retries, not tries")
	assert.Equal(t, streamRecoveryAttempts, notices[0].MaxRetries)
	assert.Equal(t, "stream ended early", notices[0].Reason)
	assert.Contains(t, eventTypes(events), EventMessageDiscarded,
		"the partial candidate is retracted before the re-issue")

	// The notice is live state rather than transcript, and the reducer keeps it
	// only for the duration of the wait (see the reducer test in
	// stream_retry_test.go).
	snapshot := runtime.Snapshot()
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	assert.False(t, snapshot.Retry.Active, "the wait ends with the next attempt")
	assert.Equal(t, 2, model.attempts())

	require.Len(t, snapshot.Transcript, 2)
	assistant, ok := snapshot.Transcript[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "recovered", text.Text)

	require.NoError(t, runtime.Close(t.Context()))
}
