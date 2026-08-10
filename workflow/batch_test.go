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

func TestBatchNodeSequentialPreservesOrderAndLiftedInputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	action := &fakeAction{
		spec: actionSpec(
			"map_item",
			map[string]workflow.PortSchema{
				"item": stringSchema, "index": integerSchema, "prefix": stringSchema,
			},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			item, err := workflow.DecodeValue[string](input.Values["item"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			index, err := workflow.DecodeValue[int](input.Values["index"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			prefix, err := workflow.DecodeValue[string](input.Values["prefix"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(fmt.Sprintf("%s:%s:%d", prefix, item, index)),
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "map_item", true)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, stringSchema)
	plan := compileRoundTrip(t, definition, action)

	result := runWorkflow(t, plan, map[string]workflow.Value{
		"items":  workflow.MustValueOf([]string{"a", "b", "c"}),
		"prefix": workflow.MustValueOf("p"),
	})
	if got := result.Outputs["results"].String(); got != `["p:a:0","p:b:1","p:c:2"]` {
		t.Fatalf("results = %s", got)
	}

	empty := runWorkflow(t, plan, map[string]workflow.Value{
		"items":  workflow.MustValueOf([]string{}),
		"prefix": workflow.MustValueOf("p"),
	})
	if got := empty.Outputs["results"].String(); got != `[]` {
		t.Fatalf("empty results = %s, want []", got)
	}
}

func TestBatchNodeParallelBoundsConcurrencyAndPreservesOrder(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	started := make(chan struct{}, 4)
	release := make(chan struct{})

	var (
		active  atomic.Int32
		maximum atomic.Int32
	)

	action := &fakeAction{
		spec: actionSpec(
			"parallel_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			current := active.Add(1)
			defer active.Add(-1)

			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}

			started <- struct{}{}

			select {
			case <-ctx.Done():
				return workflow.ActionOutput{}, ctx.Err()
			case <-release:
				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["item"],
				}}, nil
			}
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "parallel_item", false)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		MaxConcurrency: 2, ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
	definition.Limits = workflow.Limits{MaxConcurrency: 4, MaxSteps: 100}
	plan := compileRoundTrip(t, definition, action)
	runner := mustRunner(t)

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"a", "b", "c", "d"}),
		})
		resultChannel <- result

		errorChannel <- err
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two parallel item Actions did not start")
		}
	}

	close(release)

	if err := <-errorChannel; err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	result := <-resultChannel
	if got := result.Outputs["results"].String(); got != `["a","b","c","d"]` {
		t.Fatalf("results = %s", got)
	}

	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active Actions = %d, want 2", got)
	}
}

func TestBatchNodeSharesRootConcurrencyAndStepLimits(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	started := make(chan struct{}, 3)
	release := make(chan struct{})

	var (
		active  atomic.Int32
		maximum atomic.Int32
	)

	action := &fakeAction{
		spec: actionSpec(
			"shared_limit_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			current := active.Add(1)
			defer active.Add(-1)

			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}

			started <- struct{}{}

			select {
			case <-ctx.Done():
				return workflow.ActionOutput{}, ctx.Err()
			case <-release:
				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["item"],
				}}, nil
			}
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "shared_limit_item", false)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		MaxConcurrency: 3, ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
	definition.Limits = workflow.Limits{MaxConcurrency: 1, MaxSteps: 100}
	plan := compileRoundTrip(t, definition, action)
	runner := mustRunner(t)

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"a", "b", "c"}),
		})
		resultChannel <- result

		errorChannel <- err
	}()

	<-started

	select {
	case <-started:
		t.Fatal("a second Action bypassed the root concurrency limit")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	if err := <-errorChannel; err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	<-resultChannel

	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum active Actions = %d, want 1", got)
	}

	stepDefinition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
	stepDefinition.Limits = workflow.Limits{MaxConcurrency: 1, MaxSteps: 4}
	stepPlan := compileRoundTrip(t, stepDefinition, action)

	result, err := runner.Run(t.Context(), stepPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"only"}),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("shared step limit error = %v, want ErrRun", err)
	}

	if result.Nodes["batch"].Failure != workflow.FailureLimit {
		t.Fatalf("Batch failure = %q, want limit", result.Nodes["batch"].Failure)
	}
}

func TestBatchNodeItemErrorModes(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	nullableResultsSchema := mustSchema(
		t,
		`{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}}`,
	)
	action := &fakeAction{
		spec: actionSpec(
			"fallible_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			item, err := workflow.DecodeValue[string](input.Values["item"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			if item == "bad" {
				return workflow.ActionOutput{}, errors.New("item failed")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "fallible_item", false)

	tests := []struct {
		name          string
		errorMode     workflow.BatchErrorMode
		resultsSchema workflow.PortSchema
		want          string
		wantError     bool
	}{
		{
			name: "terminate", errorMode: workflow.BatchTerminate,
			resultsSchema: itemsSchema, wantError: true,
		},
		{
			name: "continue with null", errorMode: workflow.BatchContinueWithNull,
			resultsSchema: nullableResultsSchema, want: `["a",null,"c"]`,
		},
		{
			name: "remove failed", errorMode: workflow.BatchRemoveFailed,
			resultsSchema: itemsSchema, want: `["a","c"]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			config := workflow.BatchConfig{
				Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
				ErrorMode: test.errorMode, MaxItems: 10,
			}
			definition := batchParentDefinition(
				t,
				itemsSchema,
				test.resultsSchema,
				config,
				workflow.PortSchema{},
			)
			plan := compileRoundTrip(t, definition, action)
			runner := mustRunner(t)
			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
				"items": workflow.MustValueOf([]string{"a", "bad", "c"}),
			})

			if test.wantError {
				if !errors.Is(err, workflow.ErrRun) {
					t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Runner.Run() error = %v", err)
			}

			if got := result.Outputs["results"].String(); got != test.want {
				t.Fatalf("results = %s, want %s", got, test.want)
			}
		})
	}
}

func TestBatchNodeParallelPropagatesCancellation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	started := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	action := &fakeAction{
		spec: actionSpec(
			"blocking_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			started <- struct{}{}

			<-ctx.Done()

			finished <- struct{}{}

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "blocking_item", false)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		MaxConcurrency: 2, ErrorMode: workflow.BatchContinueWithNull, MaxItems: 10,
	}
	nullableResultsSchema := mustSchema(
		t,
		`{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}}`,
	)
	definition := batchParentDefinition(
		t,
		itemsSchema,
		nullableResultsSchema,
		config,
		workflow.PortSchema{},
	)
	definition.Limits = workflow.Limits{MaxConcurrency: 2, MaxSteps: 100}
	plan := compileRoundTrip(t, definition, action)
	runner := mustRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(ctx, plan, map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"a", "b", "c"}),
		})
		resultChannel <- result

		errorChannel <- err
	}()

	for range 2 {
		<-started
	}

	cancel()

	if err := <-errorChannel; !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	if result := <-resultChannel; result.Status != workflow.RunStatusCanceled {
		t.Fatalf("RunResult.Status = %q, want canceled", result.Status)
	}

	for range 2 {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("Batch worker did not finish after cancellation")
		}
	}
}

func TestBatchNodeRejectsInvalidBoundsAndNestedBatch(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	leafAction := &fakeAction{spec: actionSpec(
		"leaf_item",
		map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
		map[string]workflow.PortSchema{"result": stringSchema},
	)}
	leafBody := batchBodyDefinition(t, stringSchema, stringSchema, "leaf_item", false)

	registry, err := workflow.NewDefaultRegistry(leafAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	invalidConfigs := []workflow.BatchConfig{
		{
			Body: leafBody, ResultOutput: "result", Mode: workflow.BatchParallel,
			MaxConcurrency: 0, ErrorMode: workflow.BatchTerminate, MaxItems: 10,
		},
		{
			Body: leafBody, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 10_001,
		},
	}
	for _, invalidConfig := range invalidConfigs {
		invalid := batchParentDefinition(
			t,
			itemsSchema,
			itemsSchema,
			invalidConfig,
			workflow.PortSchema{},
		)

		_, err = workflow.Compile(t.Context(), invalid, registry)
		if !errors.Is(err, workflow.ErrCompile) {
			t.Fatalf("invalid bounds Compile() error = %v, want ErrCompile", err)
		}
	}

	runtimeLimitConfig := workflow.BatchConfig{
		Body: leafBody, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 1,
	}
	runtimeLimitDefinition := batchParentDefinition(
		t,
		itemsSchema,
		itemsSchema,
		runtimeLimitConfig,
		workflow.PortSchema{},
	)
	runtimeLimitPlan := compileRoundTrip(t, runtimeLimitDefinition, leafAction)
	runner := mustRunner(t)

	_, err = runner.Run(t.Context(), runtimeLimitPlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b"}),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("runtime item limit error = %v, want ErrRun", err)
	}

	innerBody := nestedBatchBodyDefinition(t, integerSchema, leafBody)
	arrayItemsSchema := mustSchema(
		t,
		`{"type":"array","items":{"type":"array","items":{"type":"string"}}}`,
	)
	outerConfig := workflow.BatchConfig{
		Body: innerBody, ResultOutput: "results", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	outer := batchParentDefinition(
		t,
		arrayItemsSchema,
		arrayItemsSchema,
		outerConfig,
		workflow.PortSchema{},
	)

	_, err = workflow.Compile(t.Context(), outer, registry)
	if !errors.Is(err, workflow.ErrCompile) || !strings.Contains(err.Error(), "must not contain Batch") {
		t.Fatalf("nested Batch Compile() error = %v, want nested Batch ErrCompile", err)
	}
}

func TestBatchEventsAreScopedAndSerialized(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	action := &fakeAction{
		spec: actionSpec(
			"event_item",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "event_item", false)
	config := workflow.BatchConfig{
		Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
		MaxConcurrency: 3, ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}
	definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
	definition.Limits = workflow.Limits{MaxConcurrency: 3, MaxSteps: 100}
	plan := compileRoundTrip(t, definition, action)

	var (
		activeSink     atomic.Int32
		concurrentSink atomic.Bool
	)

	events := make([]workflow.Event, 0)

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "batch-run", nil }),
		workflow.WithEventSink(func(event workflow.Event) {
			if activeSink.Add(1) != 1 {
				concurrentSink.Store(true)
			}

			time.Sleep(100 * time.Microsecond)

			events = append(events, event)

			activeSink.Add(-1)
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"a", "b", "c"}),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if concurrentSink.Load() {
		t.Fatal("event sink was invoked concurrently for one Run")
	}

	indexes := map[int]bool{}

	for _, event := range events {
		if event.RunID != "batch-run" {
			t.Fatalf("event RunID = %q, want batch-run", event.RunID)
		}

		if event.DefinitionID != body.ID {
			continue
		}

		scope := event.Scope()
		if len(scope) != 1 || scope[0].Kind != workflow.ScopeBatchItem ||
			scope[0].NodeID != "batch" {
			t.Fatalf("batch event scope = %#v", scope)
		}

		indexes[scope[0].Index] = true
	}

	if len(indexes) != 3 {
		t.Fatalf("scoped batch indexes = %#v, want 0, 1, 2", indexes)
	}
}

func batchBodyDefinition(
	t *testing.T,
	itemSchema workflow.PortSchema,
	resultSchema workflow.PortSchema,
	action workflow.ActionKey,
	withPrefix bool,
) workflow.Definition {
	t.Helper()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	inputs := map[string]workflow.PortSchema{
		"item": itemSchema, "index": integerSchema,
	}
	actionInputs := map[string]workflow.Binding{
		"item": workflowInput("item"), "index": workflowInput("index"),
	}

	if withPrefix {
		inputs["prefix"] = resultSchema
		actionInputs["prefix"] = workflowInput("prefix")
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "batch-body", Revision: "v1", Name: "Batch Body",
		Inputs: inputs,
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(resultSchema, "map", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "map", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, action), Inputs: actionInputs,
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "map"),
			edge("map", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
}

func batchParentDefinition(
	t *testing.T,
	itemsSchema workflow.PortSchema,
	resultsSchema workflow.PortSchema,
	config workflow.BatchConfig,
	liftedSchema workflow.PortSchema,
) workflow.Definition {
	t.Helper()

	inputs := map[string]workflow.PortSchema{"items": itemsSchema}
	bindings := map[string]workflow.Binding{"items": workflowInput("items")}

	if liftedSchema.IsValid() {
		inputs["prefix"] = liftedSchema
		bindings["prefix"] = workflowInput("prefix")
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "batch-parent", Revision: "v1", Name: "Batch Parent",
		Inputs: inputs,
		Outputs: map[string]workflow.OutputBinding{
			"results": nodeOutput(resultsSchema, "batch", "results"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "batch", Type: workflow.NodeTypeBatch, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config), Inputs: bindings,
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

func nestedBatchBodyDefinition(
	t *testing.T,
	integerSchema workflow.PortSchema,
	leafBody workflow.Definition,
) workflow.Definition {
	t.Helper()

	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	config := workflow.BatchConfig{
		Body: leafBody, ResultOutput: "result", Mode: workflow.BatchSequential,
		ErrorMode: workflow.BatchTerminate, MaxItems: 10,
	}

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "nested-body", Revision: "v1", Name: "Nested Body",
		Inputs: map[string]workflow.PortSchema{
			"item": itemsSchema, "index": integerSchema,
		},
		Outputs: map[string]workflow.OutputBinding{
			"results": nodeOutput(itemsSchema, "inner", "results"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "inner", Type: workflow.NodeTypeBatch, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, config),
				Inputs: map[string]workflow.Binding{"items": workflowInput("item")},
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
