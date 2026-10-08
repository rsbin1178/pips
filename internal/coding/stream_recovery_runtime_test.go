package coding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
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

	// The default recovery regenerates, so the partial the consumer was looking
	// at is retracted before the re-issue streams — and because the turn
	// succeeds it is superseded rather than reported as an incomplete reply.
	assert.Contains(t, eventTypes(events), EventMessageDiscarded,
		"regenerating retracts the partial candidate it replaces")
	assert.NotContains(t, eventTypes(events), EventMessageIncomplete,
		"a superseded fragment is not an incomplete reply")

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
	assert.Equal(t, "recovered", text.Text,
		"the committed answer is the re-issue's own answer, not a glued fragment")

	require.NoError(t, runtime.Close(t.Context()))
}

// TestRuntimePersistsRetryTallyInTheInteractionJournal covers the gap this
// change exists for: a retry is live state, so without a durable tally the only
// way to know how much retrying an interaction needed was to watch it happen.
func TestRuntimePersistsRetryTallyInTheInteractionJournal(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := &interruptedRuntimeModel{failure: io.ErrUnexpectedEOF, reply: "recovered"}

	first := openTestRuntimeAt(t, base, SessionTarget{}, model)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("hello")))

	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	// Reopen rather than reading the live runtime's path: the tally has to be
	// durable, not merely in memory.
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, model)

	tally, terminals := recordedRetryTally(t, second.journal.store.Path())

	assert.Equal(t, 1, terminals, "one interaction writes one terminal record")
	assert.Equal(t, 1, tally.Retries)
	assert.Equal(t, map[string]int{"stream ended early": 1}, tally.RetryReasons)

	require.NoError(t, second.Close(t.Context()))
}

// recordedRetryTally reads the retry accounting off the most recent terminal
// interaction record in a durable session path, and reports how many terminal
// records the path holds.
func recordedRetryTally(t *testing.T, path []harness.Entry) (interactionTally, int) {
	t.Helper()

	var (
		tally     interactionTally
		terminals int
		found     bool
	)

	for entry := range slices.Values(path) {
		if entry.Kind != harness.KindCustom || entry.Custom != interactionCustomType {
			continue
		}

		record, err := decodeInteractionRecord(entry.Data)
		require.NoError(t, err)

		if record.Event != interactionTerminalEvent {
			continue
		}

		terminals++
		tally = interactionTally{Retries: record.Retries, RetryReasons: record.RetryReasons}
		found = true
	}

	require.True(t, found, "the durable path holds a terminal interaction record")

	return tally, terminals
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

// continuationRuntimeModel streams a partial answer and then breaks once,
// recording every request so the test can prove the re-issue carried the prefix
// as a trailing assistant message rather than answering from scratch.
type continuationRuntimeModel struct {
	mu       sync.Mutex
	calls    int
	requests []ai.Request
}

func (m *continuationRuntimeModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("continuationRuntimeModel: Generate is not scripted")
}

func (m *continuationRuntimeModel) Stream(_ context.Context, request ai.Request) ai.Stream {
	m.mu.Lock()
	m.calls++
	attempt := m.calls
	m.requests = append(m.requests, request)
	m.mu.Unlock()

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp-1"}, nil) {
			return
		}

		if attempt == 1 {
			if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "half the answer"}, nil) {
				return
			}

			yield(ai.StreamEvent{}, io.ErrUnexpectedEOF)

			return
		}

		yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: " and the rest"}, nil)
		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *continuationRuntimeModel) requestsSnapshot() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

func (m *continuationRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (m *continuationRuntimeModel) ModelID() string       { return "continuation-runtime" }
func (m *continuationRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

// TestRuntimeContinuesInterruptedStreamWhenModelOptsIn covers the per-model
// opt-in: with stream_continuation = true the broken turn keeps the partial the
// consumer already saw and re-issues it as a trailing assistant prefix, so the
// committed answer is prefix + continuation and nothing is retracted.
func TestRuntimeContinuesInterruptedStreamWhenModelOptsIn(t *testing.T) {
	t.Parallel()

	model := &continuationRuntimeModel{}
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.Models = append(cfg.Models, config.ModelConfig{
				Ref:                config.ModelRef{Provider: ai.ProviderOpenAI, Model: model.ModelID()},
				StreamContinuation: true,
			})
		},
	)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assertEventSequence(t, events)

	assert.NotContains(t, eventTypes(events), EventMessageDiscarded,
		"a continued turn keeps what the consumer rendered")

	snapshot := runtime.Snapshot()
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	require.Len(t, snapshot.Transcript, 2)
	assistant, ok := snapshot.Transcript[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "half the answer and the rest", text.Text,
		"the committed answer is the retained prefix plus the continuation")

	requests := model.requestsSnapshot()
	require.Len(t, requests, 2, "the broken stream is re-issued exactly once")
	last := requests[1].Messages[len(requests[1].Messages)-1]
	prefixMsg, ok := last.(ai.AssistantMessage)
	require.True(t, ok, "the continuation request ends with a trailing assistant message")
	prefixText, ok := prefixMsg.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "half the answer", prefixText.Text)
	assert.Contains(t, requestSystemText(requests[1]), "Continue it",
		"the continuation request carries the continuation instruction")

	require.NoError(t, runtime.Close(t.Context()))
}

// TestRuntimeRegeneratesInterruptedStreamByDefault pins the default policy: a
// model that does not opt in keeps the regenerate-and-discard recovery, so its
// broken turn retracts the partial and commits the re-issue's own answer.
func TestRuntimeRegeneratesInterruptedStreamByDefault(t *testing.T) {
	t.Parallel()

	model := &continuationRuntimeModel{}
	runtime := openTestRuntime(t, model)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assertEventSequence(t, events)

	assert.Contains(t, eventTypes(events), EventMessageDiscarded,
		"the default retracts the partial it replaces")

	snapshot := runtime.Snapshot()
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	require.Len(t, snapshot.Transcript, 2)
	assistant, ok := snapshot.Transcript[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, " and the rest", text.Text,
		"the default commits the re-issue's own answer, not a glued fragment")

	requests := model.requestsSnapshot()
	require.Len(t, requests, 2)
	_, lastIsAssistant := requests[1].Messages[len(requests[1].Messages)-1].(ai.AssistantMessage)
	assert.False(t, lastIsAssistant, "the regenerating re-issue does not carry the partial back")
	assert.NotContains(t, requestSystemText(requests[1]), "Continue it",
		"the regenerating re-issue carries no continuation instruction")

	require.NoError(t, runtime.Close(t.Context()))
}
