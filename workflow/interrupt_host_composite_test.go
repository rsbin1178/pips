package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestHostInterruptInsideSubWorkflowRestoresLeaf(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"host_sub_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "host_sub_action")
	definition := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithDefinitionResolver(staticResolver(child)),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	runner := hostInterruptRunner(t, "host-sub")
	interrupted := interruptRunAfterStarted(
		t,
		runner,
		plan,
		"host-sub",
		map[string]workflow.Value{"value": workflow.MustValueOf("kept")},
		started,
	)
	assertSingleRerunAddress(
		t,
		interrupted,
		"child_action",
		workflow.ScopeFrame{Kind: workflow.ScopeSubWorkflow, NodeID: "sub", Index: -1},
	)

	completed, err := runner.Resume(t.Context(), plan, "host-sub", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["result"].String() != `"kept"` || calls.Load() != 2 {
		t.Fatalf(
			"SubWorkflow Resume() result = %#v, error = %v, calls = %d",
			completed,
			err,
			calls.Load(),
		)
	}
}

func TestHostInterruptInsideBatchRestoresExactItem(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
	started := make(chan struct{})

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"host_batch_action",
			map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "host_batch_action", false)
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

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	runner := hostInterruptRunner(t, "host-batch")
	interrupted := interruptRunAfterStarted(
		t,
		runner,
		plan,
		"host-batch",
		map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"kept"}),
		},
		started,
	)
	assertSingleRerunAddress(
		t,
		interrupted,
		"map",
		workflow.ScopeFrame{Kind: workflow.ScopeBatchItem, NodeID: "batch", Index: 0},
	)

	completed, err := runner.Resume(t.Context(), plan, "host-batch", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["results"].String() != `["kept"]` || calls.Load() != 2 {
		t.Fatalf(
			"Batch Resume() result = %#v, error = %v, calls = %d",
			completed,
			err,
			calls.Load(),
		)
	}
}

func TestHostInterruptInsideLoopRestoresExactIteration(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
	started := make(chan struct{})

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"host_loop_action",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}
	body := singleActionLoopBody(t, integerSchema, "host_loop_action", "work")
	definition := loopParentDefinition(
		t,
		"host-loop",
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

	runner := hostInterruptRunner(t, "host-loop")
	interrupted := interruptRunAfterStarted(
		t,
		runner,
		plan,
		"host-loop",
		map[string]workflow.Value{"count": workflow.MustValueOf(1)},
		started,
	)
	assertSingleRerunAddress(
		t,
		interrupted,
		"work",
		workflow.ScopeFrame{Kind: workflow.ScopeLoopIteration, NodeID: "loop", Index: 0},
	)

	completed, err := runner.Resume(t.Context(), plan, "host-loop", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf(
			"Loop Resume() result = %#v, error = %v, calls = %d",
			completed,
			err,
			calls.Load(),
		)
	}
}

func TestHostInterruptDuringRetryWinsNodeTimeoutRace(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})

	var attempts atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"host_retry_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			switch attempts.Add(1) {
			case 1:
				return workflow.ActionOutput{}, errors.New("retry once")
			case 2:
				close(started)
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			default:
				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf("ok"),
				}}, nil
			}
		},
	}
	policy := workflow.NodePolicy{
		TimeoutMilli: 1_000,
		Retry:        workflow.RetryPolicy{MaxAttempts: 2},
	}
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "host_retry_action", stringSchema, policy),
		action,
	)
	runner := hostInterruptRunner(t, "host-retry")
	interrupted := interruptRunAfterStarted(
		t,
		runner,
		plan,
		"host-retry",
		map[string]workflow.Value{},
		started,
	)

	if interrupted.Nodes["action"].Failure == workflow.FailureTimeout || attempts.Load() != 2 {
		t.Fatalf(
			"interrupted retry result = %#v, attempts = %d",
			interrupted.Nodes["action"],
			attempts.Load(),
		)
	}

	completed, err := runner.Resume(t.Context(), plan, "host-retry", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Nodes["action"].Attempts != 3 || attempts.Load() != 3 {
		t.Fatalf(
			"retry Resume() result = %#v, error = %v, attempts = %d",
			completed,
			err,
			attempts.Load(),
		)
	}
}

func TestHostInterruptParallelFrontierKeepsSettledSibling(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	leftStarted := make(chan struct{})
	rightStarted := make(chan struct{})
	releaseLeft := make(chan struct{})

	var (
		leftCalls  atomic.Int32
		rightCalls atomic.Int32
	)

	left := &fakeAction{
		spec: actionSpec(
			"left",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			leftCalls.Add(1)
			close(leftStarted)

			select {
			case <-ctx.Done():
				return workflow.ActionOutput{}, ctx.Err()
			case <-releaseLeft:
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("left"),
			}}, nil
		},
	}
	right := &fakeAction{
		spec: actionSpec(
			"right",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			if rightCalls.Add(1) == 1 {
				close(rightStarted)
				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("right"),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(left, right)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(t.Context(), parallelInterruptDefinition(t, stringSchema), registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	runner := hostInterruptRunner(t, "host-parallel")
	ctx, interrupt := workflow.WithRunInterrupt(t.Context())

	type outcome struct {
		result workflow.RunResult
		err    error
	}

	done := make(chan outcome, 1)

	go func() {
		result, runErr := runner.Run(ctx, plan, map[string]workflow.Value{})
		done <- outcome{result: result, err: runErr}
	}()

	for name, started := range map[string]<-chan struct{}{
		"left": leftStarted, "right": rightStarted,
	} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s Action did not start", name)
		}
	}

	interrupt(workflow.WithRunInterruptTimeout(100 * time.Millisecond))
	close(releaseLeft)

	var interrupted outcome

	select {
	case interrupted = <-done:
	case <-time.After(time.Second):
		t.Fatal("parallel Run did not stop after grace timeout")
	}

	assertInterrupted(t, interrupted.result, interrupted.err, "host-parallel")

	if !containsNodeAddress(interrupted.result.Interruption.AfterNodes, "left") ||
		!containsNodeAddress(interrupted.result.Interruption.RerunNodes, "right") ||
		leftCalls.Load() != 1 || rightCalls.Load() != 1 {
		t.Fatalf(
			"parallel interruption = %#v, calls = %d/%d",
			interrupted.result.Interruption,
			leftCalls.Load(),
			rightCalls.Load(),
		)
	}

	completed, err := runner.Resume(t.Context(), plan, "host-parallel", nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded ||
		completed.Outputs["left"].String() != `"left"` ||
		completed.Outputs["right"].String() != `"right"` ||
		leftCalls.Load() != 1 || rightCalls.Load() != 2 {
		t.Fatalf(
			"parallel Resume() result = %#v, error = %v, calls = %d/%d",
			completed,
			err,
			leftCalls.Load(),
			rightCalls.Load(),
		)
	}
}

func TestHostInterruptSignalsAreIndependentAcrossConcurrentRuns(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{}, 2)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"host_concurrent_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			if calls.Add(1) <= 2 {
				started <- struct{}{}

				<-ctx.Done()

				return workflow.ActionOutput{}, ctx.Err()
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("ok"),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		dynamicSingleActionDefinition(t, "host-concurrent", "host_concurrent_action", stringSchema),
		registry,
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	var runIDs atomic.Int32

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(&memoryCheckpointStore{}),
		workflow.WithRunIDSource(func(time.Time) (string, error) {
			return fmt.Sprintf("host-concurrent-%d", runIDs.Add(1)), nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	type outcome struct {
		result workflow.RunResult
		err    error
	}

	done := make(chan outcome, 2)
	interrupts := make([]func(...workflow.RunInterruptOption), 2)

	for index := range interrupts {
		ctx, interrupt := workflow.WithRunInterrupt(t.Context())
		interrupts[index] = interrupt

		go func() {
			result, runErr := runner.Run(ctx, plan, map[string]workflow.Value{})
			done <- outcome{result: result, err: runErr}
		}()
	}

	for range interrupts {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent Action did not start")
		}
	}

	for _, interrupt := range interrupts {
		interrupt(workflow.WithRunInterruptTimeout(0))
	}

	interrupted := make([]workflow.RunResult, 0, len(interrupts))

	for range interrupts {
		select {
		case current := <-done:
			assertInterrupted(t, current.result, current.err, current.result.RunID)
			interrupted = append(interrupted, current.result)
		case <-time.After(time.Second):
			t.Fatal("concurrent Run did not stop after host interruption")
		}
	}

	if interrupted[0].RunID == interrupted[1].RunID {
		t.Fatalf("concurrent Runs reused Run ID %q", interrupted[0].RunID)
	}

	for _, current := range interrupted {
		completed, resumeErr := runner.Resume(t.Context(), plan, current.RunID, nil)
		if resumeErr != nil || completed.Status != workflow.RunStatusSucceeded {
			t.Fatalf(
				"Resume(%q) result = %#v, error = %v",
				current.RunID,
				completed,
				resumeErr,
			)
		}
	}

	if calls.Load() != 4 {
		t.Fatalf("concurrent Action calls = %d, want 4", calls.Load())
	}
}

func hostInterruptRunner(t *testing.T, runID string) *workflow.Runner {
	t.Helper()

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(&memoryCheckpointStore{}),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return runID, nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	return runner
}

func interruptRunAfterStarted(
	t *testing.T,
	runner *workflow.Runner,
	plan *workflow.Plan,
	runID string,
	inputs map[string]workflow.Value,
	started <-chan struct{},
) workflow.RunResult {
	t.Helper()

	ctx, interrupt := workflow.WithRunInterrupt(t.Context())

	type outcome struct {
		result workflow.RunResult
		err    error
	}

	done := make(chan outcome, 1)

	go func() {
		result, err := runner.Run(ctx, plan, inputs)
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Action did not start")
	}

	interrupt(workflow.WithRunInterruptTimeout(0))

	select {
	case result := <-done:
		assertInterrupted(t, result.result, result.err, runID)

		return result.result
	case <-time.After(time.Second):
		t.Fatal("Runner.Run() did not stop after host interruption")

		return workflow.RunResult{}
	}
}

func assertSingleRerunAddress(
	t *testing.T,
	result workflow.RunResult,
	nodeID workflow.NodeID,
	frame workflow.ScopeFrame,
) {
	t.Helper()

	if result.Interruption == nil || len(result.Interruption.RerunNodes) != 1 {
		t.Fatalf("rerun nodes = %#v, want one", result.Interruption)
	}

	address := result.Interruption.RerunNodes[0]
	if address.NodeID != nodeID || len(address.Scope) != 1 || address.Scope[0] != frame {
		t.Fatalf("rerun address = %#v, want %s under %#v", address, nodeID, frame)
	}
}

func containsNodeAddress(addresses []workflow.NodeAddress, nodeID workflow.NodeID) bool {
	for _, address := range addresses {
		if address.NodeID == nodeID && len(address.Scope) == 0 {
			return true
		}
	}

	return false
}
