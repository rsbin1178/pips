package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func TestGetExecutionContextWithoutInvocation(t *testing.T) {
	t.Parallel()

	var nilContext context.Context

	for _, ctx := range []context.Context{nilContext, context.Background()} {
		identity, ok := workflow.GetExecutionContext(ctx)
		if ok || identity.RunID != "" || identity.DefinitionID != "" ||
			identity.Revision != "" || identity.Address.NodeID != "" ||
			len(identity.Address.Scope) != 0 || identity.Attempt != 0 {
			t.Fatalf("GetExecutionContext() = %#v, %v, want zero, false", identity, ok)
		}
	}
}

func TestExecutionContextMatchesActionAndCustomNodeEvents(t *testing.T) {
	t.Parallel()

	t.Run("action", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 1)
		action := executionContextAction("identity_action", stringSchema, identities)
		definition := singleActionDefinition(
			t,
			"identity_action",
			stringSchema,
			workflow.NodePolicy{},
		)
		plan := compileRoundTrip(t, definition, action)

		var started workflow.Event

		runner, err := workflow.NewRunner(
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "action-context-run", nil
			}),
			workflow.WithEventSink(func(event workflow.Event) {
				if event.Type() == workflow.EventNodeStarted && event.NodeID == "action" {
					started = event
				}
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		if err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		identity := <-identities
		assertExecutionContext(t, identity, workflow.ExecutionContext{
			RunID:        result.RunID,
			DefinitionID: plan.DefinitionID(),
			Revision:     plan.Revision(),
			Address:      workflow.NodeAddress{NodeID: "action"},
			Attempt:      1,
		})
		assertExecutionContextMatchesEvent(t, identity, started)
	})

	t.Run("custom node type", func(t *testing.T) {
		t.Parallel()

		identities := make(chan workflow.ExecutionContext, 1)
		nodeType := executionContextNodeType{identities: identities}
		nodeTypes := append(workflow.BuiltinNodeTypes(), nodeType)

		registry, err := workflow.NewRegistry(nodeTypes, nil)
		if err != nil {
			t.Fatalf("NewRegistry() error = %v", err)
		}

		definition := workflow.Definition{
			Schema: workflow.SchemaV1Alpha1, ID: "custom-context", Revision: "v2",
			Name: "Custom Context", Inputs: map[string]workflow.WorkflowInput{},
			Outputs: map[string]workflow.OutputBinding{},
			Nodes: []workflow.NodeDefinition{
				{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
				{ID: "custom", Type: nodeType.Spec().Key, Version: nodeType.Spec().Version},
				{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
			},
			Edges: []workflow.ControlEdge{
				edge("start", workflow.RouteSuccess, "custom"),
				edge("custom", workflow.RouteSuccess, "end"),
			},
			Limits: workflow.DefaultLimits(),
		}

		plan, err := workflow.Compile(t.Context(), definition, registry)
		if err != nil {
			t.Fatalf("Compile() error = %v", err)
		}

		runner, err := workflow.NewRunner(workflow.WithRunIDSource(
			func(time.Time) (string, error) { return "custom-context-run", nil },
		))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		if err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		assertExecutionContext(t, <-identities, workflow.ExecutionContext{
			RunID:        result.RunID,
			DefinitionID: "custom-context",
			Revision:     "v2",
			Address:      workflow.NodeAddress{NodeID: "custom"},
			Attempt:      1,
		})
	})
}

func TestExecutionContextRetryAndTimeout(t *testing.T) {
	t.Parallel()

	t.Run("retry uses cumulative attempts", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 2)

		var calls atomic.Int32

		action := &fakeAction{
			spec: actionSpec(
				"context_retry",
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				if calls.Add(1) == 1 {
					return workflow.ActionOutput{}, errors.New("retry")
				}

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf("ok"),
				}}, nil
			},
		}
		definition := singleActionDefinition(t, "context_retry", stringSchema, workflow.NodePolicy{
			Retry: workflow.RetryPolicy{MaxAttempts: 2},
		})
		plan := compileRoundTrip(t, definition, action)
		runner := runnerWithID(t, "retry-context-run")

		if _, err := runner.Run(t.Context(), plan, map[string]workflow.Value{}); err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		first := <-identities

		second := <-identities
		if first.Attempt != 1 || second.Attempt != 2 {
			t.Fatalf("attempts = %d, %d, want 1, 2", first.Attempt, second.Attempt)
		}

		if first.RunID != second.RunID || first.Address.NodeID != second.Address.NodeID {
			t.Fatalf("retry identity changed: first=%#v second=%#v", first, second)
		}
	})

	t.Run("timeout keeps identity and deadline", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 1)
		deadlines := make(chan bool, 1)
		action := &fakeAction{
			spec: actionSpec(
				"context_timeout",
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				_, hasDeadline := ctx.Deadline()
				deadlines <- hasDeadline

				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			},
		}
		definition := singleActionDefinition(t, "context_timeout", stringSchema, workflow.NodePolicy{
			TimeoutMilli: 10,
		})
		plan := compileRoundTrip(t, definition, action)
		runner := runnerWithID(t, "timeout-context-run")

		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		if !errors.Is(err, context.DeadlineExceeded) ||
			result.Nodes["action"].Failure != workflow.FailureTimeout {
			t.Fatalf("Runner.Run() result=%#v error=%v, want timeout", result, err)
		}

		identity := <-identities
		if identity.RunID != result.RunID || identity.Attempt != 1 || !<-deadlines {
			t.Fatalf("timeout identity=%#v hasDeadline=false", identity)
		}
	})
}

func TestExecutionContextCancellation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	identities := make(chan workflow.ExecutionContext, 1)
	started := make(chan struct{})
	action := &fakeAction{
		spec: actionSpec(
			"context_cancel",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			identity, ok := workflow.GetExecutionContext(ctx)
			if !ok {
				return workflow.ActionOutput{}, errors.New("execution context is unavailable")
			}

			identities <- identity

			close(started)
			<-ctx.Done()

			return workflow.ActionOutput{}, ctx.Err()
		},
	}
	definition := singleActionDefinition(t, "context_cancel", stringSchema, workflow.NodePolicy{})
	plan := compileRoundTrip(t, definition, action)
	runner := runnerWithID(t, "cancel-context-run")
	runCtx, cancel := context.WithCancel(t.Context())

	resultChannel := make(chan workflow.RunResult, 1)
	errorChannel := make(chan error, 1)

	go func() {
		result, err := runner.Run(runCtx, plan, map[string]workflow.Value{})
		resultChannel <- result

		errorChannel <- err
	}()

	<-started
	cancel()

	err := <-errorChannel

	result := <-resultChannel
	if !errors.Is(err, context.Canceled) || result.Status != workflow.RunStatusCanceled {
		t.Fatalf("Runner.Run() result=%#v error=%v, want canceled", result, err)
	}

	identity := <-identities
	assertExecutionContext(t, identity, workflow.ExecutionContext{
		RunID:        result.RunID,
		DefinitionID: plan.DefinitionID(),
		Revision:     plan.Revision(),
		Address:      workflow.NodeAddress{NodeID: "action"},
		Attempt:      1,
	})
}

func TestExecutionContextCompositeScopes(t *testing.T) {
	t.Parallel()

	t.Run("sub-workflow", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 1)
		action := &fakeAction{
			spec: actionSpec(
				"context_child_action",
				map[string]workflow.PortSchema{"value": stringSchema},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
				first, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				first.Address.Scope[0].Index = 99

				second, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context disappeared")
				}

				identities <- second

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["value"],
				}}, nil
			},
		}
		child := subWorkflowChildDefinition(stringSchema, "context_child_action")
		parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
		plan := compileWithResolver(t, parent, staticResolver(child), action)
		started := make(chan workflow.Event, 1)

		runner, err := workflow.NewRunner(
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "sub-context-run", nil
			}),
			workflow.WithEventSink(func(event workflow.Event) {
				if event.Type() == workflow.EventNodeStarted &&
					event.DefinitionID == child.ID && event.NodeID == "child_action" {
					started <- event
				}
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		if _, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
			"value": workflow.MustValueOf("value"),
		}); err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		identity := <-identities
		assertExecutionContext(t, identity, workflow.ExecutionContext{
			RunID:        "sub-context-run",
			DefinitionID: child.ID,
			Revision:     child.Revision,
			Address: workflow.NodeAddress{NodeID: "child_action", Scope: []workflow.ScopeFrame{{
				Kind: workflow.ScopeSubWorkflow, NodeID: "sub", Index: -1,
			}}},
			Attempt: 1,
		})
		assertExecutionContextMatchesEvent(t, identity, <-started)
	})

	t.Run("parallel batch", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		integerSchema := mustSchema(t, `{"type":"integer"}`)
		itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
		identities := make(chan workflow.ExecutionContext, 3)
		action := &fakeAction{
			spec: actionSpec(
				"context_batch_action",
				map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["item"],
				}}, nil
			},
		}
		body := batchBodyDefinition(t, stringSchema, stringSchema, "context_batch_action", false)
		config := workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchParallel,
			MaxConcurrency: 3, ErrorMode: workflow.BatchTerminate, MaxItems: 3,
		}
		definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
		definition.Limits.MaxConcurrency = 3
		plan := compileRoundTrip(t, definition, action)
		started := make(chan workflow.Event, 3)

		runner, err := workflow.NewRunner(
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "batch-context-run", nil
			}),
			workflow.WithEventSink(func(event workflow.Event) {
				if event.Type() == workflow.EventNodeStarted &&
					event.DefinitionID == body.ID && event.NodeID == "map" {
					started <- event
				}
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		if _, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"a", "b", "c"}),
		}); err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		seen := make(map[int]bool, 3)
		events := make(map[int]workflow.Event, 3)

		for range 3 {
			event := <-started
			events[event.Scope()[0].Index] = event
		}

		for range 3 {
			identity := <-identities
			if identity.RunID != "batch-context-run" || identity.DefinitionID != body.ID ||
				identity.Address.NodeID != "map" || identity.Attempt != 1 ||
				len(identity.Address.Scope) != 1 ||
				identity.Address.Scope[0].Kind != workflow.ScopeBatchItem ||
				identity.Address.Scope[0].NodeID != "batch" {
				t.Fatalf("batch identity = %#v", identity)
			}

			index := identity.Address.Scope[0].Index
			seen[index] = true
			assertExecutionContextMatchesEvent(t, identity, events[index])
		}

		if len(seen) != 3 || !seen[0] || !seen[1] || !seen[2] {
			t.Fatalf("batch indexes = %#v, want 0, 1, 2", seen)
		}
	})

	t.Run("loop", func(t *testing.T) {
		t.Parallel()

		integerSchema := mustSchema(t, `{"type":"integer"}`)
		countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
		identities := make(chan workflow.ExecutionContext, 2)
		action := &fakeAction{
			spec: actionSpec(
				"context_loop_action",
				map[string]workflow.PortSchema{"index": integerSchema},
				map[string]workflow.PortSchema{"result": integerSchema},
			),
			run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["index"],
				}}, nil
			},
		}
		body := executionContextLoopBody(t, integerSchema, "context_loop_action")
		definition := loopParentDefinition(
			t,
			"loop-context-parent",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 2},
		)
		plan := compileRoundTrip(t, definition, action)
		started := make(chan workflow.Event, 2)

		runner, err := workflow.NewRunner(
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "loop-context-run", nil
			}),
			workflow.WithEventSink(func(event workflow.Event) {
				if event.Type() == workflow.EventNodeStarted &&
					event.DefinitionID == body.ID && event.NodeID == "work" {
					started <- event
				}
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		if _, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
			"count": workflow.MustValueOf(2),
		}); err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		for expectedIndex := range 2 {
			identity := <-identities
			event := <-started

			assertExecutionContext(t, identity, workflow.ExecutionContext{
				RunID:        "loop-context-run",
				DefinitionID: body.ID,
				Revision:     body.Revision,
				Address: workflow.NodeAddress{NodeID: "work", Scope: []workflow.ScopeFrame{{
					Kind: workflow.ScopeLoopIteration, NodeID: "loop", Index: expectedIndex,
				}}},
				Attempt: 1,
			})
			assertExecutionContextMatchesEvent(t, identity, event)
		}
	})
}

func TestExecutionContextResumeDebugAndPartialRun(t *testing.T) {
	t.Parallel()

	t.Run("static interruption", func(t *testing.T) {
		t.Parallel()

		identities := make(chan workflow.ExecutionContext, 1)
		action := &fakeAction{
			spec: actionSpec(
				"context_static",
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
			},
		}
		definition := executionContextEmptyActionDefinition(t, "static-context", "context_static")

		registry, err := workflow.NewDefaultRegistry(action)
		if err != nil {
			t.Fatalf("NewDefaultRegistry() error = %v", err)
		}

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
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "static-context-run", nil
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		assertInterrupted(t, interrupted, err, "static-context-run")

		resumed, err := runner.Resume(t.Context(), plan, interrupted.RunID, nil)
		if err != nil {
			t.Fatalf("Runner.Resume() error = %v", err)
		}

		assertExecutionContext(t, <-identities, workflow.ExecutionContext{
			RunID:        resumed.RunID,
			DefinitionID: plan.DefinitionID(),
			Revision:     plan.Revision(),
			Address:      workflow.NodeAddress{NodeID: "work"},
			Attempt:      1,
		})
	})

	t.Run("host interruption", func(t *testing.T) {
		t.Parallel()

		identities := make(chan workflow.ExecutionContext, 2)
		started := make(chan struct{})

		var calls atomic.Int32

		action := &fakeAction{
			spec: actionSpec(
				"context_host",
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				if calls.Add(1) == 1 {
					close(started)
					<-ctx.Done()

					return workflow.ActionOutput{}, ctx.Err()
				}

				return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
			},
		}
		definition := executionContextEmptyActionDefinition(t, "host-context", "context_host")

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
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "host-context-run", nil
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		runCtx, interrupt := workflow.WithRunInterrupt(t.Context())

		type outcome struct {
			result workflow.RunResult
			err    error
		}

		done := make(chan outcome, 1)

		go func() {
			result, runErr := runner.Run(runCtx, plan, map[string]workflow.Value{})
			done <- outcome{result: result, err: runErr}
		}()

		<-started
		interrupt(workflow.WithRunInterruptTimeout(0))

		interrupted := <-done
		assertInterrupted(t, interrupted.result, interrupted.err, "host-context-run")

		resumed, err := runner.Resume(t.Context(), plan, interrupted.result.RunID, nil)
		if err != nil {
			t.Fatalf("Runner.Resume() error = %v", err)
		}

		first := <-identities

		second := <-identities
		if first.RunID != resumed.RunID || second.RunID != resumed.RunID ||
			first.Address.NodeID != second.Address.NodeID ||
			first.Attempt != 1 || second.Attempt != 2 {
			t.Fatalf("host identities = %#v, %#v", first, second)
		}
	})

	t.Run("resume", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 2)

		var calls atomic.Int32

		action := &fakeAction{
			spec: actionSpec(
				"context_resume",
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				identity, ok := workflow.GetExecutionContext(ctx)
				if !ok {
					return workflow.ActionOutput{}, errors.New("execution context is unavailable")
				}

				identities <- identity

				if calls.Add(1) == 1 {
					return workflow.ActionOutput{}, workflow.Interrupt(
						ctx,
						workflow.MustValueOf("resume"),
					)
				}

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf("done"),
				}}, nil
			},
		}
		definition := singleActionDefinition(t, "context_resume", stringSchema, workflow.NodePolicy{})
		plan := compileRoundTrip(t, definition, action)
		store := &memoryCheckpointStore{}

		runner, err := workflow.NewRunner(
			workflow.WithCheckpointStore(store),
			workflow.WithRunIDSource(func(time.Time) (string, error) {
				return "resume-context-run", nil
			}),
		)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		assertInterrupted(t, interrupted, err, "resume-context-run")

		if len(interrupted.Interruption.Contexts) != 1 {
			t.Fatalf("interrupt contexts = %#v", interrupted.Interruption.Contexts)
		}

		resumed, err := runner.Resume(t.Context(), plan, interrupted.RunID, []workflow.ResumeTarget{{
			InterruptID: interrupted.Interruption.Contexts[0].ID,
		}})
		if err != nil {
			t.Fatalf("Runner.Resume() error = %v", err)
		}

		first := <-identities

		second := <-identities
		if first.RunID != resumed.RunID || second.RunID != resumed.RunID ||
			first.Address.NodeID != second.Address.NodeID ||
			first.Attempt != 1 || second.Attempt != 2 {
			t.Fatalf("resume identities = %#v, %#v", first, second)
		}
	})

	t.Run("node debug", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 1)
		action := executionContextAction("context_debug", stringSchema, identities)
		definition := singleActionDefinition(t, "context_debug", stringSchema, workflow.NodePolicy{})
		plan := compileRoundTrip(t, definition, action)

		debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
		if err != nil {
			t.Fatalf("PrepareNodeDebug() error = %v", err)
		}

		runner := runnerWithID(t, "debug-context-run")

		result, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
		if err != nil {
			t.Fatalf("Runner.DebugNode() error = %v", err)
		}

		assertExecutionContext(t, <-identities, workflow.ExecutionContext{
			RunID:        result.RunID,
			DefinitionID: plan.DefinitionID(),
			Revision:     plan.Revision(),
			Address:      workflow.NodeAddress{NodeID: "action"},
			Attempt:      1,
		})
	})

	t.Run("partial run", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		identities := make(chan workflow.ExecutionContext, 1)
		action := executionContextAction("context_partial", stringSchema, identities)
		definition := singleActionDefinition(t, "context_partial", stringSchema, workflow.NodePolicy{})
		definition.ID = "partial-context"
		definition.Revision = "v3"
		plan := compileRoundTrip(t, definition, action)
		runner := runnerWithID(t, "partial-context-run")

		result, err := runner.RunPartial(
			t.Context(),
			plan,
			"action",
			workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
		)
		if err != nil {
			t.Fatalf("Runner.RunPartial() error = %v", err)
		}

		assertExecutionContext(t, <-identities, workflow.ExecutionContext{
			RunID:        result.RunID,
			DefinitionID: plan.DefinitionID(),
			Revision:     plan.Revision(),
			Address:      workflow.NodeAddress{NodeID: "action"},
			Attempt:      1,
		})
	})
}

func TestExecutionContextConcurrentRunsAreIsolated(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"context_concurrent",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			identity, ok := workflow.GetExecutionContext(ctx)
			if !ok {
				return workflow.ActionOutput{}, errors.New("execution context is unavailable")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(identity.RunID),
			}}, nil
		},
	}
	definition := singleActionDefinition(t, "context_concurrent", stringSchema, workflow.NodePolicy{})
	plan := compileRoundTrip(t, definition, action)

	var next atomic.Int32

	runner, err := workflow.NewRunner(workflow.WithRunIDSource(func(time.Time) (string, error) {
		return fmt.Sprintf("concurrent-context-%d", next.Add(1)), nil
	}))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	const runs = 24

	errorsChannel := make(chan error, runs)

	var wait sync.WaitGroup
	for range runs {
		wait.Go(func() {
			result, runErr := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			if runErr != nil {
				errorsChannel <- runErr

				return
			}

			value, decodeErr := workflow.DecodeValue[string](result.Outputs["result"])
			if decodeErr != nil {
				errorsChannel <- decodeErr

				return
			}

			if value != result.RunID {
				errorsChannel <- fmt.Errorf("Action RunID %q != result RunID %q", value, result.RunID)

				return
			}

			errorsChannel <- nil
		})
	}

	wait.Wait()

	for range runs {
		if runErr := <-errorsChannel; runErr != nil {
			t.Fatal(runErr)
		}
	}
}

type executionContextNodeType struct {
	identities chan<- workflow.ExecutionContext
}

func (n executionContextNodeType) Spec() workflow.NodeTypeSpec {
	return workflow.NodeTypeSpec{
		Key: "execution_context_test", Version: "v1", DisplayName: "Execution Context Test",
	}
}

func (n executionContextNodeType) Compile(
	context.Context,
	workflow.CompileContext,
	workflow.NodeDefinition,
) (workflow.CompiledNode, error) {
	return executionContextCompiledNode(n), nil
}

type executionContextCompiledNode struct {
	identities chan<- workflow.ExecutionContext
}

func (n executionContextCompiledNode) Spec() workflow.NodeSpec {
	return workflow.NodeSpec{
		Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.PortSchema{},
		Routes: []string{workflow.RouteSuccess},
	}
}

func (n executionContextCompiledNode) Invoke(
	ctx context.Context,
	_ workflow.NodeInput,
) (workflow.NodeOutput, error) {
	identity, ok := workflow.GetExecutionContext(ctx)
	if !ok {
		return workflow.NodeOutput{}, errors.New("execution context is unavailable")
	}

	n.identities <- identity

	return workflow.NodeOutput{Values: map[string]workflow.Value{}, Route: workflow.RouteSuccess}, nil
}

func executionContextAction(
	key workflow.ActionKey,
	schema workflow.PortSchema,
	identities chan<- workflow.ExecutionContext,
) *fakeAction {
	return &fakeAction{
		spec: actionSpec(
			key,
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": schema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			identity, ok := workflow.GetExecutionContext(ctx)
			if !ok {
				return workflow.ActionOutput{}, errors.New("execution context is unavailable")
			}

			identities <- identity

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("ok"),
			}}, nil
		},
	}
}

func executionContextLoopBody(
	t *testing.T,
	integerSchema workflow.PortSchema,
	action workflow.ActionKey,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "context-loop-body", Revision: "v1",
		Name: "Context Loop Body", Inputs: map[string]workflow.WorkflowInput{"index": {Schema: integerSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(integerSchema, "work", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, action),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
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

func executionContextEmptyActionDefinition(
	t *testing.T,
	id workflow.DefinitionID,
	action workflow.ActionKey,
) workflow.Definition {
	t.Helper()

	return workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: id, Revision: "v1", Name: string(id),
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
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
		Limits: workflow.DefaultLimits(),
	}
}

func runnerWithID(t *testing.T, runID string) *workflow.Runner {
	t.Helper()

	runner, err := workflow.NewRunner(workflow.WithRunIDSource(
		func(time.Time) (string, error) { return runID, nil },
	))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	return runner
}

func assertExecutionContext(
	t *testing.T,
	actual workflow.ExecutionContext,
	expected workflow.ExecutionContext,
) {
	t.Helper()

	if actual.RunID != expected.RunID || actual.DefinitionID != expected.DefinitionID ||
		actual.Revision != expected.Revision || actual.Address.NodeID != expected.Address.NodeID ||
		actual.Attempt != expected.Attempt ||
		!slices.Equal(actual.Address.Scope, expected.Address.Scope) {
		t.Fatalf("ExecutionContext = %#v, want %#v", actual, expected)
	}
}

func assertExecutionContextMatchesEvent(
	t *testing.T,
	identity workflow.ExecutionContext,
	event workflow.Event,
) {
	t.Helper()

	if event.Type() != workflow.EventNodeStarted || identity.RunID != event.RunID ||
		identity.DefinitionID != event.DefinitionID || identity.Revision != event.Revision ||
		identity.Address.NodeID != event.NodeID || identity.Attempt != event.Attempt ||
		!slices.Equal(identity.Address.Scope, event.Scope()) {
		t.Fatalf("ExecutionContext = %#v, NodeStarted = %#v", identity, event)
	}
}
