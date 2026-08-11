package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestLoopCountCommitsVariablesAndHonorsContinueAndBreak(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	integerArraySchema := mustSchema(t, `{"type":"array","items":{"type":"integer"}}`)

	increment := &fakeAction{
		spec: actionSpec(
			"increment_loop_variable",
			map[string]workflow.PortSchema{"value": integerSchema},
			map[string]workflow.PortSchema{"next": integerSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			value, err := workflow.DecodeValue[int](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"next": workflow.MustValueOf(value + 1),
			}}, nil
		},
	}

	body := statefulLoopBody(t, integerSchema)
	config := workflow.LoopConfig{
		Body: body,
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
		"count-loop",
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
	definition.Limits = workflow.Limits{MaxConcurrency: 1, MaxSteps: 100}
	plan := compileRoundTrip(t, definition, increment)

	result := runWorkflow(t, plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(5), "counter": workflow.MustValueOf(0),
	})
	if got := result.Outputs["values"].String(); got != `[1,2]` {
		t.Fatalf("values = %s, want [1,2]", got)
	}

	if got := result.Outputs["final"].String(); got != `2` {
		t.Fatalf("final = %s, want 2", got)
	}
}

func TestLoopArrayUsesShortestInputAndLiftedValues(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	stringsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)

	join := &fakeAction{
		spec: actionSpec(
			"join_loop_arrays",
			map[string]workflow.PortSchema{
				"left": stringSchema, "right": stringSchema,
				"prefix": stringSchema, "index": integerSchema,
			},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			left, _ := workflow.DecodeValue[string](input.Values["left"])
			right, _ := workflow.DecodeValue[string](input.Values["right"])
			prefix, _ := workflow.DecodeValue[string](input.Values["prefix"])
			index, _ := workflow.DecodeValue[int](input.Values["index"])

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(fmt.Sprintf("%s:%s:%s:%d", prefix, left, right, index)),
			}}, nil
		},
	}

	body := arrayLoopBody(t, stringSchema, integerSchema)
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopArray, Arrays: []string{"left", "right"},
		Outputs: []workflow.LoopOutput{
			{Name: "results", Source: workflow.LoopOutputBody, Port: "result"},
		},
		MaxIterations: 10,
	}
	definition := loopParentDefinition(
		t,
		"array-loop",
		map[string]workflow.PortSchema{
			"left": stringsSchema, "right": stringsSchema, "prefix": stringSchema,
		},
		map[string]workflow.Binding{
			"left": workflowInput("left"), "right": workflowInput("right"),
			"prefix": workflowInput("prefix"),
		},
		map[string]workflow.OutputBinding{
			"results": nodeOutput(stringsSchema, "loop", "results"),
		},
		config,
	)
	plan := compileRoundTrip(t, definition, join)

	result := runWorkflow(t, plan, map[string]workflow.Value{
		"left":   workflow.MustValueOf([]string{"a", "b", "c"}),
		"right":  workflow.MustValueOf([]string{"x", "y"}),
		"prefix": workflow.MustValueOf("p"),
	})
	if got := result.Outputs["results"].String(); got != `["p:a:x:0","p:b:y:1"]` {
		t.Fatalf("results = %s", got)
	}

	empty := runWorkflow(t, plan, map[string]workflow.Value{
		"left": workflow.MustValueOf([]string{}), "right": workflow.MustValueOf([]string{"x"}),
		"prefix": workflow.MustValueOf("p"),
	})
	if got := empty.Outputs["results"].String(); got != `[]` {
		t.Fatalf("empty results = %s, want []", got)
	}
}

func TestLoopInfiniteStopsAtBreak(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	integerArraySchema := mustSchema(t, `{"type":"array","items":{"type":"integer"}}`)
	body := indexedBreakBody(t, integerSchema, 2)
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopInfinite, MaxIterations: 5,
		Outputs: []workflow.LoopOutput{
			{Name: "indexes", Source: workflow.LoopOutputBody, Port: "index"},
		},
	}
	definition := loopParentDefinition(
		t,
		"infinite-loop",
		map[string]workflow.PortSchema{},
		map[string]workflow.Binding{},
		map[string]workflow.OutputBinding{
			"indexes": nodeOutput(integerArraySchema, "loop", "indexes"),
		},
		config,
	)
	plan := compileRoundTrip(t, definition)

	result := runWorkflow(t, plan, map[string]workflow.Value{})
	if got := result.Outputs["indexes"].String(); got != `[0,1,2]` {
		t.Fatalf("indexes = %s, want [0,1,2]", got)
	}
}

func TestLoopLimitIsClassifiedAndStopsSuccessorIterations(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	body := indexedBreakBody(t, integerSchema, 99)
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopInfinite, MaxIterations: 2,
	}
	definition := loopParentDefinition(
		t,
		"limited-loop",
		map[string]workflow.PortSchema{},
		map[string]workflow.Binding{},
		map[string]workflow.OutputBinding{},
		config,
	)
	plan := compileRoundTrip(t, definition)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) || !strings.Contains(err.Error(), "loop iteration limit") {
		t.Fatalf("Runner.Run() error = %v, want Loop FailureLimit", err)
	}

	if got := result.Nodes["loop"].Failure; got != workflow.FailureLimit {
		t.Fatalf("Loop failure = %q, want %q", got, workflow.FailureLimit)
	}
}

func TestLoopHandledBodyFailureMarksRootPartial(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	resultsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)
	body := failureBranchDefinition(t, stringSchema, 1)
	body.Inputs["index"] = integerSchema
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
		Outputs: []workflow.LoopOutput{
			{Name: "results", Source: workflow.LoopOutputBody, Port: "result"},
		},
	}
	definition := loopParentDefinition(
		t,
		"handled-failure-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{
			"results": nodeOutput(resultsSchema, "loop", "results"),
		},
		config,
	)

	var attempts atomic.Int32

	failing := resultAction(
		"typed_failure",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			attempts.Add(1)

			return workflow.Value{}, errors.New("loop handled")
		},
	)
	handler := &fakeAction{
		spec: actionSpec(
			"typed_handler",
			map[string]workflow.PortSchema{"message": stringSchema, "type": errorTypeSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["message"],
			}}, nil
		},
	}
	plan := compileRoundTrip(
		t,
		definition,
		failing,
		constantAction("typed_success", "success", stringSchema),
		handler,
	)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(2),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded || attempts.Load() != 2 ||
		result.Nodes["loop"].Status != workflow.NodeStatusSucceeded ||
		result.Outputs["results"].String() != `["loop handled","loop handled"]` {
		t.Fatalf("result = %#v, attempts = %d", result, attempts.Load())
	}
}

func TestLoopUnhandledBodyFailureUsesOuterPolicy(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	resultsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	body := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "unhandled-loop-body", Revision: "v1", Name: "Unhandled Loop Body",
		Inputs: map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "fail", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "fail", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "loop_fail")},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "fail"),
			edge("fail", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopCount, MaxIterations: 3,
		Outputs: []workflow.LoopOutput{
			{Name: "results", Source: workflow.LoopOutputBody, Port: "result"},
		},
	}
	definition := loopParentDefinition(
		t,
		"outer-policy-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{
			"results": nodeOutput(resultsSchema, "loop", "results"),
		},
		config,
	)
	definition.Nodes[1].Policy = workflow.NodePolicy{
		Error: workflow.ErrorContinueWithDefault,
		DefaultOutputs: map[string]workflow.Value{
			"results": workflow.MustValueOf([]string{"default"}),
		},
	}

	var calls atomic.Int32

	failing := resultAction(
		"loop_fail",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.Value{}, errors.New("iteration stopped")
		},
	)
	plan := compileRoundTrip(t, definition, failing)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(3),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if result.Status != workflow.RunStatusPartialSucceeded || calls.Load() != 1 ||
		result.Nodes["loop"].Status != workflow.NodeStatusException ||
		result.Outputs["results"].String() != `["default"]` {
		t.Fatalf("result = %#v, calls = %d", result, calls.Load())
	}
}

func TestLoopRejectsContextAndDeterminismViolations(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)

	t.Run("Break outside Loop", func(t *testing.T) {
		t.Parallel()

		definition := workflow.Definition{
			Schema: workflow.SchemaV1Alpha1, ID: "orphan-break", Revision: "v1", Name: "Orphan Break",
			Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.OutputBinding{},
			Nodes: []workflow.NodeDefinition{
				{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
				{ID: "break", Type: workflow.NodeTypeBreak, Version: workflow.BuiltinNodeVersion},
				{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
			},
			Edges: []workflow.ControlEdge{
				edge("start", workflow.RouteSuccess, "break"),
				edge("break", workflow.RouteSuccess, "end"),
			},
			Limits: workflow.DefaultLimits(),
		}

		assertCompileErrorContains(t, definition, "only available in a direct Loop body")
	})

	t.Run("infinite without Break", func(t *testing.T) {
		t.Parallel()

		body := passthroughIndexBody(t, integerSchema)
		config := workflow.LoopConfig{Body: body, Mode: workflow.LoopInfinite, MaxIterations: 2}
		definition := loopParentDefinition(
			t, "unbounded-loop", map[string]workflow.PortSchema{},
			map[string]workflow.Binding{}, map[string]workflow.OutputBinding{}, config,
		)

		assertCompileErrorContains(t, definition, "requires Break")
	})

	t.Run("unordered writes", func(t *testing.T) {
		t.Parallel()

		body := unorderedWritesBody(t, integerSchema)
		config := workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
			Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
		}
		countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
		definition := loopParentDefinition(
			t,
			"unordered-loop",
			map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
			map[string]workflow.Binding{
				"count": workflowInput("count"), "counter": workflowInput("counter"),
			},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "unordered access")
	})
}

func TestLoopRejectsInvalidContractsAndNesting(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	t.Run("unknown config field", func(t *testing.T) {
		t.Parallel()

		_, err := (workflow.LoopNode{}).Compile(
			t.Context(),
			testCompileContext{},
			workflow.NodeDefinition{ID: "loop", Config: []byte(`{"unknown":true}`)},
		)
		if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("LoopNode.Compile() error = %v, want strict config ErrCompile", err)
		}
	})

	t.Run("invalid maximum", func(t *testing.T) {
		t.Parallel()

		config := workflow.LoopConfig{
			Body: passthroughIndexBody(t, integerSchema), Mode: workflow.LoopCount,
			MaxIterations: 1_001,
		}
		definition := loopParentDefinition(
			t,
			"invalid-loop-maximum",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "max iterations")
	})

	t.Run("invalid index schema", func(t *testing.T) {
		t.Parallel()

		config := workflow.LoopConfig{
			Body: passthroughIndexBody(t, stringSchema), Mode: workflow.LoopCount,
			MaxIterations: 2,
		}
		definition := loopParentDefinition(
			t,
			"invalid-loop-index",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "index must accept an integer")
	})

	t.Run("unknown output projection", func(t *testing.T) {
		t.Parallel()

		config := workflow.LoopConfig{
			Body: passthroughIndexBody(t, integerSchema), Mode: workflow.LoopCount,
			MaxIterations: 2,
			Outputs: []workflow.LoopOutput{
				{Name: "missing", Source: workflow.LoopOutputBody, Port: "missing"},
			},
		}
		definition := loopParentDefinition(
			t,
			"invalid-loop-output",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "unknown body output")
	})

	t.Run("assignment schema mismatch", func(t *testing.T) {
		t.Parallel()

		body := setVariableLiteralBody(t, integerSchema, workflow.MustValueOf("wrong"))
		config := workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
			Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
		}
		definition := loopParentDefinition(
			t,
			"invalid-loop-assignment",
			map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
			map[string]workflow.Binding{
				"count": workflowInput("count"), "counter": workflowInput("counter"),
			},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "schema violation")
	})

	t.Run("Loop inside Loop", func(t *testing.T) {
		t.Parallel()

		innerConfig := workflow.LoopConfig{
			Body: passthroughIndexBody(t, integerSchema), Mode: workflow.LoopCount,
			MaxIterations: 2,
		}
		body := nestedLoopBody(t, integerSchema, countSchema, innerConfig)
		outerConfig := workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
		}
		definition := loopParentDefinition(
			t,
			"nested-loop",
			map[string]workflow.PortSchema{"count": countSchema, "inner_count": countSchema},
			map[string]workflow.Binding{
				"count": workflowInput("count"), "inner_count": workflowInput("inner_count"),
			},
			map[string]workflow.OutputBinding{},
			outerConfig,
		)

		assertCompileErrorContains(t, definition, "not allowed inside a Loop body")
	})

	t.Run("Loop inside Batch", func(t *testing.T) {
		t.Parallel()

		itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
		innerConfig := workflow.LoopConfig{
			Body: passthroughIndexBody(t, integerSchema), Mode: workflow.LoopCount,
			MaxIterations: 2,
		}
		body := batchBodyContainingLoop(t, stringSchema, integerSchema, innerConfig)
		config := workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 2,
		}
		definition := batchParentDefinition(
			t, itemsSchema, itemsSchema, config, workflow.PortSchema{},
		)

		assertCompileErrorContains(t, definition, "must not contain Batch or Loop")
	})

	t.Run("Batch inside Loop", func(t *testing.T) {
		t.Parallel()

		itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
		body := loopBodyContainingBatch(t, stringSchema, integerSchema, itemsSchema)
		config := workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
		}
		definition := loopParentDefinition(
			t,
			"loop-containing-batch",
			map[string]workflow.PortSchema{"count": countSchema, "items": itemsSchema},
			map[string]workflow.Binding{
				"count": workflowInput("count"), "items": workflowInput("items"),
			},
			map[string]workflow.OutputBinding{},
			config,
		)

		assertCompileErrorContains(t, definition, "must not contain Loop or Batch")
	})
}

func TestLoopEventsUseIterationScopeAndRootRunID(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	body := passthroughIndexBody(t, integerSchema)
	config := workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 3}
	definition := loopParentDefinition(
		t,
		"event-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		config,
	)
	plan := compileRoundTrip(t, definition)

	var events []workflow.Event

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "loop-run", nil }),
		workflow.WithEventSink(func(event workflow.Event) { events = append(events, event) }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	if _, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(2),
	}); err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	indexes := map[int]bool{}

	for _, event := range events {
		if event.RunID != "loop-run" {
			t.Fatalf("event RunID = %q, want loop-run", event.RunID)
		}

		if event.DefinitionID != body.ID {
			continue
		}

		scope := event.Scope()
		if len(scope) != 1 || scope[0].Kind != workflow.ScopeLoopIteration ||
			scope[0].NodeID != "loop" {
			t.Fatalf("loop event scope = %#v", scope)
		}

		indexes[scope[0].Index] = true
	}

	if len(indexes) != 2 || !indexes[0] || !indexes[1] {
		t.Fatalf("iteration indexes = %#v, want 0 and 1", indexes)
	}
}

func TestLoopAllowsMutuallyExclusiveVariableWrites(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	body := exclusiveWritesBody(t, integerSchema)
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopCount, MaxIterations: 2,
		Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
		Outputs: []workflow.LoopOutput{
			{Name: "final", Source: workflow.LoopOutputVariable, Port: "counter"},
		},
	}
	definition := loopParentDefinition(
		t,
		"exclusive-writes-loop",
		map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
		map[string]workflow.Binding{
			"count": workflowInput("count"), "counter": workflowInput("counter"),
		},
		map[string]workflow.OutputBinding{
			"final": nodeOutput(integerSchema, "loop", "final"),
		},
		config,
	)
	plan := compileRoundTrip(t, definition)

	result := runWorkflow(t, plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(1), "counter": workflow.MustValueOf(0),
	})
	if got := result.Outputs["final"].String(); got != `1` {
		t.Fatalf("final = %s, want 1", got)
	}
}

func TestLoopBodyChangesParentPlanFingerprint(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	firstBody := passthroughIndexBody(t, integerSchema)
	secondBody := firstBody
	secondBody.Limits.MaxSteps--

	definitionFor := func(body workflow.Definition) workflow.Definition {
		return loopParentDefinition(
			t,
			"fingerprint-loop",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 2},
		)
	}

	first := compileRoundTrip(t, definitionFor(firstBody))
	second := compileRoundTrip(t, definitionFor(secondBody))

	if first.Fingerprint() == second.Fingerprint() {
		t.Fatal("parent Plan fingerprint did not change with the Loop body")
	}
}

func statefulLoopBody(t *testing.T, integerSchema workflow.PortSchema) workflow.Definition {
	t.Helper()

	stopAt := workflow.MustValueOf(1)
	condition := workflow.ConditionConfig{
		Predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "index", Value: &stopAt},
		TrueRoute: "stop", FalseRoute: "next",
	}
	setConfig := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{
		{Target: "counter", Input: "next"},
	}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "stateful-loop-body", Revision: "v1", Name: "Stateful Loop Body",
		Inputs: map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{
			"value": nodeOutput(integerSchema, "increment", "next"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "increment", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "increment_loop_variable"),
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
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, condition),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{ID: "break", Type: workflow.NodeTypeBreak, Version: workflow.BuiltinNodeVersion},
			{ID: "continue", Type: workflow.NodeTypeContinue, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "increment"),
			edge("increment", workflow.RouteSuccess, "set"),
			edge("set", workflow.RouteSuccess, "condition"),
			edge("condition", "stop", "break"),
			edge("condition", "next", "continue"),
			edge("break", workflow.RouteSuccess, "end"),
			edge("continue", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func arrayLoopBody(
	t *testing.T,
	stringSchema workflow.PortSchema,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "array-loop-body", Revision: "v1", Name: "Array Loop Body",
		Inputs: map[string]workflow.PortSchema{
			"left": stringSchema, "right": stringSchema,
			"prefix": stringSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "join", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "join", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "join_loop_arrays"),
				Inputs: map[string]workflow.Binding{
					"left": workflowInput("left"), "right": workflowInput("right"),
					"prefix": workflowInput("prefix"), "index": workflowInput("index"),
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "join"),
			edge("join", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func indexedBreakBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
	breakIndex int,
) workflow.Definition {
	t.Helper()

	stopAt := workflow.MustValueOf(breakIndex)
	condition := workflow.ConditionConfig{
		Predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "index", Value: &stopAt},
		TrueRoute: "stop", FalseRoute: "next",
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: workflow.DefinitionID(fmt.Sprintf("indexed-break-%d", breakIndex)),
		Revision: "v1", Name: "Indexed Break Body",
		Inputs: map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{
			"index": {Schema: integerSchema, Binding: workflowInput("index")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, condition),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{ID: "break", Type: workflow.NodeTypeBreak, Version: workflow.BuiltinNodeVersion},
			{ID: "continue", Type: workflow.NodeTypeContinue, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "stop", "break"),
			edge("condition", "next", "continue"),
			edge("break", workflow.RouteSuccess, "end"),
			edge("continue", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func passthroughIndexBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "passthrough-index", Revision: "v1", Name: "Passthrough Index",
		Inputs: map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{
			"index": {Schema: integerSchema, Binding: workflowInput("index")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges:  []workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
		Limits: workflow.DefaultLimits(),
	}
}

func unorderedWritesBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	config := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{
		{Target: "counter", Input: "value"},
	}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "unordered-writes", Revision: "v1", Name: "Unordered Writes",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "left", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLiteral, Value: new(workflow.MustValueOf(1))},
				},
			},
			{
				ID: "right", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLiteral, Value: new(workflow.MustValueOf(2))},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "left"),
			edge("start", workflow.RouteSuccess, "right"),
			edge("left", workflow.RouteSuccess, "end"),
			edge("right", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
}

func setVariableLiteralBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
	value workflow.Value,
) workflow.Definition {
	t.Helper()

	config := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{
		{Target: "counter", Input: "value"},
	}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "literal-assignment", Revision: "v1", Name: "Literal Assignment",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "set", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLiteral, Value: new(value)},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "set"),
			edge("set", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func exclusiveWritesBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	zero := workflow.MustValueOf(0)
	condition := workflow.ConditionConfig{
		Predicate: workflow.Predicate{Op: workflow.PredicateEqual, Input: "index", Value: &zero},
		TrueRoute: "left", FalseRoute: "right",
	}
	config := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{
		{Target: "counter", Input: "value"},
	}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "exclusive-writes", Revision: "v1", Name: "Exclusive Writes",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "condition", Type: workflow.NodeTypeCondition, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, condition),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{
				ID: "left", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLiteral, Value: new(workflow.MustValueOf(1))},
				},
			},
			{
				ID: "right", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLiteral, Value: new(workflow.MustValueOf(2))},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "condition"),
			edge("condition", "left", "left"),
			edge("condition", "right", "right"),
			edge("left", workflow.RouteSuccess, "end"),
			edge("right", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
}

func nestedLoopBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
	countSchema workflow.PortSchema,
	innerConfig workflow.LoopConfig,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "nested-loop-body", Revision: "v1", Name: "Nested Loop Body",
		Inputs: map[string]workflow.PortSchema{
			"index": integerSchema, "inner_count": countSchema,
		},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "inner", Type: workflow.NodeTypeLoop, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, innerConfig),
				Inputs: map[string]workflow.Binding{"count": workflowInput("inner_count")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "inner"),
			edge("inner", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func singleActionLoopBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
	action workflow.ActionKey,
	nodeID workflow.NodeID,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "single-action-loop-body", Revision: "v1", Name: "Single Action Loop Body",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: nodeID, Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, action),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
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

func breakWithFailingSiblingBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "break-failure-body", Revision: "v1", Name: "Break Failure Body",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "break", Type: workflow.NodeTypeBreak, Version: workflow.BuiltinNodeVersion},
			{
				ID: "fail", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "fail_break_sibling"),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "break"),
			edge("start", workflow.RouteSuccess, "fail"),
			edge("break", workflow.RouteSuccess, "end"),
			edge("fail", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.Limits{MaxConcurrency: 2, MaxSteps: 100},
	}
}

func batchBodyContainingLoop(
	t *testing.T,
	stringSchema workflow.PortSchema,
	integerSchema workflow.PortSchema,
	loopConfig workflow.LoopConfig,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "batch-body-containing-loop", Revision: "v1", Name: "Batch Body Containing Loop",
		Inputs: map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": {Schema: stringSchema, Binding: workflowInput("item")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "inner", Type: workflow.NodeTypeLoop, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, loopConfig),
				Inputs: map[string]workflow.Binding{
					"count": {Source: workflow.BindingLiteral, Value: new(workflow.MustValueOf(1))},
				},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "inner"),
			edge("inner", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func loopBodyContainingBatch(
	t *testing.T,
	stringSchema workflow.PortSchema,
	integerSchema workflow.PortSchema,
	itemsSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	batchBody := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "nested-batch-body", Revision: "v1", Name: "Nested Batch Body",
		Inputs: map[string]workflow.PortSchema{
			"item": stringSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"result": {Schema: stringSchema, Binding: workflowInput("item")},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges:  []workflow.ControlEdge{edge("start", workflow.RouteSuccess, "end")},
		Limits: workflow.DefaultLimits(),
	}
	batchConfig := workflow.BatchConfig{
		Body: batchBody, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 2,
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "loop-body-containing-batch", Revision: "v1", Name: "Loop Body Containing Batch",
		Inputs: map[string]workflow.PortSchema{
			"index": integerSchema, "items": itemsSchema,
		},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "batch", Type: workflow.NodeTypeBatch, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, batchConfig),
				Inputs: map[string]workflow.Binding{"items": workflowInput("items")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "batch"),
			edge("batch", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func loopParentDefinition(
	t *testing.T,
	id workflow.DefinitionID,
	inputs map[string]workflow.PortSchema,
	bindings map[string]workflow.Binding,
	outputs map[string]workflow.OutputBinding,
	config workflow.LoopConfig,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: "Loop Parent",
		Inputs: inputs, Outputs: outputs,
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "loop", Type: workflow.NodeTypeLoop, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: bindings,
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "loop"),
			edge("loop", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func assertCompileErrorContains(
	t *testing.T,
	definition workflow.Definition,
	message string,
) {
	t.Helper()

	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	_, err = workflow.Compile(t.Context(), definition, registry)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), message) {
		t.Fatalf("Compile() error = %v, want ErrCompile containing %q", err, message)
	}
}

func TestLoopFailureDoesNotStartSuccessorIteration(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	var calls atomic.Int32

	fail := &fakeAction{
		spec: actionSpec(
			"fail_loop_iteration",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(_ context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{}, errors.New("iteration failed")
		},
	}
	body := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "failed-iteration", Revision: "v1", Name: "Failed Iteration",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "fail", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "fail_loop_iteration"),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "fail"),
			edge("fail", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	config := workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 5}
	definition := loopParentDefinition(
		t,
		"failed-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		config,
	)
	plan := compileRoundTrip(t, definition, fail)
	runner := mustRunner(t)

	_, err := runner.Run(t.Context(), plan, map[string]workflow.Value{"count": workflow.MustValueOf(3)})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("failed Action calls = %d, want 1", got)
	}
}

func TestLoopCancellationDoesNotStartSuccessorIteration(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	started := make(chan struct{}, 1)

	var calls atomic.Int32

	block := &fakeAction{
		spec: actionSpec(
			"block_loop_iteration",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			started <- struct{}{}

			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	body := singleActionLoopBody(t, integerSchema, "block_loop_iteration", "block")
	config := workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 5}
	definition := loopParentDefinition(
		t,
		"canceled-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		config,
	)
	plan := compileRoundTrip(t, definition, block)
	runner := mustRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(ctx, plan, map[string]workflow.Value{
			"count": workflow.MustValueOf(3),
		})
		resultChannel <- result

		errorChannel <- err
	}()

	<-started
	cancel()

	result := <-resultChannel

	if err := <-errorChannel; !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	if result.Status != workflow.RunStatusCanceled {
		t.Fatalf("Run status = %q, want canceled", result.Status)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("canceled Action calls = %d, want 1", got)
	}
}

func TestLoopDiscardsBreakWhenSiblingFails(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)

	var calls atomic.Int32

	fail := &fakeAction{
		spec: actionSpec(
			"fail_break_sibling",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{},
		),
		run: func(_ context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{}, errors.New("sibling failed")
		},
	}
	body := breakWithFailingSiblingBody(t, integerSchema)
	config := workflow.LoopConfig{Body: body, Mode: workflow.LoopInfinite, MaxIterations: 3}
	definition := loopParentDefinition(
		t,
		"break-failure-loop",
		map[string]workflow.PortSchema{},
		map[string]workflow.Binding{},
		map[string]workflow.OutputBinding{},
		config,
	)
	plan := compileRoundTrip(t, definition, fail)
	runner := mustRunner(t)

	_, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("failed sibling calls = %d, want 1", got)
	}
}

func TestLoopSharesRootStepLimit(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	body := passthroughIndexBody(t, integerSchema)
	config := workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 5}
	definition := loopParentDefinition(
		t,
		"step-limited-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		config,
	)
	definition.Limits = workflow.Limits{MaxConcurrency: 1, MaxSteps: 4}
	plan := compileRoundTrip(t, definition)
	runner := mustRunner(t)

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"count": workflow.MustValueOf(2),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if got := result.Nodes["loop"].Failure; got != workflow.FailureLimit {
		t.Fatalf("Loop failure = %q, want %q", got, workflow.FailureLimit)
	}
}

func TestLoopPlanSupportsConcurrentRunsWithoutStateLeakage(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	increment := &fakeAction{
		spec: actionSpec(
			"concurrent_increment",
			map[string]workflow.PortSchema{"value": integerSchema},
			map[string]workflow.PortSchema{"next": integerSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			value, err := workflow.DecodeValue[int](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"next": workflow.MustValueOf(value + 1),
			}}, nil
		},
	}
	body := concurrentStateBody(t, integerSchema)
	config := workflow.LoopConfig{
		Body: body, Mode: workflow.LoopCount, MaxIterations: 4,
		Variables: []workflow.LoopVariable{{Name: "counter", Schema: integerSchema}},
		Outputs: []workflow.LoopOutput{
			{Name: "final", Source: workflow.LoopOutputVariable, Port: "counter"},
		},
	}
	definition := loopParentDefinition(
		t,
		"concurrent-loop",
		map[string]workflow.PortSchema{"count": countSchema, "counter": integerSchema},
		map[string]workflow.Binding{
			"count": workflowInput("count"), "counter": workflowInput("counter"),
		},
		map[string]workflow.OutputBinding{
			"final": nodeOutput(integerSchema, "loop", "final"),
		},
		config,
	)
	plan := compileRoundTrip(t, definition, increment)
	runner := mustRunner(t)

	type runOutcome struct {
		initial int
		result  workflow.RunResult
		err     error
	}

	outcomes := make(chan runOutcome, 8)

	for initial := range 8 {
		go func() {
			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
				"count": workflow.MustValueOf(3), "counter": workflow.MustValueOf(initial),
			})
			outcomes <- runOutcome{initial: initial, result: result, err: err}
		}()
	}

	for range 8 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("Runner.Run(%d) error = %v", outcome.initial, outcome.err)
		}

		final, err := workflow.DecodeValue[int](outcome.result.Outputs["final"])
		if err != nil {
			t.Fatalf("DecodeValue() error = %v", err)
		}

		if final != outcome.initial+3 {
			t.Fatalf("final for %d = %d, want %d", outcome.initial, final, outcome.initial+3)
		}
	}
}

func concurrentStateBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	setConfig := workflow.SetVariableConfig{Assignments: []workflow.SetVariableAssignment{
		{Target: "counter", Input: "next"},
	}}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "concurrent-loop-body", Revision: "v1", Name: "Concurrent Loop Body",
		Inputs:  map[string]workflow.PortSchema{"index": integerSchema},
		Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "increment", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "concurrent_increment"),
				Inputs: map[string]workflow.Binding{
					"value": {Source: workflow.BindingLoopVariable, Port: "counter"},
				},
			},
			{
				ID: "set", Type: workflow.NodeTypeSetVariable, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, setConfig),
				Inputs: map[string]workflow.Binding{"next": nodeBinding("increment", "next")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "increment"),
			edge("increment", workflow.RouteSuccess, "set"),
			edge("set", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}
