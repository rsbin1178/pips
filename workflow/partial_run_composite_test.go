package workflow_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestRunPartialTreatsSubWorkflowAsAtomicDataBoundary(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	childAction := &fakeAction{
		spec: actionSpec(
			"partial-child",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("child:" + mustDecodeString(t, input.Values["value"])),
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "partial-child")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	plan := compileWithResolver(t, parent, staticResolver(child), childAction)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	executed, err := runner.RunPartial(t.Context(), plan, "sub", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := executed.Outputs["result"].String(); got != `"child:input"` || calls.Load() != 1 {
		t.Fatalf("executed result = %s, calls = %d", got, calls.Load())
	}

	pinned, err := runner.RunPartial(t.Context(), plan, "sub", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start": {"value": workflow.MustValueOf("input")},
			"sub":   {"result": workflow.MustValueOf("pinned")},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(pin) error = %v", err)
	}

	if got := pinned.Outputs["result"].String(); got != `"pinned"` || calls.Load() != 1 {
		t.Fatalf("pinned result = %s, calls = %d", got, calls.Load())
	}
}

func TestRunPartialTreatsBatchAsAtomicDataBoundary(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)

	var calls atomic.Int32

	mapAction := &fakeAction{
		spec: actionSpec(
			"partial-map",
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
	body := batchBodyDefinition(t, stringSchema, stringSchema, "partial-map", false)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
	plan := compileRoundTrip(t, definition, mapAction)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	executed, err := runner.RunPartial(t.Context(), plan, "batch", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"items": workflow.MustValueOf([]string{"a", "b"})},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := executed.Outputs["results"].String(); got != `["a","b"]` || calls.Load() != 2 {
		t.Fatalf("executed results = %s, calls = %d", got, calls.Load())
	}

	pinned, err := runner.RunPartial(t.Context(), plan, "batch", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start": {"items": workflow.MustValueOf([]string{})},
			"batch": {"results": workflow.MustValueOf([]string{"pinned"})},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(pin) error = %v", err)
	}

	if got := pinned.Outputs["results"].String(); got != `["pinned"]` || calls.Load() != 2 {
		t.Fatalf("pinned results = %s, calls = %d", got, calls.Load())
	}
}

func TestRunPartialTreatsLoopAsAtomicDataBoundary(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	integerArraySchema := mustSchema(t, `{"type":"array","items":{"type":"integer"}}`)

	var calls atomic.Int32

	increment := &fakeAction{
		spec: actionSpec(
			"increment_loop_variable",
			map[string]workflow.PortSchema{"value": integerSchema},
			map[string]workflow.PortSchema{"next": integerSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			value, err := workflow.DecodeValue[int](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"next": workflow.MustValueOf(value + 1),
			}}, nil
		},
	}
	config := workflow.LoopConfig{
		Body: statefulLoopBody(t, integerSchema),
		Mode: workflow.LoopCount,
		Variables: []workflow.LoopVariable{
			{Name: "counter", Schema: integerSchema},
		},
		Outputs: []workflow.LoopOutput{
			{Name: "values", Source: workflow.LoopOutputBody, Port: "value"},
			{Name: "final", Source: workflow.LoopOutputVariable, Port: "counter"},
		},
		MaxIterations: 10,
	}
	definition := loopParentDefinition(
		t,
		"partial-loop",
		map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
		map[string]workflow.Binding{
			"count": workflowInput("count"), "counter": workflowInput("counter"),
		},
		map[string]workflow.OutputBinding{
			"values": nodeOutput(integerArraySchema, "loop", "values"),
			"final":  nodeOutput(integerSchema, "loop", "final"),
		},
		config,
	)
	plan := compileRoundTrip(t, definition, increment)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	executed, err := runner.RunPartial(t.Context(), plan, "loop", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{
			"count": workflow.MustValueOf(5), "counter": workflow.MustValueOf(0),
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	if got := executed.Outputs["final"].String(); got != `2` || calls.Load() != 2 {
		t.Fatalf("executed final = %s, calls = %d", got, calls.Load())
	}

	pinned, err := runner.RunPartial(t.Context(), plan, "loop", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start": {
				"count": workflow.MustValueOf(1), "counter": workflow.MustValueOf(0),
			},
			"loop": {
				"values": workflow.MustValueOf([]int{9}), "final": workflow.MustValueOf(9),
			},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(pin) error = %v", err)
	}

	if got := pinned.Outputs["final"].String(); got != `9` || calls.Load() != 2 {
		t.Fatalf("pinned final = %s, calls = %d", got, calls.Load())
	}
}

func TestResumePartialRestoresNestedBatchChildFrontier(t *testing.T) {
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
		"partial-deep-inner",
		"to_leaf",
		stringSchema,
		integerSchema,
		workflowRef(t, leaf),
	)
	body := nestedInterruptSubDefinition(
		t,
		"partial-deep-body",
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "partial-deep", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.RunPartial(t.Context(), plan, "batch", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"only"}),
		},
	})
	assertPartialInterrupted(t, interrupted, err, "partial-deep")

	if calls.Load() != 0 || len(interrupted.Interruption.BeforeNodes) != 1 {
		t.Fatalf("deep interruption = %#v, calls = %d", interrupted.Interruption, calls.Load())
	}

	completed, err := runner.ResumePartial(t.Context(), plan, "batch", interrupted.RunID, nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 ||
		completed.Outputs["results"].String() != `["only"]` ||
		completed.Nodes["batch"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("deep resume = %#v, error = %v, calls = %d", completed, err, calls.Load())
	}
}
