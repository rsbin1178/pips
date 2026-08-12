//nolint:wsl_v5 // Protocol regressions keep each transition and assertion adjacent.
package coding

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
