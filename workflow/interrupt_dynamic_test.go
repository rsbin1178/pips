package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestStatefulDynamicInterruptResumesWithTargetData(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"dynamic_interrupt_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			wasInterrupted, hasState, state := workflow.GetInterruptState(ctx)

			isTarget, hasData, data := workflow.GetResumeContext(ctx)
			if !wasInterrupted {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf(map[string]any{"question": "name"}),
					workflow.MustValueOf(map[string]any{"phase": "waiting"}),
				)
			}

			if !hasState || state.String() != `{"phase":"waiting"}` {
				return workflow.ActionOutput{}, errors.New("interrupt state was not restored")
			}

			if !isTarget || !hasData {
				return workflow.ActionOutput{}, errors.New("resume target data is unavailable")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": data,
			}}, nil
		},
	}
	definition := dynamicSingleActionDefinition(
		t,
		"dynamic-interrupt",
		"dynamic_interrupt_action",
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

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "dynamic-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "dynamic-run")

	if calls.Load() != 1 {
		t.Fatalf("Action calls = %d, want 1", calls.Load())
	}

	contexts := interrupted.Interruption.Contexts
	if len(contexts) != 1 || contexts[0].ID == "" || contexts[0].Address.NodeID != "work" ||
		contexts[0].Info.String() != `{"question":"name"}` {
		t.Fatalf("dynamic contexts = %#v", contexts)
	}

	newRunner, err := workflow.NewRunner(workflow.WithCheckpointStore(store))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = newRunner.Resume(t.Context(), plan, "dynamic-run", []workflow.ResumeTarget{{
		InterruptID: "unknown",
	}})
	if !errors.Is(err, workflow.ErrRun) || calls.Load() != 1 {
		t.Fatalf("unknown target error = %v, calls = %d", err, calls.Load())
	}

	resumed, err := newRunner.Resume(t.Context(), plan, "dynamic-run", []workflow.ResumeTarget{{
		InterruptID: contexts[0].ID,
		Data:        workflow.MustValueOf("Ada"),
	}})
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf("resumed result = %#v, calls = %d", resumed, calls.Load())
	}

	if got := resumed.Outputs["result"].String(); got != `"Ada"` {
		t.Fatalf("resumed output = %s, want Ada", got)
	}
}

func TestResumeTargetsOneParallelDynamicInterruptAtATime(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var leftCalls, rightCalls atomic.Int32

	interruptingAction := func(key string, calls *atomic.Int32) *fakeAction {
		return &fakeAction{
			spec: actionSpec(
				workflow.ActionKey(key),
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				calls.Add(1)

				isTarget, _, _ := workflow.GetResumeContext(ctx)
				if !isTarget {
					return workflow.ActionOutput{}, workflow.Interrupt(
						ctx,
						workflow.MustValueOf(map[string]any{"branch": key}),
					)
				}

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf(key),
				}}, nil
			},
		}
	}
	left := interruptingAction("left", &leftCalls)
	right := interruptingAction("right", &rightCalls)
	definition := parallelInterruptDefinition(t, stringSchema)

	registry, err := workflow.NewDefaultRegistry(left, right)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "parallel-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	first, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, first, err, "parallel-run")

	if len(first.Interruption.Contexts) != 2 {
		t.Fatalf("dynamic contexts = %#v, want two", first.Interruption.Contexts)
	}

	byNode := map[workflow.NodeID]workflow.InterruptContext{}
	for _, interruption := range first.Interruption.Contexts {
		byNode[interruption.Address.NodeID] = interruption
	}

	second, err := runner.Resume(t.Context(), plan, "parallel-run", []workflow.ResumeTarget{{
		InterruptID: byNode["left"].ID,
	}})
	assertInterrupted(t, second, err, "parallel-run")

	if leftCalls.Load() != 2 || rightCalls.Load() != 1 {
		t.Fatalf("calls after partial resume = left %d, right %d", leftCalls.Load(), rightCalls.Load())
	}

	if len(second.Interruption.Contexts) != 1 ||
		second.Interruption.Contexts[0].ID != byNode["right"].ID {
		t.Fatalf("remaining contexts = %#v", second.Interruption.Contexts)
	}

	third, err := runner.Resume(t.Context(), plan, "parallel-run", []workflow.ResumeTarget{{
		InterruptID: byNode["right"].ID,
	}})
	if err != nil {
		t.Fatalf("Runner.Resume() second target error = %v", err)
	}

	if third.Status != workflow.RunStatusSucceeded || leftCalls.Load() != 2 ||
		rightCalls.Load() != 2 {
		t.Fatalf(
			"final result = %#v, calls = left %d, right %d",
			third,
			leftCalls.Load(),
			rightCalls.Load(),
		)
	}
}

func TestTargetedReinterruptGetsNewStableID(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"reinterrupt_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			wasInterrupted, hasState, state := workflow.GetInterruptState(ctx)
			if !wasInterrupted {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf("first"),
					workflow.MustValueOf(1),
				)
			}

			if !hasState {
				return workflow.ActionOutput{}, errors.New("missing reinterrupt state")
			}

			phase, err := workflow.DecodeValue[int](state)
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			if phase == 1 {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf("second"),
					workflow.MustValueOf(2),
				)
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done"),
			}}, nil
		},
	}
	definition := dynamicSingleActionDefinition(
		t,
		"reinterrupt",
		"reinterrupt_action",
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

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "reinterrupt-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	first, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, first, err, "reinterrupt-run")
	firstID := first.Interruption.Contexts[0].ID

	second, err := runner.Resume(t.Context(), plan, "reinterrupt-run", []workflow.ResumeTarget{{
		InterruptID: firstID,
	}})
	assertInterrupted(t, second, err, "reinterrupt-run")

	secondID := second.Interruption.Contexts[0].ID
	if secondID == firstID || second.Interruption.Contexts[0].Info.String() != `"second"` {
		t.Fatalf("reinterrupt context = %#v, first ID = %q", second.Interruption.Contexts[0], firstID)
	}

	completed, err := runner.Resume(t.Context(), plan, "reinterrupt-run", []workflow.ResumeTarget{{
		InterruptID: secondID,
	}})
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["result"].String() != `"done"` {
		t.Fatalf("reinterrupt completion = %#v, error = %v", completed, err)
	}
}

func TestCompositeInterruptPreservesUntargetedDescendants(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"composite_interrupt_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			wasInterrupted, hasState, state := workflow.GetInterruptState(ctx)
			if !wasInterrupted {
				left := workflow.Interrupt(ctx, workflow.MustValueOf("left"))
				right := workflow.Interrupt(ctx, workflow.MustValueOf("right"))

				return workflow.ActionOutput{}, workflow.CompositeInterrupt(
					ctx,
					workflow.MustValueOf("composite"),
					workflow.MustValueOf(map[string]any{"phase": "waiting"}),
					left,
					right,
				)
			}

			if !hasState || state.String() != `{"phase":"waiting"}` {
				return workflow.ActionOutput{}, errors.New("composite state was not restored")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done"),
			}}, nil
		},
	}
	definition := dynamicSingleActionDefinition(
		t,
		"composite-interrupt",
		"composite_interrupt_action",
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

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "composite-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	first, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, first, err, "composite-run")

	if len(first.Interruption.Contexts) != 2 {
		t.Fatalf("composite contexts = %#v, want two", first.Interruption.Contexts)
	}

	leftID := first.Interruption.Contexts[0].ID
	rightID := first.Interruption.Contexts[1].ID
	second, err := runner.Resume(t.Context(), plan, "composite-run", []workflow.ResumeTarget{{
		InterruptID: leftID,
	}})
	assertInterrupted(t, second, err, "composite-run")

	if len(second.Interruption.Contexts) != 1 ||
		second.Interruption.Contexts[0].ID != rightID || calls.Load() != 2 {
		t.Fatalf("remaining composite contexts = %#v, calls = %d", second.Interruption.Contexts, calls.Load())
	}

	completed, err := runner.Resume(t.Context(), plan, "composite-run", []workflow.ResumeTarget{{
		InterruptID: rightID,
	}})
	if err != nil || completed.Status != workflow.RunStatusSucceeded || calls.Load() != 3 {
		t.Fatalf("composite completion = %#v, error = %v, calls = %d", completed, err, calls.Load())
	}
}

func dynamicSingleActionDefinition(
	t *testing.T,
	id workflow.DefinitionID,
	action workflow.ActionKey,
	outputSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(outputSchema, "work", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, action),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "work"),
			edge("work", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
}

func parallelInterruptDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeParallel,
		Outputs: map[string]workflow.MergeOutputConfig{
			"left":  {Schema: stringSchema, Sources: []string{"left"}},
			"right": {Schema: stringSchema, Sources: []string{"right"}},
		},
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "parallel-interrupt", Revision: "v1",
		Name:   "Parallel Interrupt",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"left":  nodeOutput(stringSchema, "merge", "left"),
			"right": nodeOutput(stringSchema, "merge", "right"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "left", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "left"),
			},
			{
				ID: "right", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "right"),
			},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"left":  nodeBinding("left", "result"),
					"right": nodeBinding("right", "result"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "left"),
			edge("start", workflow.RouteSuccess, "right"),
			edge("left", workflow.RouteSuccess, "merge"),
			edge("right", workflow.RouteSuccess, "merge"),
			edge("merge", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
}
