package workflow_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestRunPartialEmitsNodeEventsOnlyForRealInvocations(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	plan := partialRunLinearPlan(t, &firstCalls, &secondCalls)

	var (
		eventsMu sync.Mutex
		events   []workflow.Event
	)

	runner, err := workflow.NewRunner(workflow.WithEventSink(func(event workflow.Event) {
		eventsMu.Lock()
		defer eventsMu.Unlock()

		events = append(events, event)
	}))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start": {"value": workflow.MustValueOf("input")},
			"first": {"value": workflow.MustValueOf("first:input")},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()

	for _, event := range events {
		if event.NodeID != "" && event.NodeID != "second" {
			t.Fatalf("substituted node emitted event: %#v", event)
		}

		if event.PlanFingerprint != result.PlanFingerprint {
			t.Fatalf("event PlanFingerprint = %q, want %q", event.PlanFingerprint, result.PlanFingerprint)
		}
	}

	types := make([]workflow.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type())
	}

	if countEvent(types, workflow.EventNodeReady) != 1 ||
		countEvent(types, workflow.EventNodeStarted) != 1 ||
		countEvent(types, workflow.EventNodeCompleted) != 1 ||
		countEvent(types, workflow.EventRunStarted) != 1 ||
		countEvent(types, workflow.EventRunCompleted) != 1 {
		t.Fatalf("partial run events = %#v", types)
	}
}
