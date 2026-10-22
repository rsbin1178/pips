//nolint:wsl_v5 // The configured window and its two owners stay adjacent.
package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapHonorsConfiguredTranscriptLimit(t *testing.T) {
	t.Parallel()

	store := harness.NewMemoryStore("session-1")
	session, err := harness.NewSession(store)
	require.NoError(t, err)

	for index := 0; index < 1_000; index++ {
		_, err := session.AppendMessage(ai.UserText("message"), nil)
		require.NoError(t, err)
	}

	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "test-model",
		Path: session.Path(), TranscriptLimit: 300, PlanMode: planmode.StateInactive,
	})
	require.NoError(t, err)

	require.Len(t, result.State.Transcript, 300)
	assert.Equal(t, 700, result.State.TranscriptBase())
	assert.Equal(t, 300, result.State.transcriptLimit)
}

func TestBootstrapClampsAnOversizedTranscriptLimit(t *testing.T) {
	t.Parallel()

	store := harness.NewMemoryStore("session-1")
	session, err := harness.NewSession(store)
	require.NoError(t, err)

	for index := 0; index < 10; index++ {
		_, err := session.AppendMessage(ai.UserText("message"), nil)
		require.NoError(t, err)
	}

	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "test-model",
		Path: session.Path(), TranscriptLimit: 100_000, PlanMode: planmode.StateInactive,
	})
	require.NoError(t, err)
	assert.Equal(t, retainedTranscriptItems, result.State.transcriptLimit)
}

func TestBootstrapStateRetainsOnlyTheWindowAndOwnsItsMessages(t *testing.T) {
	t.Parallel()

	path := make([]harness.Entry, 0, 1_000)
	for index := 0; index < 1_000; index++ {
		path = append(path, harness.Entry{
			Kind: harness.KindMessage, ID: fmt.Sprintf("entry-%d", index),
			Message: ai.UserText(fmt.Sprintf("message-%d", index)),
		})
	}

	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "test-model",
		Path: path, TranscriptLimit: 300, PlanMode: planmode.StateInactive,
	})
	require.NoError(t, err)
	require.Len(t, result.State.Transcript, 300)
	assert.Equal(t, 700, result.State.TranscriptBase())
	assert.Equal(t, ai.UserText("message-700"), result.State.Transcript[0])

	// The result owns its retained messages; the caller's Harness path can be
	// replaced without changing the projection.
	path[700].Message = ai.UserText("mutated")
	assert.Equal(t, ai.UserText("message-700"), result.State.Transcript[0])
}

func TestReduceHonorsConfiguredTranscriptLimit(t *testing.T) {
	t.Parallel()

	transcript := make(ai.Messages, 300)
	for index := range transcript {
		transcript[index] = ai.UserText("message")
	}

	state := fixtureState(t, transcript)
	state.transcriptLimit = 300

	next, err := Reduce(state, Event{
		Schema: EventSchema, Sequence: state.Sequence + 1, Time: eventTestTime,
		SessionID: state.SessionID, InteractionID: state.Interaction.ID, RunID: "bench-run",
		Type:    EventMessageCommitted,
		Payload: MessageCommitted{Message: ai.AssistantText("answer")},
	})
	require.NoError(t, err)

	require.Len(t, next.Transcript, 225)
	assert.Equal(t, 76, next.TranscriptBase())
	assert.Equal(t, 300, next.transcriptLimit, "the transition keeps the configured window")
}

func TestCloneKeepsConfiguredTranscriptLimit(t *testing.T) {
	t.Parallel()

	state := fixtureState(t, nil)
	state.transcriptLimit = 512

	cloned := state.Clone()
	assert.Equal(t, 512, cloned.transcriptLimit)
	assert.Equal(t, 512, cloned.CloneReadOnly().transcriptLimit)
}
