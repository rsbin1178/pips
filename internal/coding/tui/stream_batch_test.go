//nolint:wsl_v5 // Streaming contract tests keep frame setup and its assertion adjacent.
package tui

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runtimeStateController stands in for a Controller backed by a live Runtime:
// its Snapshot is the authority for the parent projection.
type runtimeStateController struct {
	stubController
}

func (c runtimeStateController) Snapshot() coding.State { return c.state.Clone() }

// streamTestDelta builds one parent-Session reasoning delta.
func streamTestDelta(sequence uint64) coding.Event {
	return coding.Event{
		Schema: coding.EventSchema, Sequence: sequence, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
		Type: coding.EventMessageDelta,
		Payload: coding.MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: "delta ",
		},
	}
}

// TestSubscribedFrameAdoptsRuntimeState pins R2's single-reduction rule: when
// the subscription is backed by a Runtime that already reduced the events, the
// TUI must take the projection from the Runtime instead of running the
// transition table again.
func TestSubscribedFrameAdoptsRuntimeState(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 8
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	state.Draft = []coding.MessageDelta{{Kind: ai.StreamReasoningDelta, Text: "from runtime "}}

	controller := runtimeStateController{stubController: stubController{state: state}}

	// An event the local transition table rejects: no run named "run-missing"
	// is active. Adopting the Runtime state is the only path that succeeds.
	rejected := coding.Event{
		Schema: coding.EventSchema, Sequence: 8, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-missing",
		Type: coding.EventMessageDelta,
		Payload: coding.MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: "not applied locally ",
		},
	}

	for _, test := range []struct {
		name         string
		runtimeState bool
		wantErr      bool
	}{
		{name: "runtime-backed subscription adopts the snapshot", runtimeState: true},
		{name: "local fallback keeps the transition table", runtimeState: false, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModelWithController(t, controller, true)
			model.subscription = &subscriptionBridge{}
			model.subscriptionMode = true
			model.runtimeState = test.runtimeState
			model.observedSequence = rejected.Sequence - 1

			_, _ = model.Update(subscriptionEventMsg{
				bridge: model.subscription, ok: true,
				record: coding.EventRecord{Event: rejected},
			})
			// The delta waits for the frame boundary, so the frame is what
			// decides whether the Runtime state or the transition table wins.
			_, _ = model.Update(renderTickMsg{})

			if test.wantErr {
				require.ErrorIs(t, model.streamErr, coding.ErrEventProtocol)

				return
			}

			require.NoError(t, model.streamErr)
			assert.Equal(t, uint64(8), model.state.Sequence, "the snapshot is the projection")
			require.NotEmpty(t, model.state.Draft)
			assert.Equal(t, "from runtime ", model.state.Draft[0].Text)
		})
	}
}

// TestSubscribedRuntimeFramesTrackTheRuntimeState drives the production
// subscription wiring against a real Runtime: every drained frame must leave the
// TUI at the Runtime's committed state even though the TUI never runs the
// transition table for those events.
func TestSubscribedRuntimeFramesTrackTheRuntimeState(t *testing.T) {
	t.Parallel()

	controller := openScriptedController(t)
	model := readyModelWithController(t, controller, true)

	// Subscribe exactly the way the startup path does: the generation counter
	// must match the model's current subscription sequence.
	startCommand := model.startSubscription()
	started, ok := startCommand().(subscriptionStartedMsg)
	require.True(t, ok)
	require.NoError(t, started.err)
	require.True(t, started.supported, "the scripted Runtime exposes event observation")
	require.NotNil(t, started.bridge)

	_, _ = model.Update(started)
	require.True(t, model.runtimeState, "a Runtime-backed subscription owns the parent projection")
	require.Same(t, started.bridge, model.subscription)

	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)

		for range controller.Prompt(t.Context(), ai.UserText("read the fixture")) {
		}
	}()

	<-promptDone

	frames := 0
	for {
		select {
		case record, ok := <-started.bridge.subscription.Events():
			if !ok {
				t.Fatal("subscription closed before the scripted interaction was delivered")
			}

			_, _ = model.Update(subscriptionEventMsg{
				bridge: started.bridge, ok: true, record: record,
			})
			frames++

			continue
		default:
		}

		break
	}

	require.NoError(t, model.streamErr)
	require.Positive(t, frames, "the subscription must deliver frames")

	// The delivered deltas are applied at the frame boundary.
	_, _ = model.Update(renderTickMsg{})

	snapshot := controller.Snapshot()
	assert.Equal(t, snapshot.Sequence, model.state.Sequence)
	assert.Equal(t, snapshot.Durable(), model.state.Durable())
	assert.Equal(t, snapshot.Tools, model.state.Tools)
	require.Len(t, model.state.Tools, 1)
	assert.Equal(t, coding.PhaseIdle, model.state.Phase)
	assert.False(t, model.state.Interaction.Active)
}

// TestSubscribedFrameDetectsSequenceGap pins D8's sequence-check
// responsibility: reading the projection from the Runtime must not drop the
// per-frame continuity check.
func TestSubscribedFrameDetectsSequenceGap(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 10

	model := readyModelWithController(t, runtimeStateController{
		stubController: stubController{state: state},
	}, true)
	model.subscription = &subscriptionBridge{}
	model.subscriptionMode = true
	model.runtimeState = true
	model.observedSequence = 7 // event 8 was never delivered

	_, _ = model.Update(subscriptionEventMsg{
		bridge: model.subscription, ok: true,
		record: coding.EventRecord{Event: coding.Event{
			Schema: coding.EventSchema, Sequence: 9, Time: time.Unix(1, 0).UTC(),
			SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
			Type: coding.EventMessageDelta,
			Payload: coding.MessageDelta{
				Kind: ai.StreamTextDelta, Text: "after the gap",
			},
		}},
	})
	// D8's continuity check runs when the frame applies the delivered deltas.
	_, _ = model.Update(renderTickMsg{})

	require.Error(t, model.streamErr)
	assert.Contains(t, model.streamErr.Error(), "event sequence 9 follows 7")
}

// TestStreamBatchIsBoundedAndKeepsInputResponsive covers the fairness rule: one
// update consumes at most one frame's worth of queued deltas, so terminal input
// is handled in the next update instead of behind an unbounded backlog.
func TestStreamBatchIsBoundedAndKeepsInputResponsive(t *testing.T) {
	t.Parallel()

	_, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	model := readyModel(t, true)
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	model.bridge = &eventBridge{cancel: cancel, items: make(chan streamItem, 4*streamBatchMax)}

	const queued = 3 * streamBatchMax
	first := streamItem{event: streamTestDelta(model.state.Sequence + 1)}
	for index := 2; index <= queued; index++ {
		model.bridge.items <- streamItem{event: streamTestDelta(model.state.Sequence + uint64(index))}
	}

	_, _ = model.Update(streamItemMsg{bridge: model.bridge, item: first, ok: true})
	require.NoError(t, model.streamErr)

	drained := queued - 1 - len(model.bridge.items)
	assert.LessOrEqual(t, drained, streamBatchMax-1, "a frame must not drain more than its bound")
	assert.GreaterOrEqual(t, len(model.bridge.items), queued-1-(streamBatchMax-1),
		"queued deltas must remain for later frames")

	// The very next update handles terminal input; it does not wait for the
	// stream backlog to empty.
	_, _ = model.Update(key("x"))
	assert.Equal(t, "x", model.composer.Value())
}
