package extension_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/agent/harness"
)

func TestRuntimeRetiresGenerationAfterFinalLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := &eventLog{}
	oldLifecycle := &recordingLifecycle{name: "old", events: events}
	newLifecycle := &recordingLifecycle{name: "new", events: events}
	runtime := newRuntime(t)

	oldActivation, err := runtime.Activate(ctx, newExtension(t, "old", extension.Contribution{
		Lifecycle: oldLifecycle,
	}))
	if err != nil {
		t.Fatalf("activate old generation: %v", err)
	}

	pinned, err := runtime.Acquire()
	if err != nil {
		t.Fatalf("acquire old generation: %v", err)
	}

	newActivation, err := runtime.Activate(ctx, newExtension(t, "new", extension.Contribution{
		Lifecycle: newLifecycle,
	}))
	if err != nil {
		t.Fatalf("activate new generation: %v", err)
	}

	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"old.start", "new.start"}) {
		t.Fatalf("events before releasing old leases = %v", got)
	}

	if err := oldActivation.Release(ctx); err != nil {
		t.Fatalf("release activation lease: %v", err)
	}

	if got := oldLifecycle.stops.Load(); got != 0 {
		t.Fatalf("old lifecycle stopped with acquired lease: %d", got)
	}

	if err := pinned.Release(ctx); err != nil {
		t.Fatalf("release acquired lease: %v", err)
	}

	if got := oldLifecycle.stops.Load(); got != 1 {
		t.Fatalf("old lifecycle stop count = %d, want 1", got)
	}

	if err := newActivation.Release(ctx); err != nil {
		t.Fatalf("release new activation: %v", err)
	}

	if got := newLifecycle.stops.Load(); got != 0 {
		t.Fatalf("current lifecycle stopped before shutdown: %d", got)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	want := []string{"old.start", "new.start", "old.stop", "new.stop"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRuntimeActivationFailureKeepsCurrentGeneration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := &eventLog{}
	stableLifecycle := &recordingLifecycle{name: "stable", events: events}
	runtime := newRuntime(t)

	stable, err := runtime.Activate(ctx, newExtension(t, "stable", extension.Contribution{
		Lifecycle: stableLifecycle,
	}))
	if err != nil {
		t.Fatalf("activate stable generation: %v", err)
	}

	stableGeneration := stable.Snapshot().Generation()

	first := &recordingLifecycle{name: "candidate-1", events: events}
	second := &recordingLifecycle{name: "candidate-2", events: events}
	third := &recordingLifecycle{
		name:     "candidate-3",
		events:   events,
		startErr: errors.New("start failed"),
	}

	_, err = runtime.Activate(
		ctx,
		newExtension(t, "candidate-1", extension.Contribution{Lifecycle: first}),
		newExtension(t, "candidate-2", extension.Contribution{Lifecycle: second}),
		newExtension(t, "candidate-3", extension.Contribution{Lifecycle: third}),
	)
	if err == nil {
		t.Fatal("activate failing generation returned nil error")
	}

	current, err := runtime.Acquire()
	if err != nil {
		t.Fatalf("acquire current generation: %v", err)
	}

	if got := current.Snapshot().Generation(); got != stableGeneration {
		t.Fatalf("current generation = %d, want %d", got, stableGeneration)
	}

	if got := first.stops.Load(); got != 1 {
		t.Fatalf("first candidate rollback count = %d, want 1", got)
	}

	if got := second.stops.Load(); got != 1 {
		t.Fatalf("second candidate rollback count = %d, want 1", got)
	}

	if got := third.stops.Load(); got != 0 {
		t.Fatalf("failed lifecycle stop count = %d, want 0", got)
	}

	if err := current.Release(ctx); err != nil {
		t.Fatalf("release current: %v", err)
	}

	if err := stable.Release(ctx); err != nil {
		t.Fatalf("release stable: %v", err)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	want := []string{
		"stable.start",
		"candidate-1.start",
		"candidate-2.start",
		"candidate-3.start",
		"candidate-2.stop", "candidate-1.stop",
		"stable.stop",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRuntimeReturnsInstalledActivationWithPriorCleanupError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stopErr := errors.New("stop failed")
	oldLifecycle := &recordingLifecycle{
		name:    "old",
		events:  &eventLog{},
		stopErr: stopErr,
	}
	runtime := newRuntime(t)

	old, err := runtime.Activate(ctx, newExtension(t, "old", extension.Contribution{
		Lifecycle: oldLifecycle,
	}))
	if err != nil {
		t.Fatalf("activate old: %v", err)
	}

	if err := old.Release(ctx); err != nil {
		t.Fatalf("release current old activation: %v", err)
	}

	next, err := runtime.Activate(ctx, newExtension(t, "next", extension.Contribution{}))
	if next == nil || !errors.Is(err, stopErr) {
		t.Fatalf("next activation, error = %#v, %v", next, err)
	}

	if got := next.Snapshot().Descriptors()[0].ID; got != "next" {
		t.Fatalf("installed descriptor = %q, want next", got)
	}

	if err := next.Release(ctx); err != nil {
		t.Fatalf("release next: %v", err)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestRuntimeNegotiatesCapabilities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime := newRuntime(t)

	var prepared atomic.Bool

	required, err := extension.NewDefinition(extension.Descriptor{
		ID:       "requires-missing",
		Version:  "1.0.0",
		Requires: []extension.Capability{"application.missing"},
	}, func(context.Context) (extension.Contribution, error) {
		prepared.Store(true)
		return extension.Contribution{}, nil
	})
	if err != nil {
		t.Fatalf("new required definition: %v", err)
	}

	_, err = runtime.Activate(ctx, required)
	if !errors.Is(err, extension.ErrCapabilityUnavailable) {
		t.Fatalf("activate required capability error = %v", err)
	}

	if prepared.Load() {
		t.Fatal("extension was prepared before required capability passed")
	}

	optional, err := extension.NewDefinition(extension.Descriptor{
		ID:       "optional-missing",
		Version:  "1.0.0",
		Optional: []extension.Capability{"application.optional"},
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{}, nil
	})
	if err != nil {
		t.Fatalf("new optional definition: %v", err)
	}

	activation, err := runtime.Activate(ctx, optional)
	if err != nil {
		t.Fatalf("activate optional capability: %v", err)
	}

	diagnostics := activation.Snapshot().Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Capability != "application.optional" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}

	if err := activation.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestRuntimeRejectsInvalidCompleteGenerationBeforeStart(t *testing.T) {
	t.Parallel()

	lifecycle := &recordingLifecycle{name: "invalid", events: &eventLog{}}
	duplicateTool := func() agent.Tool {
		return agent.NewTool("duplicate", "duplicate", func(context.Context, struct{}) (string, error) {
			return "ok", nil
		})
	}
	runtime := newRuntime(t)

	_, err := runtime.Activate(
		context.Background(),
		newExtension(t, "one", extension.Contribution{
			Tools:     []extension.Tool{{Value: duplicateTool()}},
			Lifecycle: lifecycle,
		}),
		newExtension(t, "two", extension.Contribution{
			Tools: []extension.Tool{{Value: duplicateTool()}},
		}),
	)
	if err == nil {
		t.Fatal("activate duplicate tools returned nil error")
	}

	if got := lifecycle.starts.Load(); got != 0 {
		t.Fatalf("lifecycle start count = %d, want 0", got)
	}
}

func TestSnapshotDefensiveCopiesAndProvenance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tool := agent.NewTool("lookup", "look up a value", func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
	ext := newExtension(t, "resources", extension.Contribution{
		Tools: []extension.Tool{{Value: tool, Risk: catalog.RiskRead, Tags: []string{"safe"}}},
		Skills: []harness.Skill{{
			Name:         "review",
			Description:  "Review a change",
			Content:      "Review carefully.",
			Metadata:     map[string]string{"owner": "core"},
			AllowedTools: []string{"lookup"},
		}},
		Prompts: []harness.PromptTemplate{{Name: "summary", Content: "Summarize $1"}},
		Assets:  []extension.Asset{{Kind: "theme", Name: "default", Data: []byte("blue")}},
	})
	runtime := newRuntime(t)

	activation, err := runtime.Activate(ctx, ext)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	t.Cleanup(func() {
		_ = activation.Release(context.Background())
		_ = runtime.Shutdown(context.Background())
	})

	snapshot := activation.Snapshot()
	descriptors := snapshot.Descriptors()
	descriptors[0].ID = "mutated"
	skills := snapshot.Skills()
	skills[0].Metadata["owner"] = "mutated"
	skills[0].AllowedTools[0] = "mutated"
	assets := snapshot.Assets()
	assets[0].Asset.Data[0] = 'x'

	if got := snapshot.Descriptors()[0].ID; got != "resources" {
		t.Fatalf("descriptor ID after mutation = %q", got)
	}

	gotSkill := snapshot.Skills()[0]
	if gotSkill.Metadata["owner"] != "core" || gotSkill.AllowedTools[0] != "lookup" {
		t.Fatalf("skill after mutation = %#v", gotSkill)
	}

	if got := string(snapshot.Assets()[0].Asset.Data); got != "blue" {
		t.Fatalf("asset data after mutation = %q", got)
	}

	descriptions, err := snapshot.Catalog().Search(
		ctx,
		catalog.AllowAll("test", catalog.RiskPrivileged),
		"lookup",
	)
	if err != nil {
		t.Fatalf("search catalog: %v", err)
	}

	if len(descriptions) != 1 || descriptions[0].Source.Kind != catalog.SourceExtension || descriptions[0].Source.ID != "resources" {
		t.Fatalf("tool provenance = %#v", descriptions)
	}
}

func TestRuntimeResolveUsesExactCallerOrder(t *testing.T) {
	t.Parallel()

	one := newExtension(t, "one", extension.Contribution{})
	two := newExtension(t, "two", extension.Contribution{})

	runtime, err := extension.New(extension.WithExtensions(one, two))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	registered, err := runtime.Resolve("two", "one")
	if err != nil {
		t.Fatalf("registered: %v", err)
	}

	got := []string{registered[0].Descriptor().ID, registered[1].Descriptor().ID}
	if want := []string{"two", "one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("registered IDs = %v, want %v", got, want)
	}

	if empty, err := runtime.Resolve(); err != nil || len(empty) != 0 {
		t.Fatalf("resolve empty = %#v, %v", empty, err)
	}

	if _, err := runtime.Resolve("missing"); !errors.Is(err, extension.ErrNotRegistered) {
		t.Fatalf("missing registered error = %v", err)
	}

	if _, err := runtime.Resolve("one", "one"); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("duplicate registered error = %v", err)
	}
}

func TestRuntimeConcurrentAcquireAndReloadClosesEveryLifecycleOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime := newRuntime(t)
	lifecycles := make([]*recordingLifecycle, 0, 17)
	activations := make([]*extension.Activation, 0, 17)

	firstLifecycle := &recordingLifecycle{name: "generation", events: &eventLog{}}

	first, err := runtime.Activate(ctx, newExtension(t, "generation-0", extension.Contribution{
		Lifecycle: firstLifecycle,
	}))
	if err != nil {
		t.Fatalf("activate initial generation: %v", err)
	}

	lifecycles = append(lifecycles, firstLifecycle)
	activations = append(activations, first)

	start := make(chan struct{})

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start

			for range 200 {
				activation, err := runtime.Acquire()
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}

				if activation.Snapshot().Generation() == 0 {
					t.Error("acquired zero generation")
					return
				}

				if err := activation.Release(ctx); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		})
	}

	close(start)

	for generation := 1; generation <= 16; generation++ {
		lifecycle := &recordingLifecycle{name: "generation", events: &eventLog{}}

		activation, err := runtime.Activate(ctx, newExtension(
			t,
			fmt.Sprintf("generation-%d", generation),
			extension.Contribution{Lifecycle: lifecycle},
		))
		if err != nil {
			t.Fatalf("activate generation %d: %v", generation, err)
		}

		lifecycles = append(lifecycles, lifecycle)

		activations = append(activations, activation)
		if err := activations[generation-1].Release(ctx); err != nil {
			t.Fatalf("release generation %d: %v", generation-1, err)
		}
	}

	workers.Wait()

	if err := activations[len(activations)-1].Release(ctx); err != nil {
		t.Fatalf("release final activation: %v", err)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for generation, lifecycle := range lifecycles {
		if got := lifecycle.starts.Load(); got != 1 {
			t.Errorf("generation %d starts = %d, want 1", generation, got)
		}

		if got := lifecycle.stops.Load(); got != 1 {
			t.Errorf("generation %d stops = %d, want 1", generation, got)
		}
	}
}

func TestZeroSnapshotOptionsReturnNotActive(t *testing.T) {
	t.Parallel()

	var snapshot extension.Snapshot
	if _, err := snapshot.AgentOptions(context.Background(), catalog.Policy{}); !errors.Is(err, extension.ErrNotActive) {
		t.Fatalf("agent options error = %v", err)
	}

	if _, err := snapshot.HarnessOptions(context.Background(), catalog.Policy{}); !errors.Is(err, extension.ErrNotActive) {
		t.Fatalf("harness options error = %v", err)
	}
}

func newRuntime(t *testing.T) *extension.Runtime {
	t.Helper()

	runtime, err := extension.New()
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	return runtime
}

func newExtension(t *testing.T, id string, contribution extension.Contribution) extension.Extension {
	t.Helper()

	ext, err := extension.NewDefinition(extension.Descriptor{
		ID:      id,
		Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return contribution, nil
	})
	if err != nil {
		t.Fatalf("new definition: %v", err)
	}

	return ext
}

type eventLog struct {
	mu     sync.Mutex
	values []string
}

func (l *eventLog) append(value string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.values = append(l.values, value)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.values...)
}

type recordingLifecycle struct {
	name     string
	events   *eventLog
	startErr error
	stopErr  error
	starts   atomic.Int32
	stops    atomic.Int32
}

func (l *recordingLifecycle) Start(context.Context) error {
	l.starts.Add(1)
	l.events.append(l.name + ".start")

	return l.startErr
}

func (l *recordingLifecycle) Stop(context.Context) error {
	l.stops.Add(1)
	l.events.append(l.name + ".stop")

	return l.stopErr
}
