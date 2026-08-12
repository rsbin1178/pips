package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/workflow"
)

func TestHostInterruptBeforeFirstNodeResumesNormally(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	definition, registry := staticInterruptFixture(t, &calls)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "host-before", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ctx, interrupt := workflow.WithRunInterrupt(t.Context())
	interrupt()

	result, err := runner.Run(ctx, plan, map[string]workflow.Value{})
	assertInterrupted(t, result, err, "host-before")

	if calls.Load() != 0 || len(result.Interruption.RerunNodes) != 1 ||
		result.Interruption.RerunNodes[0].NodeID != "start" {
		t.Fatalf("host-before result = %#v, calls = %d", result, calls.Load())
	}

	resumed, err := runner.Resume(t.Context(), plan, "host-before", nil)
	if err != nil || resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed result = %#v, error = %v, calls = %d", resumed, err, calls.Load())
	}
}

func TestHostInterruptDefaultWaitsAndDoesNotRerunSettledNode(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	var calls atomic.Int32

	action, definition, registry := blockingHostInterruptFixture(
		t,
		"host_wait_action",
		func(ctx context.Context, call int32) error {
			if call == 1 {
				close(started)

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
				}
			}

			return nil
		},
		&calls,
	)
	_ = action

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "host-wait", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

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

	<-started
	interrupt()

	select {
	case premature := <-done:
		t.Fatalf("Run returned before settled node: %#v", premature)
	case <-time.After(10 * time.Millisecond):
	}

	close(release)

	interrupted := <-done
	assertInterrupted(t, interrupted.result, interrupted.err, "host-wait")

	if len(interrupted.result.Interruption.AfterNodes) != 1 ||
		interrupted.result.Interruption.AfterNodes[0].NodeID != "work" {
		t.Fatalf("host settled info = %#v", interrupted.result.Interruption)
	}

	resumed, err := runner.Resume(t.Context(), plan, "host-wait", nil)
	if err != nil || resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed result = %#v, error = %v, calls = %d", resumed, err, calls.Load())
	}
}

func TestHostInterruptImmediateRerunsCanceledInvocation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	var calls atomic.Int32

	_, definition, registry := blockingHostInterruptFixture(
		t,
		"host_immediate_action",
		func(ctx context.Context, call int32) error {
			if call != 1 {
				return nil
			}

			close(started)
			<-ctx.Done()

			return ctx.Err()
		},
		&calls,
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "host-immediate", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

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

	<-started
	interrupt(workflow.WithRunInterruptTimeout(0))

	interrupted := <-done
	assertInterrupted(t, interrupted.result, interrupted.err, "host-immediate")

	if len(interrupted.result.Interruption.RerunNodes) != 1 ||
		interrupted.result.Interruption.RerunNodes[0].NodeID != "work" {
		t.Fatalf("host rerun info = %#v", interrupted.result.Interruption)
	}

	resumed, err := runner.Resume(t.Context(), plan, "host-immediate", nil)
	if err != nil || resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf("resumed result = %#v, error = %v, calls = %d", resumed, err, calls.Load())
	}
}

func TestHostInterruptPositiveGraceCancelsAtDeadline(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	var calls atomic.Int32

	_, definition, registry := blockingHostInterruptFixture(
		t,
		"host_grace_action",
		func(ctx context.Context, call int32) error {
			if call != 1 {
				return nil
			}

			close(started)
			<-ctx.Done()

			return ctx.Err()
		},
		&calls,
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "host-grace", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ctx, interrupt := workflow.WithRunInterrupt(t.Context())
	done := make(chan error, 1)

	go func() {
		_, runErr := runner.Run(ctx, plan, map[string]workflow.Value{})
		done <- runErr
	}()

	<-started
	interrupt(workflow.WithRunInterruptTimeout(20 * time.Millisecond))

	select {
	case err := <-done:
		t.Fatalf("Run returned before positive grace elapsed: %v", err)
	case <-time.After(5 * time.Millisecond):
	}

	if err := <-done; !errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Run after positive grace error = %v", err)
	}

	resumed, err := runner.Resume(t.Context(), plan, "host-grace", nil)
	if err != nil || resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf("grace resumed result = %#v, error = %v, calls = %d", resumed, err, calls.Load())
	}
}

func TestContextCancellationDoesNotCreateResumableCheckpoint(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	var calls atomic.Int32

	_, definition, registry := blockingHostInterruptFixture(
		t,
		"ordinary_cancel_action",
		func(ctx context.Context, call int32) error {
			if call == 1 {
				close(started)
			}

			<-ctx.Done()

			return ctx.Err()
		},
		&calls,
	)

	plan, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "ordinary-cancel", nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		_, runErr := runner.Run(ctx, plan, map[string]workflow.Value{})
		done <- runErr
	}()

	<-started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled only", err)
	}

	if _, found, err := store.Get(t.Context(), "ordinary-cancel"); err != nil || found {
		t.Fatalf("checkpoint after cancellation: found=%v error=%v", found, err)
	}
}

func blockingHostInterruptFixture(
	t *testing.T,
	key workflow.ActionKey,
	block func(context.Context, int32) error,
	calls *atomic.Int32,
) (*fakeAction, workflow.Definition, *workflow.Registry) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			key,
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			call := calls.Add(1)
			if err := block(ctx, call); err != nil {
				return workflow.ActionOutput{}, err
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

	return action, dynamicSingleActionDefinition(t, "host-interrupt", key, stringSchema), registry
}
