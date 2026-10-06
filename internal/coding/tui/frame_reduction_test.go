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

// TestStreamFrameReusesTheComposedViewForDuplicateIteratorDelivery pins the
// confirmed avoidable recomposition: the Runtime publishes one accepted event to
// the subscription hub and to the operation iterator, and in subscription mode
// the iterator's copy is already owned by the subscription. Such a delivery
// renders nothing, so it must not re-style the whole frame.
func TestStreamFrameReusesTheComposedViewForDuplicateIteratorDelivery(t *testing.T) {
	t.Parallel()

	model := subscriptionModel(t)
	model.subscriptionMode = true
	model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}

	_, _ = model.Update(subscriptionEventMsg{
		bridge: model.subscription, ok: true,
		record: coding.EventRecord{Event: streamTestTextDelta(1)},
	})
	settled := model.View().Content
	model.viewComposes = 0

	for duplicate := range 5 {
		_, command := model.Update(streamItemMsg{
			bridge: model.bridge, ok: true,
			item: streamItem{event: streamTestTextDelta(1)},
		})
		require.NotNil(t, command, "the iterator wait continues after a duplicate delivery")
		require.True(t, model.waiting)
		require.Equal(t, settled, model.View().Content, "a duplicate delivery renders nothing new")
		require.Zero(t, model.viewComposes, "duplicate %d did not recompose the frame", duplicate)
	}

	require.Len(t, model.pending, 1, "the duplicate is not applied a second time")

	// The frame boundary still paints what the subscription delivered.
	model.Update(renderTickMsg{})
	assert.Contains(t, model.View().Content, "delta")
	assert.Equal(t, 1, model.viewComposes)
}

// TestStreamFrameReusesComposedViewForDuplicateDeliveryInline covers the same
// delivery under the inline presentation, which owns the mutable tail rather than
// the managed region: the reuse rule belongs to the frame cache, not to one mode.
func TestStreamFrameReusesComposedViewForDuplicateDeliveryInline(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.subscriptionMode = true
	model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}
	require.NotEmpty(t, model.View().Content)
	model.viewComposes = 0

	_, command := model.Update(streamItemMsg{
		bridge: model.bridge, ok: true,
		item: streamItem{event: streamTestTextDelta(1)},
	})
	require.NotNil(t, command)
	require.NotEmpty(t, model.View().Content)
	assert.Zero(t, model.viewComposes, "an invisible delivery does not recompose the inline frame")
}

// TestStreamFrameKeepsPendingInvalidationAheadOfDuplicateDelivery pins the other
// half of the reuse rule: an invalidation that no View call has adopted yet - a
// session transition rerendering the region directly - must be composed before
// any later delivery may reuse the cache.
func TestStreamFrameKeepsPendingInvalidationAheadOfDuplicateDelivery(t *testing.T) {
	t.Parallel()

	model := subscriptionModel(t)
	model.subscriptionMode = true
	model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}

	_, _ = model.Update(subscriptionEventMsg{
		bridge: model.subscription, ok: true,
		record: coding.EventRecord{Event: streamTestTextDelta(1)},
	})
	model.Update(renderTickMsg{})
	require.Contains(t, model.View().Content, "delta")

	// The projection is replaced directly, as the session replacement path does,
	// and the region is rerendered without an Update or a View in between.
	model.state.Transcript = []ai.Message{ai.UserText("replacement transcript")}
	model.rerenderTranscript(false)
	model.viewComposes = 0

	// An invisible duplicate delivery arrives before the framework paints again.
	_, _ = model.Update(streamItemMsg{
		bridge: model.bridge, ok: true,
		item: streamItem{event: streamTestTextDelta(2)},
	})
	assert.Contains(t, model.View().Content, "replacement transcript",
		"a delivery that renders nothing must not resurrect an invalidated composition")
	assert.Equal(t, 1, model.viewComposes, "the unadopted invalidation is composed once")
}

// TestStreamFrameKeepsDuplicateReuseOffErrorEofAndForeignBridges keeps the reuse
// attached to the one delivery it was proven for: a successful event from the
// current operation bridge while the subscription owns the projection.
func TestStreamFrameKeepsDuplicateReuseOffErrorEofAndForeignBridges(t *testing.T) {
	t.Parallel()

	duplicateModel := func(t *testing.T, sequence uint64) *Model {
		t.Helper()

		model := subscriptionModel(t)
		model.subscriptionMode = true
		model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}
		_, _ = model.Update(subscriptionEventMsg{
			bridge: model.subscription, ok: true,
			record: coding.EventRecord{Event: streamTestTextDelta(sequence)},
		})
		require.NotEmpty(t, model.View().Content)

		return model
	}

	t.Run("cold cache composes instead of reusing nothing", func(t *testing.T) {
		t.Parallel()

		model := subscriptionModel(t)
		model.subscriptionMode = true
		model.bridge = &eventBridge{cancel: func() {}, items: make(chan streamItem, 4)}
		model.viewComposes = 0

		_, _ = model.Update(streamItemMsg{
			bridge: model.bridge, ok: true,
			item: streamItem{event: streamTestTextDelta(1)},
		})
		require.NotEmpty(t, model.View().Content)
		assert.Equal(t, 1, model.viewComposes, "the first frame is composed")
	})

	t.Run("bridge error keeps its immediate boundary", func(t *testing.T) {
		t.Parallel()

		model := duplicateModel(t, 1)
		model.viewComposes = 0
		_, _ = model.Update(streamItemMsg{
			bridge: model.bridge, ok: true,
			item: streamItem{err: errors.New("bridge failed")},
		})

		require.Error(t, model.streamErr)
		assert.Contains(t, model.View().Content, "bridge failed")
		assert.Equal(t, 1, model.viewComposes, "an error is not an invisible delivery")
	})

	t.Run("closed stream repaints its final frame", func(t *testing.T) {
		t.Parallel()

		model := duplicateModel(t, 1)
		model.viewComposes = 0
		_, _ = model.Update(streamItemMsg{bridge: model.bridge, ok: false})

		assert.Nil(t, model.bridge)
		assert.NotEmpty(t, model.View().Content)
		assert.Equal(t, 1, model.viewComposes, "the end of the stream repaints the frame")
	})

	t.Run("foreign bridge delivery is not reused", func(t *testing.T) {
		t.Parallel()

		model := duplicateModel(t, 1)
		model.viewComposes = 0
		foreign := &eventBridge{cancel: func() {}, items: make(chan streamItem, 1)}
		_, command := model.Update(streamItemMsg{
			bridge: foreign, ok: true,
			item: streamItem{event: streamTestTextDelta(2)},
		})

		assert.Nil(t, command)
		assert.NotEmpty(t, model.View().Content)
		assert.Equal(t, 1, model.viewComposes, "a replaced bridge's delivery has no reusable frame")
	})
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
