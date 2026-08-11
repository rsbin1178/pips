package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestResumeRejectsCorruptForeignAndMismatchedCheckpointsBeforeInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(t *testing.T, data []byte) []byte
	}{
		{
			name: "malformed",
			mutate: func(*testing.T, []byte) []byte {
				return []byte(`{"version":`)
			},
		},
		{
			name: "unknown field",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()

				return mutateCheckpointJSON(t, data, func(value map[string]any) {
					value["unexpected"] = true
				})
			},
		},
		{
			name: "foreign version",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()

				return mutateCheckpointJSON(t, data, func(value map[string]any) {
					value["version"] = 999
				})
			},
		},
		{
			name: "impossible running node",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()

				return mutateCheckpointJSON(t, data, func(value map[string]any) {
					execution, ok := value["execution"].(map[string]any)
					if !ok {
						t.Fatal("checkpoint execution is not an object")
					}

					nodes, ok := execution["nodes"].([]any)
					if !ok || len(nodes) == 0 {
						t.Fatal("checkpoint nodes are not a non-empty array")
					}

					node, ok := nodes[0].(map[string]any)
					if !ok {
						t.Fatal("checkpoint node is not an object")
					}

					node["status"] = "running"
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32

			definition, registry := staticInterruptFixture(t, &calls)

			plan, err := workflow.Compile(
				t.Context(),
				definition,
				registry,
				workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
			)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}

			store := &memoryCheckpointStore{}

			runner, err := workflow.NewRunner(
				workflow.WithCheckpointStore(store),
				workflow.WithRunIDSource(func(time.Time) (string, error) { return "bad-checkpoint", nil }),
			)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			assertInterrupted(t, result, err, "bad-checkpoint")
			store.replace("bad-checkpoint", test.mutate(t, store.value("bad-checkpoint")))

			_, err = runner.Resume(t.Context(), plan, "bad-checkpoint", nil)
			if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
				t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
			}

			if calls.Load() != 0 {
				t.Fatalf("Action calls after invalid checkpoint = %d, want 0", calls.Load())
			}
		})
	}
}

func TestResumeRejectsDifferentPlanFingerprint(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	beforePlan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		t.Fatalf("Compile() before error = %v", err)
	}

	afterPlan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		t.Fatalf("Compile() after error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "mismatch-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), beforePlan, map[string]workflow.Value{})
	assertInterrupted(t, result, err, "mismatch-run")

	_, err = runner.Resume(t.Context(), afterPlan, "mismatch-run", nil)
	if !errors.Is(err, workflow.ErrRun) || calls.Load() != 0 {
		t.Fatalf("Runner.Resume() mismatch error = %v, calls = %d", err, calls.Load())
	}
}

func TestCheckpointSaveFailureIsOrdinaryRunFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	storeErr := errors.New("store unavailable")

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(failingCheckpointStore{setErr: storeErr}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) || !errors.Is(err, storeErr) ||
		errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusFailed || result.Interruption != nil || calls.Load() != 0 {
		t.Fatalf("failed result = %#v, calls = %d", result, calls.Load())
	}
}

func TestResumeRejectsMissingCheckpointAndStoreFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	missingRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(&memoryCheckpointStore{}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = missingRunner.Resume(t.Context(), plan, "missing", nil)
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("missing checkpoint error = %v", err)
	}

	getErr := errors.New("read unavailable")

	failingRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(failingCheckpointStore{getErr: getErr}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = failingRunner.Resume(t.Context(), plan, "failed-read", nil)
	if !errors.Is(err, workflow.ErrRun) || !errors.Is(err, getErr) {
		t.Fatalf("checkpoint read error = %v", err)
	}
}

type failingCheckpointStore struct {
	getErr error
	setErr error
}

func (s failingCheckpointStore) Get(
	context.Context,
	string,
) ([]byte, bool, error) {
	return nil, false, s.getErr
}

func (s failingCheckpointStore) Set(context.Context, string, []byte) error {
	return s.setErr
}

func (s *memoryCheckpointStore) value(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.values[key])
}

func (s *memoryCheckpointStore) replace(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.values[key] = slices.Clone(value)
}

func mutateCheckpointJSON(
	t *testing.T,
	data []byte,
	mutate func(map[string]any),
) []byte {
	t.Helper()

	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("json.Unmarshal(checkpoint) error = %v", err)
	}

	mutate(value)

	mutated, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(checkpoint) error = %v", err)
	}

	return mutated
}
