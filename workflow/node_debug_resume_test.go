package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestResumeNodeDebugStaticBeforePreservesRecord(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("debug-resume", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"result": stringSchema,
		}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done"),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		singleActionDefinition(t, "debug-resume", stringSchema, workflow.NodePolicy{}),
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-resume-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("DebugNode() error = %v, want ErrInterrupted", err)
	}

	if interrupted.Status != workflow.RunStatusInterrupted ||
		interrupted.Execution.NodeRun.Status != workflow.NodeStatusReady ||
		calls.Load() != 0 {
		t.Fatalf("interrupted result = %#v, calls=%d", interrupted, calls.Load())
	}

	resumed, err := runner.ResumeNodeDebug(
		t.Context(), debugPlan, interrupted.RunID, nil,
	)
	if err != nil {
		t.Fatalf("ResumeNodeDebug() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded ||
		resumed.Execution.NodeRun.Status != workflow.NodeStatusSucceeded ||
		resumed.Execution.Outputs["result"].String() != `"done"` ||
		calls.Load() != 1 {
		t.Fatalf("resumed result = %#v, calls=%d", resumed, calls.Load())
	}

	if len(resumed.InnerExecutions) != 0 || resumed.RunID != interrupted.RunID {
		t.Fatalf("resumed identity/records = %#v", resumed)
	}
}

func TestResumeNodeDebugRejectsDifferentPlanBeforeInvocation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("debug-mismatch", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"result": stringSchema,
		}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done"),
			}}, nil
		},
	}
	registry, _ := workflow.NewDefaultRegistry(action)
	definition := singleActionDefinition(t, "debug-mismatch", stringSchema, workflow.NodePolicy{})

	plan, err := workflow.Compile(
		t.Context(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	store := &memoryCheckpointStore{}
	runner, _ := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-mismatch-run", nil }),
	)

	interrupted, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("DebugNode() error = %v", err)
	}

	definition.Revision = "v2"

	otherPlan, err := workflow.Compile(
		t.Context(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile(other) error = %v", err)
	}

	otherDebugPlan, _ := workflow.PrepareNodeDebug(otherPlan, workflow.NewNodePath("action"))

	_, err = runner.ResumeNodeDebug(t.Context(), otherDebugPlan, interrupted.RunID, nil)
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("ResumeNodeDebug(other plan) error = %v, want ErrRun", err)
	}

	if calls.Load() != 0 {
		t.Fatalf("mismatched resume invoked Action %d times", calls.Load())
	}
}

func TestResumeNodeDebugParallelBatchPreservesIndexedRecords(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	body := batchBodyDefinition(t, stringSchema, stringSchema, "debug-batch-resume", false)
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10, MaxConcurrency: 3,
	}, workflow.PortSchema{})

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("debug-batch-resume", map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		}, map[string]workflow.PortSchema{"result": stringSchema}),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	registry, _ := workflow.NewDefaultRegistry(action)

	plan, err := workflow.Compile(
		t.Context(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("batch", "map")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("batch"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	store := &memoryCheckpointStore{}
	runner, _ := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-batch-resume-run", nil }),
	)

	interrupted, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b", "c"}),
	})
	if !errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("DebugNode() error = %v, want ErrInterrupted", err)
	}

	if len(interrupted.InnerExecutions) != 3 || calls.Load() != 0 {
		t.Fatalf("interrupted inner=%#v calls=%d", interrupted.InnerExecutions, calls.Load())
	}

	for index, record := range interrupted.InnerExecutions {
		if record.NodeRun.Status != workflow.NodeStatusReady ||
			record.Address.Scope[0].Index != index {
			t.Fatalf("interrupted record[%d] = %#v", index, record)
		}
	}

	resumed, err := runner.ResumeNodeDebug(t.Context(), debugPlan, interrupted.RunID, nil)
	if err != nil {
		t.Fatalf("ResumeNodeDebug() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || len(resumed.InnerExecutions) != 3 ||
		calls.Load() != 3 {
		t.Fatalf("resumed=%#v calls=%d", resumed, calls.Load())
	}

	for index, record := range resumed.InnerExecutions {
		if record.NodeRun.Status != workflow.NodeStatusSucceeded ||
			record.Address.Scope[0].Index != index {
			t.Fatalf("resumed record[%d] = %#v", index, record)
		}
	}
}

func TestResumeNodeDebugDynamicTargetRestoresStateAndData(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("debug-dynamic", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{
			"result": stringSchema,
		}),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			wasInterrupted, hasState, state := workflow.GetInterruptState(ctx)

			isTarget, hasData, data := workflow.GetResumeContext(ctx)
			if !wasInterrupted {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf("question"),
					workflow.MustValueOf("private-state"),
				)
			}

			if !hasState || state.String() != `"private-state"` || !isTarget || !hasData {
				return workflow.ActionOutput{}, errors.New("node debug resume context mismatch")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": data,
			}}, nil
		},
	}
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "debug-dynamic", stringSchema, workflow.NodePolicy{}),
		action,
	)
	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	store := &memoryCheckpointStore{}
	runner, _ := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-dynamic-run", nil }),
	)

	interrupted, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) || interrupted.Interruption == nil ||
		len(interrupted.Interruption.Contexts) != 1 {
		t.Fatalf("DebugNode() result=%#v error=%v", interrupted, err)
	}

	interruptID := interrupted.Interruption.Contexts[0].ID

	resumed, err := runner.ResumeNodeDebug(t.Context(), debugPlan, interrupted.RunID, []workflow.ResumeTarget{{
		InterruptID: interruptID,
		Data:        workflow.MustValueOf("answer"),
	}})
	if err != nil {
		t.Fatalf("ResumeNodeDebug() error = %v", err)
	}

	if resumed.Execution.Outputs["result"].String() != `"answer"` || calls.Load() != 2 {
		t.Fatalf("ResumeNodeDebug() result=%#v calls=%d", resumed, calls.Load())
	}
}

func TestNodeDebugCheckpointRequiresPrivateDebugSection(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	action := &fakeAction{
		spec: actionSpec("debug-checkpoint-section", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{}),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "debug-checkpoint-section", Revision: "v1", Name: "Debug Checkpoint Section",
		Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"debug-checkpoint-section","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "work"),
			edge("work", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	registry, _ := workflow.NewDefaultRegistry(action)

	plan, err := workflow.Compile(
		t.Context(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	debugPlan, _ := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("work"))
	store := &memoryCheckpointStore{}
	runner, _ := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "debug-section-run", nil }),
	)

	paused, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("DebugNode() error = %v", err)
	}

	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal(store.value(paused.RunID), &checkpoint); err != nil {
		t.Fatalf("json.Unmarshal(checkpoint) error = %v", err)
	}

	delete(checkpoint, "node_debug")

	corrupt, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("json.Marshal(checkpoint) error = %v", err)
	}

	store.replace(paused.RunID, corrupt)

	_, err = runner.ResumeNodeDebug(t.Context(), debugPlan, paused.RunID, nil)
	if !errors.Is(err, workflow.ErrRun) || calls.Load() != 0 {
		t.Fatalf("ResumeNodeDebug() error=%v calls=%d", err, calls.Load())
	}
}
