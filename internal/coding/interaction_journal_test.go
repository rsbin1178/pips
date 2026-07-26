package coding

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractionJournalLifecycle(t *testing.T) {
	t.Parallel()

	session, err := harness.NewSession(harness.NewMemoryStore("session-1"))
	require.NoError(t, err)
	journal, err := newInteractionJournal(session, func() (string, error) {
		return "interaction-1", nil
	})
	require.NoError(t, err)

	interactionID, err := journal.start()
	require.NoError(t, err)
	assert.Equal(t, "interaction-1", interactionID)

	recovery, err := journal.replay()
	require.NoError(t, err)
	assert.Equal(t, "interaction-1", recovery.PendingID)

	usage := TokenUsage{InputTokens: 4, OutputTokens: 2}
	require.NoError(t, journal.complete(interactionID, InteractionSucceeded, usage, 25))

	recovery, err = journal.replay()
	require.NoError(t, err)
	assert.Empty(t, recovery.PendingID)
	assert.Equal(t, interactionID, recovery.LastID)
	assert.Equal(t, InteractionSucceeded, recovery.LastOutcome)
	assert.Equal(t, usage, recovery.LastUsage)
	assert.Equal(t, int64(25), recovery.LastDurationMS)

	path := session.Path()
	require.Len(t, path, 2)
	assert.Equal(t, interactionCustomType, path[0].Custom)
	assert.Equal(t, interactionCustomType, path[1].Custom)
}

func TestReplayInteractionJournalMarksSupersededStartInterrupted(t *testing.T) {
	t.Parallel()

	path := []harness.Entry{
		interactionEntry(t, "entry-1", interactionRecord{
			Event: interactionStartedEvent, InteractionID: "interaction-1",
		}),
		interactionEntry(t, "entry-2", interactionRecord{
			Event: interactionStartedEvent, InteractionID: "interaction-2",
		}),
	}

	recovery, err := replayInteractionJournal(path)
	require.NoError(t, err)
	assert.Equal(t, "interaction-2", recovery.PendingID)
	assert.Equal(t, []string{"interaction-1"}, recovery.InterruptedIDs)
}

func TestReplayInteractionJournalRejectsMalformedLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path []harness.Entry
	}{
		{
			name: "unknown field",
			path: []harness.Entry{{
				Kind: harness.KindCustom, ID: "entry-1", Custom: interactionCustomType,
				Data: ai.JSON(`{"event":"started","interaction_id":"interaction-1","extra":true}`),
			}},
		},
		{
			name: "terminal without start",
			path: []harness.Entry{interactionEntry(t, "entry-1", interactionRecord{
				Event: interactionTerminalEvent, InteractionID: "interaction-1",
				Outcome: InteractionFailed, Usage: &TokenUsage{},
			})},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := replayInteractionJournal(test.path)
			require.ErrorIs(t, err, ErrEventProtocol)
		})
	}
}

func TestInteractionJournalDoesNotReturnIDWhenStartPersistenceFails(t *testing.T) {
	t.Parallel()

	journal, err := newInteractionJournal(failingInteractionStore{}, func() (string, error) {
		return "interaction-1", nil
	})
	require.NoError(t, err)

	interactionID, err := journal.start()
	assert.Empty(t, interactionID)
	require.ErrorIs(t, err, errInteractionAppend)
}

func TestBootstrapStateMatchesLiveDurableState(t *testing.T) {
	t.Parallel()

	session, err := harness.NewSession(harness.NewMemoryStore("session-1"))
	require.NoError(t, err)
	journal, err := newInteractionJournal(session, func() (string, error) {
		return "interaction-1", nil
	})
	require.NoError(t, err)

	interactionID, err := journal.start()
	require.NoError(t, err)

	user := ai.UserText("fix it")
	assistant := ai.AssistantText("done")
	_, err = session.AppendMessage(user, nil)
	require.NoError(t, err)
	_, err = session.AppendMessage(assistant, nil)
	require.NoError(t, err)

	usage := TokenUsage{InputTokens: 8, OutputTokens: 3}
	require.NoError(t, journal.complete(interactionID, InteractionSucceeded, usage, 50))

	liveEvents := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 1}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: user}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: assistant}),
		newTestEvent(EventTurnCompleted, TurnCompleted{Turn: 1, Usage: usage}),
		newTestEvent(EventRunCompleted, RunCompleted{
			Stop: agent.StopEndTurn, Turns: 1, Usage: usage,
		}),
		newInteractionEvent(EventInteractionCompleted, InteractionCompleted{
			Outcome: InteractionSucceeded, Usage: usage, DurationMillis: 50,
		}),
	}

	var live State

	for index, event := range liveEvents {
		event.Sequence = uint64(index + 1)
		live, err = Reduce(live, event)
		require.NoError(t, err)
	}

	bootstrapped, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		Mode: ModeAgent, Path: session.Path(),
	})
	require.NoError(t, err)
	assert.Equal(t, live.Durable(), bootstrapped.State.Durable())
}

func TestBootstrapStateRecoversOnlyDurablePendingInteraction(t *testing.T) {
	t.Parallel()

	session, err := harness.NewSession(harness.NewMemoryStore("session-1"))
	require.NoError(t, err)
	journal, err := newInteractionJournal(session, func() (string, error) {
		return "interaction-1", nil
	})
	require.NoError(t, err)
	_, err = journal.start()
	require.NoError(t, err)
	_, err = session.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "call-1", Name: "shell", Args: ai.JSON(`{}`)},
	), nil)
	require.NoError(t, err)

	resumed, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Path: session.Path(), HasPendingToolCalls: true,
		Mode: ModePlan,
	})
	require.NoError(t, err)
	assert.Equal(t, "interaction-1", resumed.Recovery.PendingID)
	assert.True(t, resumed.State.Interaction.Active)
	assert.True(t, resumed.State.Interaction.Resumed)
	assert.Equal(t, PhasePaused, resumed.State.Phase)

	interrupted, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Path: session.Path(), HasPendingToolCalls: false,
		Mode: ModeAgent,
	})
	require.NoError(t, err)
	assert.Empty(t, interrupted.Recovery.PendingID)
	assert.Contains(t, interrupted.Recovery.InterruptedIDs, "interaction-1")
	assert.False(t, interrupted.State.Interaction.Active)
	require.Len(t, interrupted.State.Diagnostics, 1)
	assert.Equal(t, "interaction_interrupted", interrupted.State.Diagnostics[0].Code)
}

func interactionEntry(t *testing.T, id string, record interactionRecord) harness.Entry {
	t.Helper()

	data, err := json.Marshal(record)
	require.NoError(t, err)

	return harness.Entry{
		Kind: harness.KindCustom, ID: id, Custom: interactionCustomType, Data: data,
	}
}

var errInteractionAppend = errors.New("append rejected")

type failingInteractionStore struct{}

func (failingInteractionStore) Path() []harness.Entry { return nil }

func (failingInteractionStore) AppendCustom(string, ai.JSON) (string, error) {
	return "", errInteractionAppend
}
