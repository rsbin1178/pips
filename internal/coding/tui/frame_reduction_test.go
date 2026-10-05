package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscriptionModel returns a fullscreen model wired to a subscription bridge the
// tests drive one record at a time.
func subscriptionModel(t *testing.T) *Model {
	t.Helper()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.subscription = &subscriptionBridge{}

	return model
}

// streamTestTextDelta is one parent-Session answer delta, whose text the live
// draft renders.
func streamTestTextDelta(sequence uint64) coding.Event {
	event := streamTestDelta(sequence)
	event.Payload = coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "delta "}

	return event
}

// TestStreamFrameCoalescesStateAdvances pins R2's frame-boundary reduction: a
// burst of deltas advances the parent state and re-projects the frame once per
// 33 ms frame, not once per delta.
func TestStreamFrameCoalescesStateAdvances(t *testing.T) {
	t.Parallel()

	const (
		deltas   = 1500
		perFrame = 5
		frames   = deltas / perFrame
	)

	model := subscriptionModel(t)
	model.frameAdvances = 0
	model.frameRenders = 0
	sequence := uint64(0)

	for frame := 0; frame < frames; frame++ {
		for delta := 0; delta < perFrame; delta++ {
			sequence++
			_, _ = model.Update(subscriptionEventMsg{
				bridge: model.subscription, ok: true,
				record: coding.EventRecord{Event: streamTestDelta(sequence)},
			})
			require.NoError(t, model.streamErr)
		}

		model.Update(renderTickMsg{})
	}

	assert.Equal(t, frames, model.frameAdvances, "one state advance per frame")
	assert.Equal(t, frames, model.frameRenders, "one render per frame")
	assert.Equal(t, deltas, len(model.state.Draft), "every delta is applied exactly once")
}

// TestStreamFrameReusesTheComposedView pins the View memo: the framework calls
// View() after every message, but a delta that only marks the frame dirty cannot
// change the frame, so its composition is reused instead of re-styled.
func TestStreamFrameReusesTheComposedView(t *testing.T) {
	t.Parallel()

	model := subscriptionModel(t)
	settled := model.View().Content
	model.viewComposes = 0

	for sequence := uint64(1); sequence <= 10; sequence++ {
		_, _ = model.Update(subscriptionEventMsg{
			bridge: model.subscription, ok: true,
			record: coding.EventRecord{Event: streamTestTextDelta(sequence)},
		})

		require.Equal(t, settled, model.View().Content, "a deferred delta is not visible yet")
		require.Zero(t, model.viewComposes, "a deferred delta does not recompose the frame")
	}

	// The frame boundary applies them, so the frame must change and recompose.
	model.Update(renderTickMsg{})
	assert.Contains(t, model.View().Content, strings.Repeat("delta ", 10))
	assert.Equal(t, 1, model.viewComposes)
}

// TestStreamFrameKeepsInputResponsive covers D6: queued deltas never delay
// terminal input, and they are not lost while the input is handled.
func TestStreamFrameKeepsInputResponsive(t *testing.T) {
	t.Parallel()

	model := subscriptionModel(t)

	for sequence := uint64(1); sequence <= 10; sequence++ {
		_, _ = model.Update(subscriptionEventMsg{
			bridge: model.subscription, ok: true,
			record: coding.EventRecord{Event: streamTestTextDelta(sequence)},
		})
	}
	require.Len(t, model.pending, 10, "deltas wait for the frame")

	_, _ = model.Update(key("x"))
	assert.Equal(t, "x", model.composer.Value(), "input is handled ahead of the queued deltas")
	assert.Len(t, model.pending, 10, "handling input does not drop the frame's deltas")

	model.Update(renderTickMsg{})
	assert.Equal(t, strings.Repeat("delta ", 10), visibleDraftText(model.state.Draft))
	assert.Empty(t, model.pending)
}

// TestStreamFrameKeepsBridgeErrorsImmediate covers D6/D7: a bridge error is
// applied in its own update, ahead of the deltas that are still waiting for the
// frame boundary.
func TestStreamFrameKeepsBridgeErrorsImmediate(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}

	_, _ = model.Update(streamItemMsg{
		bridge: model.bridge, ok: true,
		item: streamItem{event: streamTestTextDelta(1)},
	})
	require.Len(t, model.pending, 1, "a delta waits for the frame")

	_, _ = model.Update(streamItemMsg{
		bridge: model.bridge, ok: true,
		item: streamItem{err: errors.New("bridge failed")},
	})

	require.Error(t, model.streamErr, "the error surfaces without waiting for a frame")
	assert.Contains(t, model.streamErr.Error(), "bridge failed")
	assert.Empty(t, model.pending, "the frame's deltas are applied before the error")
	assert.Equal(t, strings.Repeat("delta ", 1), visibleDraftText(model.state.Draft))
}

// TestStreamFrameKeepsDurableEventsImmediate covers D6: an approval, prompt or
// commit is applied in the update that delivers it, not at the next frame.
func TestStreamFrameKeepsDurableEventsImmediate(t *testing.T) {
	t.Parallel()

	model := subscriptionModel(t)
	_, _ = model.Update(subscriptionEventMsg{
		bridge: model.subscription, ok: true,
		record: coding.EventRecord{Event: streamTestTextDelta(1)},
	})
	require.Len(t, model.pending, 1)

	_, _ = model.Update(subscriptionEventMsg{
		bridge: model.subscription, ok: true,
		record: coding.EventRecord{Event: coding.Event{
			Schema: coding.EventSchema, Sequence: 2, Time: time.Unix(2, 0).UTC(),
			SessionID:     "session-1",
			InteractionID: "interaction-1", RunID: "run-1",
			Type:    coding.EventMessageCommitted,
			Payload: coding.MessageCommitted{Message: ai.Assistant(ai.Text("committed"))},
		}},
	})

	require.NoError(t, model.streamErr)
	assert.Empty(t, model.pending, "a durable event flushes the frame's deltas")
	assert.Contains(t, model.View().Content, "committed")
}
