package workflow_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

type memoryCheckpointStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (s *memoryCheckpointStore) Get(
	_ context.Context,
	key string,
) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value, ok := s.values[key]

	return slices.Clone(value), ok, nil
}

func (s *memoryCheckpointStore) Set(
	_ context.Context,
	key string,
	value []byte,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.values == nil {
		s.values = map[string][]byte{}
	}

	s.values[key] = slices.Clone(value)

	return nil
}

func TestStaticInterruptBeforeAndAfterResumeWithoutRerun(t *testing.T) {
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

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "static-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	before, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, before, err, "static-run")

	if calls.Load() != 0 {
		t.Fatalf("Action calls before resume = %d, want 0", calls.Load())
	}

	if got := before.Interruption.BeforeNodes; len(got) != 1 || got[0].NodeID != "work" {
		t.Fatalf("before interruption = %#v, want work", got)
	}

	after, err := runner.Resume(t.Context(), plan, "static-run", nil)
	assertInterrupted(t, after, err, "static-run")

	if calls.Load() != 1 {
		t.Fatalf("Action calls after first resume = %d, want 1", calls.Load())
	}

	if got := after.Interruption.AfterNodes; len(got) != 1 || got[0].NodeID != "work" {
		t.Fatalf("after interruption = %#v, want work", got)
	}

	completed, err := runner.Resume(t.Context(), plan, "static-run", []workflow.ResumeTarget{})
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if completed.Status != workflow.RunStatusSucceeded {
		t.Fatalf("RunResult.Status = %s, want succeeded", completed.Status)
	}

	if calls.Load() != 1 {
		t.Fatalf("Action calls after second resume = %d, want 1", calls.Load())
	}

	if completed.RunID != before.RunID || !completed.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("Run identity changed: before=%#v completed=%#v", before, completed)
	}
}

func TestStaticInterruptRequiresCheckpointStore(t *testing.T) {
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

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun only", err)
	}

	if result.Status != workflow.RunStatusFailed {
		t.Fatalf("RunResult.Status = %s, want failed", result.Status)
	}

	if calls.Load() != 0 {
		t.Fatalf("Action calls = %d, want 0", calls.Load())
	}
}

func TestStaticInterruptResumesNestedSubWorkflow(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"nested_interrupt_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "nested_interrupt_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		parent,
		registry,
		workflow.WithDefinitionResolver(staticResolver(child)),
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("sub", "child_action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "nested-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	assertInterrupted(t, result, err, "nested-run")

	if calls.Load() != 0 {
		t.Fatalf("Action calls before resume = %d, want 0", calls.Load())
	}

	address := result.Interruption.BeforeNodes[0]
	if address.NodeID != "child_action" || len(address.Scope) != 1 ||
		address.Scope[0].Kind != workflow.ScopeSubWorkflow ||
		address.Scope[0].NodeID != "sub" {
		t.Fatalf("nested address = %#v", address)
	}

	resumed, err := runner.Resume(t.Context(), plan, "nested-run", nil)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed result = %#v, calls = %d", resumed, calls.Load())
	}

	if got := resumed.Outputs["result"].String(); got != `"input"` {
		t.Fatalf("resumed output = %s, want input", got)
	}
}

func TestStaticInterruptBeforeBlocksWholeReadyLaunchSet(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var leftCalls, rightCalls atomic.Int32

	action := func(key workflow.ActionKey, calls *atomic.Int32) *fakeAction {
		return &fakeAction{
			spec: actionSpec(
				key,
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
				calls.Add(1)

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf(string(key)),
				}}, nil
			},
		}
	}
	definition := parallelInterruptDefinition(t, stringSchema)

	registry, err := workflow.NewDefaultRegistry(
		action("left", &leftCalls),
		action("right", &rightCalls),
	)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("left")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "ready-set", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, result, err, "ready-set")

	if leftCalls.Load() != 0 || rightCalls.Load() != 0 {
		t.Fatalf("ready-set calls before resume = left %d, right %d", leftCalls.Load(), rightCalls.Load())
	}

	result, err = runner.Resume(t.Context(), plan, "ready-set", nil)
	if err != nil || result.Status != workflow.RunStatusSucceeded ||
		leftCalls.Load() != 1 || rightCalls.Load() != 1 {
		t.Fatalf(
			"ready-set resumed result = %#v, error = %v, calls = left %d right %d",
			result,
			err,
			leftCalls.Load(),
			rightCalls.Load(),
		)
	}
}

func assertInterrupted(
	t *testing.T,
	result workflow.RunResult,
	err error,
	runID string,
) {
	t.Helper()

	if !errors.Is(err, workflow.ErrInterrupted) || errors.Is(err, workflow.ErrRun) {
		t.Fatalf("run error = %v, want ErrInterrupted only", err)
	}

	var interruptError *workflow.InterruptError
	if !errors.As(err, &interruptError) {
		t.Fatalf("run error type = %T, want *workflow.InterruptError", err)
	}

	if result.Status != workflow.RunStatusInterrupted || result.RunID != runID ||
		result.Interruption == nil || !result.EndedAt.IsZero() {
		t.Fatalf("interrupted result = %#v", result)
	}
}

func staticInterruptFixture(
	t *testing.T,
	calls *atomic.Int32,
) (workflow.Definition, *workflow.Registry) {
	t.Helper()

	action := &fakeAction{
		spec: actionSpec(
			"static_interrupt_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{},
		),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "static-interrupt", Revision: "v1",
		Name:   "Static Interrupt",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "static_interrupt_action"),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "work"),
			edge("work", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}

	return definition, registry
}
