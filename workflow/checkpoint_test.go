package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
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

func TestResumeRestoresNodeErrorDataAndPartialStatus(t *testing.T) {
	t.Parallel()

	var sourceCalls, handlerCalls atomic.Int32

	plan := interruptedFailurePlan(t, &sourceCalls, &handlerCalls)
	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) {
			return "failure-checkpoint", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "failure-checkpoint")

	if sourceCalls.Load() != 1 || handlerCalls.Load() != 0 ||
		interrupted.Nodes["unreliable"].Status != workflow.NodeStatusException {
		t.Fatalf(
			"interrupted result = %#v, calls = %d/%d",
			interrupted,
			sourceCalls.Load(),
			handlerCalls.Load(),
		)
	}

	var checkpoint map[string]any
	if err := json.Unmarshal(store.value("failure-checkpoint"), &checkpoint); err != nil {
		t.Fatalf("json.Unmarshal(checkpoint) error = %v", err)
	}

	if checkpoint["version"] != float64(3) || checkpoint["handled_failure"] != true ||
		checkpoint["contract_fingerprint"] == "" || checkpoint["registry_fingerprint"] != nil {
		t.Fatalf("checkpoint header = %#v", checkpoint)
	}

	resumed, err := runner.Resume(t.Context(), plan, "failure-checkpoint", nil)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusPartialSucceeded ||
		resumed.Outputs["result"].String() != `"checkpoint failure|error"` ||
		sourceCalls.Load() != 1 || handlerCalls.Load() != 1 ||
		resumed.RunID != interrupted.RunID || !resumed.StartedAt.Equal(interrupted.StartedAt) {
		t.Fatalf(
			"resumed result = %#v, calls = %d/%d",
			resumed,
			sourceCalls.Load(),
			handlerCalls.Load(),
		)
	}
}

func TestResumeRejectsCorruptNodeErrorStateBeforeInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(t *testing.T, checkpoint map[string]any)
	}{
		{
			name: "version one",
			mutate: func(_ *testing.T, checkpoint map[string]any) {
				checkpoint["version"] = float64(1)
			},
		},
		{
			name: "missing handled marker",
			mutate: func(_ *testing.T, checkpoint map[string]any) {
				checkpoint["handled_failure"] = false
			},
		},
		{
			name: "mismatched error type",
			mutate: func(t *testing.T, checkpoint map[string]any) {
				t.Helper()

				data := checkpointFailureData(t, checkpoint)

				source, ok := data[1].(map[string]any)
				if !ok {
					t.Fatal("source failure data is not an object")
				}

				source[workflow.NodeErrorTypePort] = string(workflow.FailureTimeout)
			},
		},
		{
			name: "stale failure data",
			mutate: func(t *testing.T, checkpoint map[string]any) {
				t.Helper()

				data := checkpointFailureData(t, checkpoint)
				data[0] = map[string]any{
					workflow.NodeErrorMessagePort: "fabricated",
					workflow.NodeErrorTypePort:    string(workflow.FailureError),
				}
			},
		},
		{
			name: "missing failure port",
			mutate: func(t *testing.T, checkpoint map[string]any) {
				t.Helper()

				data := checkpointFailureData(t, checkpoint)

				source, ok := data[1].(map[string]any)
				if !ok {
					t.Fatal("source failure data is not an object")
				}

				delete(source, workflow.NodeErrorMessagePort)
			},
		},
		{
			name: "fabricated normal output",
			mutate: func(t *testing.T, checkpoint map[string]any) {
				t.Helper()

				execution := checkpointExecution(t, checkpoint)

				outputs, ok := execution["outputs"].([]any)
				if !ok {
					t.Fatal("checkpoint outputs are not an array")
				}

				outputs[1] = map[string]any{"result": "fabricated"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var sourceCalls, handlerCalls atomic.Int32

			plan := interruptedFailurePlan(t, &sourceCalls, &handlerCalls)
			store := &memoryCheckpointStore{}

			runner, err := workflow.NewRunner(
				workflow.WithCheckpointStore(store),
				workflow.WithRunIDSource(func(time.Time) (string, error) {
					return "corrupt-failure", nil
				}),
			)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			assertInterrupted(t, result, err, "corrupt-failure")
			store.replace(
				"corrupt-failure",
				mutateCheckpointJSON(t, store.value("corrupt-failure"), func(checkpoint map[string]any) {
					test.mutate(t, checkpoint)
				}),
			)

			_, err = runner.Resume(t.Context(), plan, "corrupt-failure", nil)
			if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
				t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
			}

			if sourceCalls.Load() != 1 || handlerCalls.Load() != 0 {
				t.Fatalf(
					"calls after invalid checkpoint = %d/%d, want 1/0",
					sourceCalls.Load(),
					handlerCalls.Load(),
				)
			}
		})
	}
}

func TestResumeValidatesDefaultExceptionOutputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := resultAction(
		"checkpoint-default",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.Value{}, errors.New("default failure")
		},
	)
	policy := workflow.NodePolicy{
		Error: workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{
			"result": workflow.MustValueOf("default"),
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		singleActionDefinition(t, "checkpoint-default", stringSchema, policy),
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("end")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) {
			return "default-checkpoint", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "default-checkpoint")

	if calls.Load() != 1 ||
		interrupted.Nodes["action"].Status != workflow.NodeStatusException {
		t.Fatalf("interrupted result = %#v, calls = %d", interrupted, calls.Load())
	}

	original := store.value("default-checkpoint")

	completed, err := runner.Resume(t.Context(), plan, "default-checkpoint", nil)
	if err != nil || completed.Status != workflow.RunStatusPartialSucceeded ||
		completed.Outputs["result"].String() != `"default"` || calls.Load() != 1 {
		t.Fatalf("completed result = %#v, error = %v, calls = %d", completed, err, calls.Load())
	}

	store.replace("default-checkpoint", mutateCheckpointJSON(t, original, func(checkpoint map[string]any) {
		execution := checkpointExecution(t, checkpoint)

		outputs, ok := execution["outputs"].([]any)
		if !ok {
			t.Fatal("checkpoint outputs are not an array")
		}

		outputs[1] = map[string]any{"result": "different but schema-valid"}
	}))

	_, err = runner.Resume(t.Context(), plan, "default-checkpoint", nil)
	if !errors.Is(err, workflow.ErrRun) || calls.Load() != 1 {
		t.Fatalf("corrupt default Resume() error = %v, calls = %d", err, calls.Load())
	}
}

func interruptedFailurePlan(
	t *testing.T,
	sourceCalls *atomic.Int32,
	handlerCalls *atomic.Int32,
) *workflow.Plan {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)
	definition := failureBranchDefinition(t, stringSchema, 1)
	source := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			sourceCalls.Add(1)

			return workflow.Value{}, errors.New("checkpoint failure")
		},
	)
	handler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			handlerCalls.Add(1)

			message, err := workflow.DecodeValue[string](input.Values["message"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			failureType, err := workflow.DecodeValue[string](input.Values["type"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(message + "|" + failureType),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(
		source,
		constantAction("typed_success", "success", stringSchema),
		handler,
	)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("handler")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return plan
}

func checkpointExecution(t *testing.T, checkpoint map[string]any) map[string]any {
	t.Helper()

	execution, ok := checkpoint["execution"].(map[string]any)
	if !ok {
		t.Fatal("checkpoint execution is not an object")
	}

	return execution
}

func checkpointFailureData(t *testing.T, checkpoint map[string]any) []any {
	t.Helper()

	data, ok := checkpointExecution(t, checkpoint)["failure_data"].([]any)
	if !ok {
		t.Fatal("checkpoint failure data is not an array")
	}

	return data
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
