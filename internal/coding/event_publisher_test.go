//nolint:wsl_v5 // Protocol regressions keep each transition and assertion adjacent.
package coding

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func draftPublisherRuntime(t *testing.T) *Runtime {
	t.Helper()

	state := contractState(t)
	records := make([]MessageDelta, streamDraftChunkSize-1)
	for index := range records {
		records[index] = MessageDelta{Kind: ai.StreamTextDelta, Text: "prefix ", Usage: &TokenUsage{OutputTokens: 7}}
	}
	state.Draft = NewStreamDraft(records...)
	writer, err := newEventWriter(state.SessionID, func() time.Time { return eventTestTime })
	require.NoError(t, err)
	writer.sequence = state.Sequence

	childState := state.Clone()
	childState.SessionID = "draft-child"
	childWriter, err := newEventWriter(childState.SessionID, func() time.Time { return eventTestTime })
	require.NoError(t, err)
	childWriter.sequence = childState.Sequence

	runtime := &Runtime{
		state: state, writer: writer,
		children: map[string]*childProjection{childState.SessionID: {
			state: childState, writer: childWriter, interactionID: childState.Interaction.ID,
		}},
	}
	runtime.publisher = newEventPublisher(runtime)
	t.Cleanup(runtime.publisher.close)

	return runtime
}

func TestEventPublisherFailedCommitPreservesDraft(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"parent", "child"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			runtime := draftPublisherRuntime(t)
			observation, err := runtime.ObserveEvents()
			require.NoError(t, err)
			t.Cleanup(observation.Subscription.Close)
			before, writer := observation.State, runtime.writer
			child := runtime.children["draft-child"]
			if target == "child" {
				before, writer = observation.Children["draft-child"], child.writer
			}
			beforeRecords := before.Draft.Materialize()
			event := newTestEvent(EventMessageDelta, reasoningDelta("must not publish"))
			event.Sequence, event.SessionID = before.Sequence+1, before.SessionID
			_, err = Reduce(before, event)
			require.NoError(t, err, "reduction must succeed so the failure reaches writer.commit")

			// A stale writer must reject after candidate reduction, before the
			// candidate's new chunks or summary are installed or delivered.
			writer.sequence++
			writerSequence := writer.sequence
			delivered := 0
			emitter := newEventEmitter(t.Context(), runtime, func(Event, error) bool {
				delivered++
				return true
			}, true)
			runtime.publisher.mu.Lock()
			if target == "child" {
				err = runtime.publisher.publishChildLocked(t.Context(), child, event)
			} else {
				err = runtime.publisher.publishLocked(t.Context(), emitter, writer, event)
			}
			runtime.publisher.mu.Unlock()
			require.ErrorIs(t, err, ErrEventProtocol)
			assert.Equal(t, writerSequence, writer.sequence)
			assert.Zero(t, delivered)
			assert.Equal(t, observation.Cursor, runtime.publisher.hub.currentCursor())

			after, err := runtime.ObserveEvents()
			require.NoError(t, err)
			after.Subscription.Close()
			state := after.State
			if target == "child" {
				state = after.Children["draft-child"]
			}
			requireEqualState(t, before, state, "failed writer commit installed a candidate")
			assert.Equal(t, beforeRecords, state.Draft.Materialize())
			assert.Equal(t, beforeRecords, before.Draft.Materialize(), "failed commit rewrote retained chunks")
			assert.False(t, state.Draft.HasReasoningDelta())
		})
	}
}

func TestStreamDraftConcurrentRuntimeReaders(t *testing.T) {
	t.Parallel()

	runtime := draftPublisherRuntime(t)
	initial, err := runtime.ObserveEvents()
	require.NoError(t, err)
	initial.Subscription.Close()
	baseSequence := initial.State.Sequence
	const baseCount = streamDraftChunkSize - 1
	require.Equal(t, baseCount, initial.State.Draft.Len())
	retained := []State{initial.State, initial.Children["draft-child"]}
	child := runtime.children["draft-child"]
	ctx := t.Context()
	const count = 3*streamDraftChunkSize + 1
	steps := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(steps)
		for index := range count {
			usage := TokenUsage{OutputTokens: index + 1}
			delta := MessageDelta{Kind: ai.StreamReasoningDelta, Text: "next ", Usage: &usage}
			if err := runtime.publisher.emit(ctx, nil, initial.State.Interaction.ID, "run-1", EventMessageDelta, delta); err != nil {
				result <- err
				return
			}
			runtime.publisher.mu.Lock()
			err := runtime.publisher.publishChildGeneratedLocked(ctx, child, eventTestTime, "run-1", EventMessageDelta, delta)
			runtime.publisher.mu.Unlock()
			if err != nil {
				result <- err
				return
			}
			usage.OutputTokens = -1
			select {
			case steps <- struct{}{}:
			case <-ctx.Done():
				result <- ctx.Err()
				return
			}
		}
		result <- nil
	}()

	for range steps {
		snapshot := runtime.Snapshot()
		observation, observeErr := runtime.ObserveEvents()
		require.NoError(t, observeErr)
		observation.Subscription.Close()
		states := []State{snapshot, observation.State, observation.Children["draft-child"]}
		for _, state := range states {
			records := state.Draft.Materialize()
			require.Equal(t, baseCount+state.Sequence-baseSequence, uint64(len(records)))
			for index, record := range records {
				wantUsage := 7
				if index >= baseCount {
					wantUsage = index - baseCount + 1
				}
				require.NotNil(t, record.Usage)
				assert.Equal(t, wantUsage, record.Usage.OutputTokens)
			}
			records[0].Usage.OutputTokens = -2
			assert.Equal(t, 7, state.Draft.Materialize()[0].Usage.OutputTokens)
		}
		retained = append(retained, states...)
	}
	require.NoError(t, <-result)
	assert.Equal(t, baseSequence+count, runtime.Snapshot().Sequence)
	assert.Equal(t, baseSequence+count, child.state.Sequence)
	for _, state := range retained {
		records := state.Draft.Materialize()
		assert.Equal(t, baseCount+state.Sequence-baseSequence, uint64(len(records)))
		assert.Len(t, records, state.Draft.Len())
		assert.Equal(t, 7, records[0].Usage.OutputTokens)
		assert.Equal(t, (state.Sequence-baseSequence)*uint64(len("next ")), uint64(len(state.Draft.ReasoningText())))
	}
}

func TestEventPublisherSerializesConcurrentSequenceCommits(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	const count = 64
	sequences := make([]uint64, count)
	start := runtime.Snapshot().Sequence

	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			emitter := newEventEmitter(t.Context(), runtime, func(event Event, err error) bool {
				if err == nil {
					sequences[index] = event.Sequence
				}

				return true
			}, true)
			err := emitter.emit(
				"", "", EventIntegrationDiagnostic,
				IntegrationDiagnostic{Component: "runtime", Code: "concurrent_commit"},
			)
			if err != nil {
				t.Errorf("publish event: %v", err)

				return
			}
		}()
	}
	wait.Wait()
	slices.Sort(sequences)
	for index, sequence := range sequences {
		assert.Equal(t, start+uint64(index)+1, sequence)
	}
	assert.Equal(t, start+count, runtime.Snapshot().Sequence)
}

func TestEventPublisherRejectedParentEventDoesNotConsumeSequence(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)
	t.Cleanup(observation.Subscription.Close)

	require.NoError(t, runtime.publisher.emit(
		t.Context(), nil, "", "", EventTeamLifecycle,
		TeamLifecycle{TeamID: "team-sequence-parent", State: TeamLifecycleAdmitted},
	))
	require.NoError(t, runtime.publisher.emit(
		t.Context(), nil, "", "", EventTeamLifecycle,
		TeamLifecycle{TeamID: "team-sequence-parent", State: TeamLifecycleCompleted},
	))
	before := runtime.Snapshot().Sequence

	err = runtime.publisher.emit(
		t.Context(), nil, "", "", EventTeamLifecycle,
		TeamLifecycle{TeamID: "team-sequence-parent", State: TeamLifecycleProposed},
	)
	require.ErrorIs(t, err, ErrEventProtocol)
	assert.Equal(t, before, runtime.Snapshot().Sequence)

	require.NoError(t, runtime.publisher.emit(
		t.Context(), nil, "", "", EventIntegrationDiagnostic,
		IntegrationDiagnostic{Component: "runtime", Code: "after_rejection"},
	))
	assert.Equal(t, before+1, runtime.Snapshot().Sequence)

	records := make([]EventRecord, 0, 3)
	for range 3 {
		select {
		case record := <-observation.Subscription.Events():
			records = append(records, record)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for published event")
		}
	}
	require.Len(t, records, 3)
	assert.Equal(t, before+1, records[2].Event.Sequence)
	assert.Equal(t, EventIntegrationDiagnostic, records[2].Event.Type)
	assert.Equal(t, observation.Cursor+3, records[2].Cursor)
}

func TestEventPublisherRejectedChildEventDoesNotConsumeSequence(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.openChildProjection(t.Context(), subagent.Event{
		ChildSessionID: "child-sequence", Time: eventTestTime,
	}))

	publisher := runtime.publisher
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	child := runtime.children["child-sequence"]
	require.NotNil(t, child)
	cursor := publisher.hub.currentCursor()

	require.NoError(t, publisher.publishChildGeneratedLocked(
		t.Context(), child, eventTestTime, "", EventStatusChanged,
		StatusChanged{Phase: PhaseClosing},
	))
	before := child.state.Sequence

	err := publisher.publishChildGeneratedLocked(
		t.Context(), child, eventTestTime, "", EventStatusChanged,
		StatusChanged{Phase: PhaseRunning},
	)
	require.ErrorIs(t, err, ErrEventProtocol)
	assert.Equal(t, before, child.state.Sequence)

	require.NoError(t, publisher.publishChildGeneratedLocked(
		t.Context(), child, eventTestTime, "", EventStatusChanged,
		StatusChanged{Phase: PhaseClosed},
	))
	assert.Equal(t, before+1, child.state.Sequence)
	assert.Equal(t, cursor+2, publisher.hub.currentCursor())
}

func TestRuntimeCloseAfterRejectedBestEffortTeamLifecycle(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	runtime.publishTeamLifecycle(t.Context(), TeamLifecycle{
		TeamID: "team-close-sequence", State: TeamLifecycleAdmitted,
	})
	runtime.publishTeamLifecycle(t.Context(), TeamLifecycle{
		TeamID: "team-close-sequence", State: TeamLifecycleCompleted,
	})
	runtime.publishTeamLifecycle(t.Context(), TeamLifecycle{
		TeamID: "team-close-sequence", State: TeamLifecycleProposed,
	})

	require.NoError(t, runtime.Close(context.Background()))
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
}
