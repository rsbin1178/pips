package coding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
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

	// Continuation keeps the partial answer, so the turn neither retracts the
	// candidate nor reports it as incomplete. The committed message is the
	// retained prefix followed by the remainder the re-issue produced.
	assert.NotContains(t, eventTypes(events), EventMessageDiscarded,
		"continuation never retracts the partial candidate")
	assert.NotContains(t, eventTypes(events), EventMessageIncomplete,
		"a successful continuation is not an incomplete reply")

	// The notice is live state rather than transcript, and the reducer keeps it
	// only for the duration of the wait (see the reducer test in
	// stream_retry_test.go).
	snapshot := runtime.Snapshot()
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	assert.False(t, snapshot.Retry.Active, "the wait ends with the next attempt")
	assert.Empty(t, snapshot.IncompleteReplies)
	assert.Equal(t, 2, model.attempts())

	require.Len(t, snapshot.Transcript, 2)
	assistant, ok := snapshot.Transcript[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "halfrecovered", text.Text,
		"the committed answer is the retained prefix plus the continuation")

	require.NoError(t, runtime.Close(t.Context()))
}

// abandonedRuntimeModel streams a partial answer and then fails with a
// non-retryable error, so the give-up path is exercised without waiting out
// the recovery backoff.
type abandonedRuntimeModel struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (m *abandonedRuntimeModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("abandonedRuntimeModel: Generate is not scripted")
}

func (m *abandonedRuntimeModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp-1"}, nil) {
			return
		}

		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: m.reply}, nil) {
			return
		}

		yield(ai.StreamEvent{}, fmt.Errorf("%w: provider refused the continuation", ai.ErrInvalidRequest))
	}
}

func (m *abandonedRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *abandonedRuntimeModel) ModelID() string       { return "abandoned-runtime" }
func (m *abandonedRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

// TestRuntimeRetainsAbandonedPartialAsIncompleteReply covers the honest half of
// the give-up path: when recovery cannot continue, the text the model already
// produced is kept as an explicitly incomplete reply, never as a committed
// answer, and is durably recorded.
func TestRuntimeRetainsAbandonedPartialAsIncompleteReply(t *testing.T) {
	t.Parallel()

	model := &abandonedRuntimeModel{reply: "half"}

	runtime := openTestRuntime(t, model)
	events, runErr := collectRuntimeEventsAndError(
		t,
		runtime.Prompt(t.Context(), ai.UserText("hello")),
	)
	require.Error(t, runErr, "the abandoned run surfaces its failure")
	assertEventSequence(t, events)
	assert.Equal(t, 1, model.calls, "a non-retryable failure is not re-issued")

	assert.Contains(t, eventTypes(events), EventMessageIncomplete)
	assert.NotContains(t, eventTypes(events), EventMessageDiscarded)

	var replies []IncompleteReply

	for _, event := range events {
		if reply, ok := event.Payload.(IncompleteReply); ok {
			replies = append(replies, reply)
		}
	}

	require.Len(t, replies, 1)
	assert.Equal(t, 1, replies[0].Turn)
	assert.Equal(t, "half", replies[0].Text)
	assert.Equal(t, len("half"), replies[0].Bytes)
	assert.NotEmpty(t, replies[0].Reason)

	snapshot := runtime.Snapshot()
	require.Len(t, snapshot.IncompleteReplies, 1)
	assert.Equal(t, "half", snapshot.IncompleteReplies[0].Text)
	assert.Empty(t, snapshot.Draft, "the give-up path leaves no live draft")
	require.Len(t, snapshot.Transcript, 1, "only the user input is committed")
	_, isAssistant := snapshot.Transcript[0].(ai.AssistantMessage)
	assert.False(t, isAssistant, "retained text must never enter the transcript")

	// The durable record restores the retained text on reopen.
	restored, err := BootstrapState(BootstrapOptions{
		SessionID: snapshot.SessionID, Provider: snapshot.Provider, ModelID: snapshot.ModelID,
		PlanMode: planmode.StateInactive, Path: runtime.session.Path(),
	})
	require.NoError(t, err)
	require.Len(t, restored.State.IncompleteReplies, 1)
	assert.Equal(t, "half", restored.State.IncompleteReplies[0].Text)

	require.NoError(t, runtime.Close(t.Context()))
}
