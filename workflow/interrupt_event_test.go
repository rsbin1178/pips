package workflow_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestInterruptLifecycleEventsDistinguishPauseAndResume(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	var (
		eventsMu sync.Mutex
		events   []workflow.EventType
	)

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "event-run", nil }),
		workflow.WithEventSink(func(event workflow.Event) {
			eventsMu.Lock()

			events = append(events, event.Type())
			eventsMu.Unlock()
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, result, err, "event-run")
	result, err = runner.Resume(t.Context(), plan, "event-run", nil)
	assertInterrupted(t, result, err, "event-run")

	result, err = runner.Resume(t.Context(), plan, "event-run", nil)
	if err != nil || result.Status != workflow.RunStatusSucceeded {
		t.Fatalf("final Resume() result = %#v, error = %v", result, err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()

	if countEvent(events, workflow.EventRunStarted) != 1 ||
		countEvent(events, workflow.EventRunInterrupted) != 2 ||
		countEvent(events, workflow.EventRunResumed) != 2 ||
		countEvent(events, workflow.EventRunCompleted) != 1 ||
		countEvent(events, workflow.EventRunFailed) != 0 ||
		countEvent(events, workflow.EventRunCanceled) != 0 {
		t.Fatalf("static lifecycle events = %#v", events)
	}
}

func TestDynamicInterruptEmitsNodeInterruptedWithoutNodeFailed(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"event_interrupt_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{}, workflow.Interrupt(
				ctx,
				workflow.MustValueOf("waiting"),
			)
		},
	}
	definition := dynamicSingleActionDefinition(
		t,
		"event-interrupt",
		"event_interrupt_action",
		stringSchema,
	)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	var events []workflow.EventType

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(&memoryCheckpointStore{}),
		workflow.WithEventSink(func(event workflow.Event) {
			events = append(events, event.Type())
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if result.Status != workflow.RunStatusInterrupted || err == nil {
		t.Fatalf("Runner.Run() result = %#v, error = %v", result, err)
	}

	if countEvent(events, workflow.EventNodeInterrupted) != 1 ||
		countEvent(events, workflow.EventNodeFailed) != 0 ||
		countEvent(events, workflow.EventRunInterrupted) != 1 {
		t.Fatalf("dynamic interruption events = %#v", events)
	}
}

func countEvent(events []workflow.EventType, eventType workflow.EventType) int {
	count := 0

	for _, current := range events {
		if current == eventType {
			count++
		}
	}

	return count
}
