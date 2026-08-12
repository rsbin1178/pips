package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

type nodeExecutionLog struct {
	mu      sync.Mutex
	records []workflow.NodeExecution
}

func (l *nodeExecutionLog) record(_ context.Context, record workflow.NodeExecution) {
	l.mu.Lock()
	l.records = append(l.records, record)
	l.mu.Unlock()
}

func (l *nodeExecutionLog) snapshot() []workflow.NodeExecution {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]workflow.NodeExecution(nil), l.records...)
}

func TestWithNodeExecutionRecorderRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(nil))
	if err == nil {
		t.Fatal("NewRunner() accepted a nil NodeExecutionRecorder")
	}
}

func TestNodeExecutionRejectsWireFormat(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(workflow.NodeExecution{})
	if !errors.Is(err, workflow.ErrNodeExecutionWireFormat) {
		t.Fatalf("json.Marshal() error = %v, want ErrNodeExecutionWireFormat", err)
	}

	var record workflow.NodeExecution
	if err := json.Unmarshal([]byte(`{}`), &record); !errors.Is(err, workflow.ErrNodeExecutionWireFormat) {
		t.Fatalf("json.Unmarshal() error = %v, want ErrNodeExecutionWireFormat", err)
	}
}

func TestNodeExecutionRecorderRecordsDetachedSuccessfulLifecycle(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"node_execution_success",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			value, err := workflow.DecodeValue[string](input.Values["value"])
			if err != nil {
				return workflow.ActionOutput{}, err
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done:" + value),
			}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "record-success", Revision: "v7", Name: "Record Success",
		Inputs: map[string]workflow.WorkflowInput{"value": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "action", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "node_execution_success"),
				Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, action)
	log := &nodeExecutionLog{}

	var ticks atomic.Int64

	base := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "record-run", nil }),
		workflow.WithClock(func() time.Time {
			return base.Add(time.Duration(ticks.Add(1)) * time.Second)
		}),
		workflow.WithNodeExecutionRecorder(func(ctx context.Context, record workflow.NodeExecution) {
			if ctx.Err() != nil {
				t.Errorf("recorder context error = %v", ctx.Err())
			}

			if record.Address.NodeID == "action" && record.NodeRun.Status == workflow.NodeStatusRunning {
				delete(record.Inputs, "value")
				record.Outputs["poison"] = workflow.MustValueOf("mutated")
				record.Address.Scope = append(record.Address.Scope, workflow.ScopeFrame{
					Kind: workflow.ScopeBatchItem, NodeID: "poison", Index: 99,
				})
			}

			log.record(ctx, record)
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if got := result.Outputs["result"].String(); got != `"done:input"` {
		t.Fatalf("result = %s, want done:input", got)
	}

	records := log.snapshot()
	if len(records) != 6 {
		t.Fatalf("record count = %d, want 6: %#v", len(records), records)
	}

	for _, nodeID := range []workflow.NodeID{"start", "action", "end"} {
		nodeRecords := nodeExecutionRecords(records, nodeID)
		if len(nodeRecords) != 2 || nodeRecords[0].NodeRun.Status != workflow.NodeStatusRunning ||
			nodeRecords[1].NodeRun.Status != workflow.NodeStatusSucceeded {
			t.Fatalf("records for %q = %#v", nodeID, nodeRecords)
		}

		if nodeRecords[0].NodeRun.Attempts != 1 || nodeRecords[1].NodeRun.Attempts != 1 ||
			nodeRecords[0].NodeRun.StartedAt.IsZero() || !nodeRecords[1].NodeRun.StartedAt.Equal(nodeRecords[0].NodeRun.StartedAt) ||
			!nodeRecords[0].NodeRun.EndedAt.IsZero() || nodeRecords[1].NodeRun.EndedAt.IsZero() {
			t.Fatalf("timing/attempts for %q = %#v", nodeID, nodeRecords)
		}
	}

	actionRecords := nodeExecutionRecords(records, "action")

	terminal := actionRecords[1]
	if terminal.DefinitionID != definition.ID || terminal.Revision != definition.Revision ||
		terminal.DefinitionFingerprint != plan.DefinitionFingerprint() ||
		terminal.PlanFingerprint != plan.Fingerprint() || terminal.RunID != "record-run" ||
		len(terminal.Address.Scope) != 0 || terminal.Route != workflow.RouteSuccess ||
		terminal.ErrorMessage != "" || terminal.Inputs["value"].String() != `"input"` ||
		terminal.Outputs["result"].String() != `"done:input"` {
		t.Fatalf("terminal action record = %#v", terminal)
	}
}

func TestNodeExecutionRecorderUsesEffectiveErrorPolicyOutcome(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	t.Run("stop after retry", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32

		action := resultAction("record-stop", stringSchema, func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, fmt.Errorf("attempt %d failed", attempts.Add(1))
		})
		policy := workflow.NodePolicy{Retry: workflow.RetryPolicy{MaxAttempts: 2}}
		plan := compileRoundTrip(t, singleActionDefinition(t, "record-stop", stringSchema, policy), action)

		records, _, err := runWithNodeExecutionRecords(t, plan)
		if !errors.Is(err, workflow.ErrRun) {
			t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
		}

		actionRecords := nodeExecutionRecords(records, "action")
		if len(actionRecords) != 2 {
			t.Fatalf("retry created %d logical snapshots, want 2: %#v", len(actionRecords), actionRecords)
		}

		terminal := actionRecords[1]
		if terminal.NodeRun.Status != workflow.NodeStatusFailed || terminal.NodeRun.Attempts != 2 ||
			terminal.NodeRun.Failure != workflow.FailureError || terminal.Route != "" ||
			len(terminal.Outputs) != 0 || terminal.ErrorMessage != "attempt 2 failed" {
			t.Fatalf("stop record = %#v", terminal)
		}
	})

	t.Run("continue with default", func(t *testing.T) {
		t.Parallel()

		action := resultAction("record-default", stringSchema, func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("use fallback")
		})
		policy := workflow.NodePolicy{
			Error: workflow.ErrorContinueWithDefault,
			DefaultOutputs: map[string]workflow.Value{
				"result": workflow.MustValueOf("safe"),
			},
		}
		plan := compileRoundTrip(t, singleActionDefinition(t, "record-default", stringSchema, policy), action)

		records, result, err := runWithNodeExecutionRecords(t, plan)
		if err != nil || result.Status != workflow.RunStatusPartialSucceeded {
			t.Fatalf("Runner.Run() = (%#v, %v)", result, err)
		}

		terminal := lastNodeExecutionRecord(t, records, "action")
		if terminal.NodeRun.Status != workflow.NodeStatusException ||
			terminal.NodeRun.Failure != workflow.FailureError || terminal.Route != workflow.RouteSuccess ||
			terminal.Outputs["result"].String() != `"safe"` || terminal.ErrorMessage != "use fallback" {
			t.Fatalf("default record = %#v", terminal)
		}
	})

	t.Run("route error", func(t *testing.T) {
		t.Parallel()

		errorTypeSchema := mustSchema(
			t,
			`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
		)
		failing := resultAction("typed_failure", stringSchema, func(context.Context) (workflow.Value, error) {
			return workflow.Value{}, errors.New("route failure")
		})
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
			failureBranchDefinition(t, stringSchema, 1),
			failing,
			constantAction("typed_success", "unexpected", stringSchema),
			handler,
		)

		records, result, err := runWithNodeExecutionRecords(t, plan)
		if err != nil || result.Status != workflow.RunStatusPartialSucceeded {
			t.Fatalf("Runner.Run() = (%#v, %v)", result, err)
		}

		terminal := lastNodeExecutionRecord(t, records, "unreliable")
		if terminal.NodeRun.Status != workflow.NodeStatusException ||
			terminal.NodeRun.Failure != workflow.FailureError || terminal.Route != workflow.RouteError ||
			len(terminal.Outputs) != 0 || terminal.ErrorMessage != "route failure" {
			t.Fatalf("route-error record = %#v", terminal)
		}

		if skipped := nodeExecutionRecords(records, "success"); len(skipped) != 0 {
			t.Fatalf("skipped success branch created records: %#v", skipped)
		}
	})
}

func TestNodeExecutionRecorderClassifiesTerminalFailures(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	tests := []struct {
		name        string
		action      *fakeAction
		policy      workflow.NodePolicy
		wantFailure workflow.FailureKind
		wantError   error
	}{
		{
			name: "panic",
			action: resultAction("record-panic", stringSchema, func(context.Context) (workflow.Value, error) {
				panic("record panic")
			}),
			wantFailure: workflow.FailurePanic,
			wantError:   workflow.ErrRun,
		},
		{
			name: "timeout",
			action: resultAction("record-timeout", stringSchema, func(ctx context.Context) (workflow.Value, error) {
				<-ctx.Done()

				return workflow.Value{}, ctx.Err()
			}),
			policy:      workflow.NodePolicy{TimeoutMilli: 1},
			wantFailure: workflow.FailureTimeout,
			wantError:   context.DeadlineExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan := compileRoundTrip(
				t,
				singleActionDefinition(t, test.action.Spec().Key, stringSchema, test.policy),
				test.action,
			)

			records, _, err := runWithNodeExecutionRecords(t, plan)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Runner.Run() error = %v, want %v", err, test.wantError)
			}

			terminal := lastNodeExecutionRecord(t, records, "action")
			if terminal.NodeRun.Status != workflow.NodeStatusFailed ||
				terminal.NodeRun.Failure != test.wantFailure || terminal.ErrorMessage == "" ||
				len(terminal.Outputs) != 0 || terminal.Route != "" {
				t.Fatalf("terminal record = %#v", terminal)
			}
		})
	}
}

func TestNodeExecutionRecorderResumeUpsertsInterruptedInvocation(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"record-interrupt",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			interrupted, _, _ := workflow.GetInterruptState(ctx)

			isTarget, hasData, data := workflow.GetResumeContext(ctx)
			if !interrupted {
				return workflow.ActionOutput{}, workflow.StatefulInterrupt(
					ctx,
					workflow.MustValueOf("question"),
					workflow.MustValueOf("waiting"),
				)
			}

			if !isTarget || !hasData {
				return workflow.ActionOutput{}, errors.New("resume data is missing")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{"result": data}}, nil
		},
	}
	definition := dynamicSingleActionDefinition(t, "record-resume", "record-interrupt", stringSchema)
	plan := compileRoundTrip(t, definition, action)
	store := &memoryCheckpointStore{}
	firstLog := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "record-resume-run", nil }),
		workflow.WithNodeExecutionRecorder(firstLog.record),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "record-resume-run")

	firstActionRecords := nodeExecutionRecords(firstLog.snapshot(), "work")
	if len(firstActionRecords) != 2 ||
		firstActionRecords[0].NodeRun.Status != workflow.NodeStatusRunning ||
		firstActionRecords[1].NodeRun.Status != workflow.NodeStatusInterrupted ||
		!firstActionRecords[1].NodeRun.StartedAt.Equal(firstActionRecords[0].NodeRun.StartedAt) {
		t.Fatalf("interrupted records = %#v", firstActionRecords)
	}

	resumeLog := &nodeExecutionLog{}

	resumeRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithNodeExecutionRecorder(resumeLog.record),
	)
	if err != nil {
		t.Fatalf("NewRunner(resume) error = %v", err)
	}

	resumeTarget := interrupted.Interruption.Contexts[0]

	completed, err := resumeRunner.Resume(
		t.Context(),
		plan,
		interrupted.RunID,
		[]workflow.ResumeTarget{{
			InterruptID: resumeTarget.ID,
			Data:        workflow.MustValueOf("Ada"),
		}},
	)
	if err != nil || completed.Status != workflow.RunStatusSucceeded {
		t.Fatalf("Runner.Resume() = (%#v, %v)", completed, err)
	}

	resumeActionRecords := nodeExecutionRecords(resumeLog.snapshot(), "work")
	if len(resumeActionRecords) != 1 {
		t.Fatalf("resume action records = %#v, want one terminal upsert", resumeActionRecords)
	}

	terminal := resumeActionRecords[0]
	if terminal.NodeRun.Status != workflow.NodeStatusSucceeded || terminal.NodeRun.Attempts != 2 ||
		!terminal.NodeRun.StartedAt.Equal(firstActionRecords[0].NodeRun.StartedAt) ||
		terminal.Outputs["result"].String() != `"Ada"` || calls.Load() != 2 {
		t.Fatalf("resume terminal = %#v, calls = %d", terminal, calls.Load())
	}
}

func TestNodeExecutionRecorderStaticInterruptCoverage(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("record-static", "ok", stringSchema)
	definition := singleActionDefinition(t, "record-static", stringSchema, workflow.NodePolicy{})

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	store := &memoryCheckpointStore{}
	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "record-static-run", nil }),
		workflow.WithNodeExecutionRecorder(log.record),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	before, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, before, err, "record-static-run")

	if records := nodeExecutionRecords(log.snapshot(), "action"); len(records) != 0 {
		t.Fatalf("static-before created records: %#v", records)
	}

	after, err := runner.Resume(t.Context(), plan, before.RunID, nil)
	assertInterrupted(t, after, err, "record-static-run")

	actionRecords := nodeExecutionRecords(log.snapshot(), "action")
	if len(actionRecords) != 2 || actionRecords[0].NodeRun.Status != workflow.NodeStatusRunning ||
		actionRecords[1].NodeRun.Status != workflow.NodeStatusSucceeded {
		t.Fatalf("static-after action records = %#v", actionRecords)
	}

	completed, err := runner.Resume(t.Context(), plan, after.RunID, nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded {
		t.Fatalf("final Resume() = (%#v, %v)", completed, err)
	}

	if records := nodeExecutionRecords(log.snapshot(), "action"); len(records) != 2 {
		t.Fatalf("final Resume duplicated settled action records: %#v", records)
	}
}

func TestNodeExecutionRecorderHostRerunPreservesLogicalStart(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})

	var calls atomic.Int32

	action := resultAction("record-host-rerun", stringSchema, func(ctx context.Context) (workflow.Value, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()

			return workflow.Value{}, ctx.Err()
		}

		return workflow.MustValueOf("ok"), nil
	})
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "record-host-rerun", stringSchema, workflow.NodePolicy{}),
		action,
	)
	store := &memoryCheckpointStore{}
	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "record-host-run", nil }),
		workflow.WithNodeExecutionRecorder(log.record),
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
	assertInterrupted(t, interrupted.result, interrupted.err, "record-host-run")

	actionRecords := nodeExecutionRecords(log.snapshot(), "action")
	if len(actionRecords) != 2 ||
		actionRecords[0].NodeRun.Status != workflow.NodeStatusRunning ||
		actionRecords[1].NodeRun.Status != workflow.NodeStatusInterrupted ||
		actionRecords[1].NodeRun.Attempts != 1 {
		t.Fatalf("host-rerun interruption records = %#v", actionRecords)
	}

	completed, err := runner.Resume(t.Context(), plan, interrupted.result.RunID, nil)
	if err != nil || completed.Status != workflow.RunStatusSucceeded {
		t.Fatalf("Runner.Resume() = (%#v, %v)", completed, err)
	}

	actionRecords = nodeExecutionRecords(log.snapshot(), "action")
	if len(actionRecords) != 3 {
		t.Fatalf("host-rerun Resume records = %#v, want running/interrupted/terminal", actionRecords)
	}

	terminal := actionRecords[2]
	if terminal.NodeRun.Status != workflow.NodeStatusSucceeded || terminal.NodeRun.Attempts != 2 ||
		!terminal.NodeRun.StartedAt.Equal(actionRecords[0].NodeRun.StartedAt) ||
		terminal.Outputs["result"].String() != `"ok"` || calls.Load() != 2 {
		t.Fatalf("host-rerun terminal = %#v, calls = %d", terminal, calls.Load())
	}
}

type nodeExecutionContextKey struct{}

func TestNodeExecutionRecorderCancellationContextAndPreInvocationOmission(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan struct{})
	action := resultAction("record-cancel", stringSchema, func(ctx context.Context) (workflow.Value, error) {
		close(started)
		<-ctx.Done()

		return workflow.Value{}, ctx.Err()
	})
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "record-cancel", stringSchema, workflow.NodePolicy{}),
		action,
	)

	var terminalContextOK atomic.Bool

	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithNodeExecutionRecorder(func(ctx context.Context, record workflow.NodeExecution) {
			if record.Address.NodeID == "action" && record.NodeRun.Status == workflow.NodeStatusFailed {
				_, hasDeadline := ctx.Deadline()
				terminalContextOK.Store(
					ctx.Err() == nil && !hasDeadline && ctx.Value(nodeExecutionContextKey{}) == "request-value",
				)
			}

			log.record(ctx, record)
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	parent := context.WithValue(t.Context(), nodeExecutionContextKey{}, "request-value")
	runCtx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)

	go func() {
		_, runErr := runner.Run(runCtx, plan, map[string]workflow.Value{})
		done <- runErr
	}()

	<-started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Runner.Run() error = %v, want context.Canceled", err)
	}

	terminal := lastNodeExecutionRecord(t, log.snapshot(), "action")
	if !terminalContextOK.Load() || terminal.NodeRun.Status != workflow.NodeStatusFailed ||
		terminal.NodeRun.Failure != workflow.FailureCanceled || terminal.ErrorMessage != context.Canceled.Error() {
		t.Fatalf("canceled terminal/context = %#v/%v", terminal, terminalContextOK.Load())
	}

	limitedDefinition := singleActionDefinition(t, "record-cancel", stringSchema, workflow.NodePolicy{})
	limitedDefinition.Limits.MaxSteps = 1
	limitedPlan := compileRoundTrip(t, limitedDefinition, action)
	limitedLog := &nodeExecutionLog{}

	limitedRunner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(limitedLog.record))
	if err != nil {
		t.Fatalf("NewRunner(limited) error = %v", err)
	}

	_, err = limitedRunner.Run(t.Context(), limitedPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("limited Runner.Run() error = %v, want ErrRun", err)
	}

	if records := nodeExecutionRecords(limitedLog.snapshot(), "action"); len(records) != 0 {
		t.Fatalf("step-limit rejection created records: %#v", records)
	}

	canceledLog := &nodeExecutionLog{}

	canceledRunner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(canceledLog.record))
	if err != nil {
		t.Fatalf("NewRunner(canceled) error = %v", err)
	}

	preCanceled, preCancel := context.WithCancel(t.Context())
	preCancel()

	if _, err := canceledRunner.Run(preCanceled, plan, map[string]workflow.Value{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Runner.Run() error = %v, want context.Canceled", err)
	}

	if records := canceledLog.snapshot(); len(records) != 0 {
		t.Fatalf("pre-canceled Run created records: %#v", records)
	}
}

func TestNodeExecutionRecorderOmitsInputResolutionFailure(t *testing.T) {
	t.Parallel()

	objectSchema := mustSchema(t, `{
		"type":"object",
		"properties":{"name":{"type":"string"},"missing":{"type":"string"}},
		"required":["name"]
	}`)
	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"record-input-failure",
			map[string]workflow.PortSchema{"name": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["name"],
			}}, nil
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "record-input-failure", Revision: "v1", Name: "Record input failure",
		Inputs: map[string]workflow.WorkflowInput{"object": {Schema: objectSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": nodeOutput(stringSchema, "action", "result"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "action", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "record-input-failure"),
				Inputs: map[string]workflow.Binding{"name": {
					Source: workflow.BindingNodeOutput, Node: "start", Port: "object",
					Path: []string{"missing"},
				}},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "action"),
			edge("action", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	plan := compileRoundTrip(t, definition, action)
	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(log.record))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
		"object": workflow.MustValueOf(map[string]any{"name": "present"}),
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.Run() error = %v, want ErrRun", err)
	}

	if records := nodeExecutionRecords(log.snapshot(), "action"); len(records) != 0 {
		t.Fatalf("input-resolution failure created records: %#v", records)
	}
}

func TestNodeExecutionRecorderUsesNestedScopes(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	childAction := &fakeAction{
		spec: actionSpec(
			"record-child",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["value"],
			}}, nil
		},
	}
	child := subWorkflowChildDefinition(stringSchema, "record-child")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))
	plan := compileWithResolver(t, parent, staticResolver(child), childAction)
	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "scoped-run", nil }),
		workflow.WithNodeExecutionRecorder(log.record),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("nested"),
	})
	if err != nil || result.Status != workflow.RunStatusSucceeded {
		t.Fatalf("Runner.Run() = (%#v, %v)", result, err)
	}

	parentRecord := lastNodeExecutionRecord(t, log.snapshot(), "sub")
	if len(parentRecord.Address.Scope) != 0 || parentRecord.DefinitionID != parent.ID {
		t.Fatalf("composite parent record = %#v", parentRecord)
	}

	childRecord := lastNodeExecutionRecord(t, log.snapshot(), "child_action")
	if childRecord.RunID != "scoped-run" || childRecord.DefinitionID != child.ID ||
		len(childRecord.Address.Scope) != 1 ||
		childRecord.Address.Scope[0] != (workflow.ScopeFrame{
			Kind: workflow.ScopeSubWorkflow, NodeID: "sub", Index: -1,
		}) {
		t.Fatalf("child record = %#v", childRecord)
	}
}

func TestNodeExecutionRecorderUsesBatchAndLoopScopes(t *testing.T) {
	t.Parallel()

	t.Run("batch items", func(t *testing.T) {
		t.Parallel()

		stringSchema := mustSchema(t, `{"type":"string"}`)
		integerSchema := mustSchema(t, `{"type":"integer"}`)
		itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)
		action := &fakeAction{
			spec: actionSpec(
				"record-batch-item",
				map[string]workflow.PortSchema{"item": stringSchema, "index": integerSchema},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": input.Values["item"],
				}}, nil
			},
		}
		body := batchBodyDefinition(t, stringSchema, stringSchema, "record-batch-item", false)
		config := workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 10,
		}
		definition := batchParentDefinition(t, itemsSchema, itemsSchema, config, workflow.PortSchema{})
		plan := compileRoundTrip(t, definition, action)
		log := &nodeExecutionLog{}

		runner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(log.record))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		_, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
			"items": workflow.MustValueOf([]string{"a", "b"}),
		})
		if err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		indexes := map[int]bool{}

		for _, record := range log.snapshot() {
			if record.DefinitionID != body.ID || record.Address.NodeID != "map" ||
				record.NodeRun.Status != workflow.NodeStatusSucceeded {
				continue
			}

			if len(record.Address.Scope) != 1 ||
				record.Address.Scope[0].Kind != workflow.ScopeBatchItem ||
				record.Address.Scope[0].NodeID != "batch" {
				t.Fatalf("batch item record = %#v", record)
			}

			indexes[record.Address.Scope[0].Index] = true
		}

		if len(indexes) != 2 || !indexes[0] || !indexes[1] {
			t.Fatalf("batch indexes = %#v, want 0 and 1", indexes)
		}
	})

	t.Run("loop iterations", func(t *testing.T) {
		t.Parallel()

		integerSchema := mustSchema(t, `{"type":"integer"}`)
		countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)
		body := passthroughIndexBody(t, integerSchema)
		config := workflow.LoopConfig{Body: body, Mode: workflow.LoopCount, MaxIterations: 3}
		definition := loopParentDefinition(
			t,
			"record-loop",
			map[string]workflow.PortSchema{"count": countSchema},
			map[string]workflow.Binding{"count": workflowInput("count")},
			map[string]workflow.OutputBinding{},
			config,
		)
		plan := compileRoundTrip(t, definition)
		log := &nodeExecutionLog{}

		runner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(log.record))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		_, err = runner.Run(t.Context(), plan, map[string]workflow.Value{
			"count": workflow.MustValueOf(2),
		})
		if err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}

		indexes := map[int]bool{}

		for _, record := range log.snapshot() {
			if record.DefinitionID != body.ID || record.Address.NodeID != "start" ||
				record.NodeRun.Status != workflow.NodeStatusSucceeded {
				continue
			}

			if len(record.Address.Scope) != 1 ||
				record.Address.Scope[0].Kind != workflow.ScopeLoopIteration ||
				record.Address.Scope[0].NodeID != "loop" {
				t.Fatalf("loop iteration record = %#v", record)
			}

			indexes[record.Address.Scope[0].Index] = true
		}

		if len(indexes) != 2 || !indexes[0] || !indexes[1] {
			t.Fatalf("loop indexes = %#v, want 0 and 1", indexes)
		}
	})
}

func TestNodeExecutionRecorderAppliesBackpressureAndSerializesOneRun(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	started := make(chan workflow.ActionKey, 2)
	action := func(key workflow.ActionKey) *fakeAction {
		return &fakeAction{
			spec: actionSpec(
				key,
				map[string]workflow.PortSchema{},
				map[string]workflow.PortSchema{"result": stringSchema},
			),
			run: func(_ context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
				started <- key

				return workflow.ActionOutput{Values: map[string]workflow.Value{
					"result": workflow.MustValueOf(string(key)),
				}}, nil
			},
		}
	}
	left := action("record-left")
	right := action("record-right")
	mergeConfig := workflow.MergeConfig{
		Mode: workflow.MergeParallel,
		Outputs: map[string]workflow.MergeOutputConfig{
			"left":  {Schema: stringSchema, Sources: []string{"left"}},
			"right": {Schema: stringSchema, Sources: []string{"right"}},
		},
	}
	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "record-serialization", Revision: "v1", Name: "Record Serialization",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"left":  nodeOutput(stringSchema, "merge", "left"),
			"right": nodeOutput(stringSchema, "merge", "right"),
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "left", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "record-left")},
			{ID: "right", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion, Config: actionConfig(t, "record-right")},
			{
				ID: "merge", Type: workflow.NodeTypeMerge, Version: workflow.BuiltinNodeVersion,
				Config: mustJSON(t, mergeConfig),
				Inputs: map[string]workflow.Binding{
					"left": nodeBinding("left", "result"), "right": nodeBinding("right", "result"),
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
	plan := compileRoundTrip(t, definition, left, right)
	entered := make(chan workflow.NodeID, 2)
	release := make(chan struct{}, 2)

	runner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(func(
		_ context.Context,
		record workflow.NodeExecution,
	) {
		if (record.Address.NodeID == "left" || record.Address.NodeID == "right") &&
			record.NodeRun.Status == workflow.NodeStatusRunning {
			entered <- record.Address.NodeID

			<-release
		}
	}))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	done := make(chan error, 1)

	go func() {
		_, runErr := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		done <- runErr
	}()

	first := <-entered
	select {
	case actionKey := <-started:
		t.Fatalf("Action %q started before its running record returned", actionKey)
	case second := <-entered:
		t.Fatalf("recorder calls overlapped within one Run: %q and %q", first, second)
	case <-time.After(20 * time.Millisecond):
	}

	release <- struct{}{}

	if actionKey := <-started; actionKey != workflow.ActionKey("record-"+string(first)) {
		t.Fatalf("first started Action = %q, want %q", actionKey, first)
	}

	second := <-entered
	if second == first {
		t.Fatalf("same parallel node entered twice: %q", second)
	}

	select {
	case actionKey := <-started:
		t.Fatalf("Action %q started before its running record returned", actionKey)
	case <-time.After(20 * time.Millisecond):
	}

	release <- struct{}{}

	if actionKey := <-started; actionKey != workflow.ActionKey("record-"+string(second)) {
		t.Fatalf("second started Action = %q, want %q", actionKey, second)
	}

	if err := <-done; err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}
}

func TestNodeExecutionRecorderAllowsConcurrentRuns(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("record-concurrent", "ok", stringSchema)
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "record-concurrent", stringSchema, workflow.NodePolicy{}),
		action,
	)
	entered := make(chan string, 2)
	release := make(chan struct{})

	var ids atomic.Int32

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) {
			return fmt.Sprintf("concurrent-%d", ids.Add(1)), nil
		}),
		workflow.WithNodeExecutionRecorder(func(_ context.Context, record workflow.NodeExecution) {
			if record.Address.NodeID == "action" && record.NodeRun.Status == workflow.NodeStatusRunning {
				entered <- record.RunID

				<-release
			}
		}),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	done := make(chan error, 2)

	for range 2 {
		go func() {
			_, runErr := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			done <- runErr
		}()
	}

	first := <-entered

	second := <-entered
	if first == second {
		t.Fatalf("concurrent Runs reused ID %q", first)
	}

	close(release)

	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("Runner.Run() error = %v", err)
		}
	}
}

func TestNodeExecutionRecorderExcludesDebugAndPartialModes(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := constantAction("record-mode", "ok", stringSchema)
	plan := compileRoundTrip(
		t,
		singleActionDefinition(t, "record-mode", stringSchema, workflow.NodePolicy{}),
		action,
	)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(workflow.WithNodeExecutionRecorder(log.record))
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	if _, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{}); err != nil {
		t.Fatalf("DebugNode() error = %v", err)
	}

	if _, err := runner.RunPartial(
		t.Context(),
		plan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	); err != nil {
		t.Fatalf("RunPartial() error = %v", err)
	}

	if records := log.snapshot(); len(records) != 0 {
		t.Fatalf("development modes called production recorder: %#v", records)
	}

	if _, err := runner.Run(t.Context(), plan, map[string]workflow.Value{}); err != nil {
		t.Fatalf("ordinary Runner.Run() error = %v", err)
	}

	if records := log.snapshot(); len(records) == 0 {
		t.Fatal("ordinary Run did not call production recorder")
	}
}

func runWithNodeExecutionRecords(
	t *testing.T,
	plan *workflow.Plan,
) ([]workflow.NodeExecution, workflow.RunResult, error) {
	t.Helper()

	log := &nodeExecutionLog{}

	runner, err := workflow.NewRunner(
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "record-errors", nil }),
		workflow.WithNodeExecutionRecorder(log.record),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, runErr := runner.Run(t.Context(), plan, map[string]workflow.Value{})

	return log.snapshot(), result, runErr
}

func nodeExecutionRecords(
	records []workflow.NodeExecution,
	nodeID workflow.NodeID,
) []workflow.NodeExecution {
	selected := make([]workflow.NodeExecution, 0, 2)

	for _, record := range records {
		if record.Address.NodeID == nodeID {
			selected = append(selected, record)
		}
	}

	return selected
}

func lastNodeExecutionRecord(
	t *testing.T,
	records []workflow.NodeExecution,
	nodeID workflow.NodeID,
) workflow.NodeExecution {
	t.Helper()

	selected := nodeExecutionRecords(records, nodeID)
	if len(selected) == 0 {
		t.Fatalf("no NodeExecution record for %q", nodeID)
	}

	return selected[len(selected)-1]
}
