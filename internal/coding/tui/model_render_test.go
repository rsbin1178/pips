package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionCoalescesTranscriptRendering(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	model := fullscreenModel(t, stubController{state: state}, true)
	bridge := &subscriptionBridge{}
	model.subscription = bridge
	model.transcript.resetRenders()

	for sequence := uint64(1); sequence <= 5; sequence++ {
		model.Update(subscriptionEventMsg{bridge: bridge, ok: true, record: coding.EventRecord{Event: coding.Event{
			Schema: coding.EventSchema, Sequence: sequence, Time: time.Now().UTC(),
			SessionID: state.SessionID, InteractionID: "interaction-1", RunID: "run-1",
			Type: coding.EventMessageDelta, Payload: coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "next "},
		}}})
		require.NoError(t, model.streamErr)
	}

	// The deltas only mark the frame dirty: the parent state advances once, at
	// the frame boundary.
	assert.Empty(t, visibleDraftText(model.state.Draft), "deltas wait for the frame")
	assert.Zero(t, model.transcript.renders, "increments only dirty the transcript")
	assert.True(t, model.renderWait)
	model.Update(renderTickMsg{})
	assert.Equal(t, strings.Repeat("next ", 5), visibleDraftText(model.state.Draft))
	assert.Equal(t, 1, model.frameAdvances, "one frame advances the state once")
	assert.Equal(t, 1, model.transcript.renders)
	assert.Contains(t, model.View().Content, strings.TrimSpace(strings.Repeat("next ", 5)))
	model.Update(renderTickMsg{})
	assert.Equal(t, 1, model.transcript.renders, "unchanged ticks do not rebuild the transcript")
	assert.Equal(t, 1, model.frameAdvances, "an idle tick does not advance the state")

	model.Update(subscriptionEventMsg{bridge: bridge, ok: true, record: coding.EventRecord{Event: coding.Event{
		Schema: coding.EventSchema, Sequence: 6, Time: time.Now().UTC(),
		SessionID: state.SessionID, InteractionID: "interaction-1", RunID: "run-1",
		Type: coding.EventMessageCommitted, Payload: coding.MessageCommitted{Message: ai.Assistant(ai.Text("committed immediately"))},
	}}})
	require.NoError(t, model.streamErr)
	assert.Contains(t, model.View().Content, "committed immediately")
}
