package workflow_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestResumePreservesExactBatchSubWorkflowAddress(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"deep_interrupt_action",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	leaf := nestedInterruptLeafDefinition(t, stringSchema, integerSchema)
	inner := nestedInterruptSubDefinition(
		t,
		"deep-inner",
		"to_leaf",
		stringSchema,
		integerSchema,
		workflowRef(t, leaf),
	)
	body := nestedInterruptSubDefinition(
		t,
		"deep-body",
		"to_inner",
		stringSchema,
		integerSchema,
		workflowRef(t, inner),
	)
	definition := batchParentDefinition(
		t,
		itemsSchema,
		itemsSchema,
		workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 10,
		},
		workflow.PortSchema{},
	)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithDefinitionResolver(staticResolver(inner, leaf)),
		workflow.WithInterruptBeforeNodes(
			workflow.NewNodePath("batch", "to_inner", "to_leaf", "work"),
		),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "deep-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"only"}),
	})
	assertInterrupted(t, interrupted, err, "deep-run")

	if calls.Load() != 0 || len(interrupted.Interruption.BeforeNodes) != 1 {
		t.Fatalf("deep interruption = %#v, calls = %d", interrupted.Interruption, calls.Load())
	}

	address := interrupted.Interruption.BeforeNodes[0]
	if address.NodeID != "work" || len(address.Scope) != 3 ||
		address.Scope[0] != (workflow.ScopeFrame{Kind: workflow.ScopeBatchItem, NodeID: "batch", Index: 0}) ||
		address.Scope[1] != (workflow.ScopeFrame{Kind: workflow.ScopeSubWorkflow, NodeID: "to_inner", Index: -1}) ||
		address.Scope[2] != (workflow.ScopeFrame{Kind: workflow.ScopeSubWorkflow, NodeID: "to_leaf", Index: -1}) {
		t.Fatalf("deep interruption address = %#v", address)
	}

	completed, err := runner.Resume(t.Context(), plan, "deep-run", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 ||
		completed.Outputs["results"].String() != `["only"]` {
		t.Fatalf("deep resumed result = %#v, error = %v, calls = %d", completed, err, calls.Load())
	}
}

func TestResumeTargetsParallelBatchItemsWithoutRerunningSiblings(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)

	var (
		callsMu sync.Mutex
		calls   = map[int]int{}
	)

	action := &fakeAction{
		spec: actionSpec(
			"interrupt_batch_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			index, err := workflow.DecodeValue[int](input.Values["index"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			callsMu.Lock()
			calls[index]++
			callsMu.Unlock()

			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(
					ctx,
					workflow.MustValueOf(map[string]any{"index": index}),
				)
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "interrupt_batch_item", false)
	definition := batchParentDefinition(
		t,
		itemsSchema,
		itemsSchema,
		workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
			MaxConcurrency: 3, ErrorMode: workflow.BatchTerminate, MaxItems: 10,
		},
		workflow.PortSchema{},
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "batch-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	first, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b", "c"}),
	})
	assertInterrupted(t, first, err, "batch-run")

	if len(first.Interruption.Contexts) != 3 {
		t.Fatalf("Batch contexts = %#v, want three", first.Interruption.Contexts)
	}

	byIndex := map[int]workflow.InterruptContext{}

	for _, interruption := range first.Interruption.Contexts {
		if len(interruption.Address.Scope) != 1 ||
			interruption.Address.Scope[0].Kind != workflow.ScopeBatchItem {
			t.Fatalf("Batch address = %#v", interruption.Address)
		}

		index := interruption.Address.Scope[0].Index
		byIndex[index] = interruption
	}

	second, err := runner.Resume(t.Context(), plan, "batch-run", []workflow.ResumeTarget{{
		InterruptID: byIndex[1].ID,
	}})
	assertInterrupted(t, second, err, "batch-run")

	if len(second.Interruption.Contexts) != 2 {
		t.Fatalf("remaining Batch contexts = %#v", second.Interruption.Contexts)
	}

	callsMu.Lock()
	if calls[0] != 1 || calls[1] != 2 || calls[2] != 1 {
		t.Fatalf("Batch calls after partial resume = %#v", calls)
	}
	callsMu.Unlock()

	completed, err := runner.Resume(t.Context(), plan, "batch-run", []workflow.ResumeTarget{
		{InterruptID: byIndex[0].ID},
		{InterruptID: byIndex[2].ID},
	})
	if err != nil {
		t.Fatalf("Runner.Resume() remaining Batch items error = %v", err)
	}

	if completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["results"].String() != `["a","b","c"]` {
		t.Fatalf("completed Batch result = %#v", completed)
	}

	callsMu.Lock()
	defer callsMu.Unlock()

	if calls[0] != 2 || calls[1] != 2 || calls[2] != 2 {
		t.Fatalf("final Batch calls = %#v", calls)
	}
}

func TestResumeLoopContinuesExactInterruptedIteration(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	var (
		callsMu sync.Mutex
		calls   = map[int]int{}
	)

	action := &fakeAction{
		spec: actionSpec(
			"interrupt_loop_iteration",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			index, err := workflow.DecodeValue[int](input.Values["index"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			callsMu.Lock()
			calls[index]++
			callsMu.Unlock()

			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(
					ctx,
					workflow.MustValueOf(map[string]any{"iteration": index}),
				)
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}
	body := singleActionLoopBody(
		t,
		integerSchema,
		"interrupt_loop_iteration",
		"work",
	)
	definition := loopParentDefinition(
		t,
		"interrupt-loop-resume",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 10},
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "loop-run", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(3),
	})
	for iteration := range 3 {
		assertInterrupted(t, result, err, "loop-run")

		if len(result.Interruption.Contexts) != 1 {
			t.Fatalf("Loop contexts = %#v", result.Interruption.Contexts)
		}

		interruption := result.Interruption.Contexts[0]
		if len(interruption.Address.Scope) != 1 ||
			interruption.Address.Scope[0].Kind != workflow.ScopeLoopIteration ||
			interruption.Address.Scope[0].Index != iteration {
			t.Fatalf("Loop address at %d = %#v", iteration, interruption.Address)
		}

		result, err = runner.Resume(t.Context(), plan, "loop-run", []workflow.ResumeTarget{{
			InterruptID: interruption.ID,
		}})
	}

	if err != nil {
		t.Fatalf("final Loop Resume() error = %v", err)
	}

	if result.Status != workflow.RunStatusSucceeded {
		t.Fatalf("final Loop status = %s, want succeeded", result.Status)
	}

	callsMu.Lock()
	defer callsMu.Unlock()

	if calls[0] != 2 || calls[1] != 2 || calls[2] != 2 {
		t.Fatalf("Loop calls = %#v", calls)
	}
}

func TestResumeLoopPreservesTentativeVariablesAndCompletedBodyNodes(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	var incrementCalls atomic.Int32

	increment := &fakeAction{
		spec: actionSpec(
			"checkpoint_increment",
			map[string]workflow.PortSchema{"value": integerSchema},
			map[string]workflow.PortSchema{"next": integerSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			incrementCalls.Add(1)

			value, err := workflow.DecodeValue[int](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"next": workflow.MustValueOf(value + 1),
			}}, nil
		},
	}
	gate := &fakeAction{
		spec: actionSpec(
			"checkpoint_gate",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if isTarget {
				return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
			}

			return workflow.ActionOutput{}, workflow.Interrupt(ctx, input.Values["index"])
		},
	}
	body := checkpointStateLoopBody(t, integerSchema)
	definition := loopParentDefinition(
		t,
		"checkpoint-state-loop",
		map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
		map[string]workflow.Binding{
			"count": workflowInput("count"), "counter": workflowInput("counter"),
		},
		map[string]workflow.OutputBinding{
			"final": nodeOutput(integerSchema, "loop", "final"),
		},
		workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 10,
			Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
			Outputs: []workflow.LoopOutput{{
				Name: "final", Source: workflow.LoopOutputVariable, Port: "counter",
			}},
		},
	)

	registry, err := workflow.NewDefaultRegistry(increment, gate)
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "state-loop", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(2), "counter": workflow.MustValueOf(0),
	})
	assertInterrupted(t, result, err, "state-loop")
	firstID := result.Interruption.Contexts[0].ID

	result, err = runner.Resume(t.Context(), plan, "state-loop", []workflow.ResumeTarget{{
		InterruptID: firstID,
	}})
	assertInterrupted(t, result, err, "state-loop")

	if incrementCalls.Load() != 2 {
		t.Fatalf("increment calls after second iteration pause = %d, want 2", incrementCalls.Load())
	}

	result, err = runner.Resume(t.Context(), plan, "state-loop", []workflow.ResumeTarget{{
		InterruptID: result.Interruption.Contexts[0].ID,
	}})
	if err != nil || result.Status != workflow.RunStatusSucceeded ||
		result.Outputs["final"].String() != `2` || incrementCalls.Load() != 2 {
		t.Fatalf(
			"state Loop result = %#v, error = %v, increment calls = %d",
			result,
			err,
			incrementCalls.Load(),
		)
	}
}

func nestedInterruptLeafDefinition(
	t *testing.T,
	stringSchema workflow.PortSchema,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "deep-leaf", Revision: "v1", Name: "Deep Leaf",
		Inputs: map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "work", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "deep_interrupt_action"),
				Inputs: map[string]workflow.Binding{
					"item": workflowInput("item"), "index": workflowInput("index"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "work"),
			edge("work", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func checkpointStateLoopBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	setConfig := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{{
		Target: "counter", Input: "next",
	}}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "checkpoint-state-body", Revision: "v1",
		Name:    "Checkpoint State Body",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "increment", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "checkpoint_increment"),
				Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLoopVariable, Port: "counter"},
				},
			},
			{
				ID: "set", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, setConfig),
				Inputs: map[string]workflow.Binding{"next": nodeBinding("increment", "next")},
			},
			{
				ID: "gate", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "checkpoint_gate"),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "increment"),
			edge("increment", workflow.RouteSuccess, "set"),
			edge("set", workflow.RouteSuccess, "gate"),
			edge("gate", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func nestedInterruptSubDefinition(
	t *testing.T,
	id workflow.DefinitionID,
	nodeID workflow.NodeID,
	stringSchema workflow.PortSchema,
	integerSchema workflow.PortSchema,
	reference workflow.DefinitionRef,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs: map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, nodeID, "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: nodeID, Type: workflow.NodeTypeSubWorkflow,
				Version: workflow.BuiltinNodeVersion,
				Config:  mustJSON(t, workflow.SubWorkflowConfig{Workflow: reference}),
				Inputs: map[string]workflow.Binding{
					"item": workflowInput("item"), "index": workflowInput("index"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, nodeID),
			edge(nodeID, workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}
