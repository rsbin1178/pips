package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func FuzzParseValue(f *testing.F) {
	f.Add([]byte(`{"name":"Ada","items":[1,true,null]}`))
	f.Add([]byte(`1.0000000000000000001`))
	f.Add([]byte(`{"duplicate":1,"duplicate":2}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := workflow.ParseValue(data)
		if err != nil {
			return
		}

		if !json.Valid(value.RawJSON()) {
			t.Fatal("successful ParseValue returned invalid JSON")
		}

		roundTripped, err := workflow.ParseValue(value.RawJSON())
		if err != nil {
			t.Fatalf("round-trip ParseValue() error = %v", err)
		}

		if !value.Equal(roundTripped) {
			t.Fatal("round-trip ParseValue changed value")
		}
	})
}

func FuzzResumeCheckpoint(f *testing.F) {
	action := &fakeAction{
		spec: actionSpec(
			"fuzz_checkpoint_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{},
		),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		f.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-checkpoint", Revision: "v1",
		Name:   "Fuzz Checkpoint",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"fuzz_checkpoint_action","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "work"},
			{From: workflow.NodeRoute{Node: "work", Route: workflow.RouteSuccess}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}

	plan, err := workflow.Compile(
		context.Background(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		f.Fatalf("Compile() error = %v", err)
	}

	seedStore := &memoryCheckpointStore{}

	seedRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(seedStore),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "fuzz-run", nil }),
	)
	if err != nil {
		f.Fatalf("NewRunner() error = %v", err)
	}

	if _, err := seedRunner.Run(context.Background(), plan, map[string]workflow.Value{}); err == nil {
		f.Fatal("Runner.Run() unexpectedly completed")
	}

	valid := seedStore.value("fuzz-run")
	f.Add(valid)
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":2,"registry_fingerprint":"legacy","contract_fingerprint":"current"}`))
	f.Add([]byte(`{"version":3,"registry_fingerprint":"legacy","contract_fingerprint":"current"}`))
	f.Add([]byte(`{"version":3,"contract_fingerprint":"first","contract_fingerprint":"second"}`))
	f.Add([]byte(`not-json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		store := &memoryCheckpointStore{values: map[string][]byte{"fuzz-run": data}}

		runner, err := workflow.NewRunner(workflow.WithCheckpointStore(store))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		result, err := runner.Resume(context.Background(), plan, "fuzz-run", nil)
		if err == nil && result.Status != workflow.RunStatusSucceeded {
			t.Fatalf("successful Resume() status = %s", result.Status)
		}
	})
}

func FuzzResumeNodeDebugCheckpoint(f *testing.F) {
	action := &fakeAction{
		spec: actionSpec(
			"fuzz_node_debug_checkpoint",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{},
		),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		f.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-node-debug", Revision: "v1", Name: "Fuzz Node Debug",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"fuzz_node_debug_checkpoint","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "work"},
			{From: workflow.NodeRoute{Node: "work", Route: workflow.RouteSuccess}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}

	plan, err := workflow.Compile(
		context.Background(), definition, registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("work")),
	)
	if err != nil {
		f.Fatalf("Compile() error = %v", err)
	}

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("work"))
	if err != nil {
		f.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	seedStore := &memoryCheckpointStore{}

	seedRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(seedStore),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "fuzz-node-debug-run", nil }),
	)
	if err != nil {
		f.Fatalf("NewRunner() error = %v", err)
	}

	if _, err := seedRunner.DebugNode(
		context.Background(), debugPlan, map[string]workflow.Value{},
	); err == nil {
		f.Fatal("Runner.DebugNode() unexpectedly completed")
	}

	valid := seedStore.value("fuzz-node-debug-run")
	f.Add(valid)
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":2,"registry_fingerprint":"legacy","contract_fingerprint":"current"}`))
	f.Add([]byte(`{"version":3,"registry_fingerprint":"legacy","contract_fingerprint":"current"}`))
	f.Add([]byte(`{"version":3,"plan_fingerprint":"first","plan_fingerprint":"second"}`))
	f.Add([]byte(`not-json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		store := &memoryCheckpointStore{values: map[string][]byte{
			"fuzz-node-debug-run": data,
		}}

		runner, err := workflow.NewRunner(workflow.WithCheckpointStore(store))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		result, err := runner.ResumeNodeDebug(
			context.Background(), debugPlan, "fuzz-node-debug-run", nil,
		)
		if err == nil && result.Status != workflow.RunStatusSucceeded {
			t.Fatalf("successful ResumeNodeDebug() status = %s", result.Status)
		}
	})
}

func FuzzPrepareNodeDebugPath(f *testing.F) {
	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		f.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-debug-path", Revision: "v1", Name: "Fuzz Debug Path",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{{
			From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "end",
		}},
		Limits: workflow.DefaultLimits(),
	}

	plan, err := workflow.Compile(context.Background(), definition, registry)
	if err != nil {
		f.Fatalf("Compile() error = %v", err)
	}

	f.Add("start")
	f.Add("missing")
	f.Add("start/child")

	f.Fuzz(func(_ *testing.T, encoded string) {
		if len(encoded) > 1_000 {
			return
		}

		segments := strings.Split(encoded, "/")

		nodes := make([]workflow.NodeID, len(segments))
		for index, segment := range segments {
			nodes[index] = workflow.NodeID(segment)
		}

		_, _ = workflow.PrepareNodeDebug(plan, workflow.NewNodePath(nodes...))
	})
}

func FuzzCompileInterruptPath(f *testing.F) {
	registry, err := workflow.NewDefaultRegistry()
	if err != nil {
		f.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-path", Revision: "v1", Name: "Fuzz Path",
		Inputs: map[string]workflow.WorkflowInput{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{{
			From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "end",
		}},
		Limits: workflow.DefaultLimits(),
	}

	f.Add("start")
	f.Add("missing")
	f.Add("start/nested")

	f.Fuzz(func(t *testing.T, encoded string) {
		if len(encoded) > 1_000 {
			return
		}

		segments := strings.Split(encoded, "/")

		nodes := make([]workflow.NodeID, len(segments))
		for index, segment := range segments {
			nodes[index] = workflow.NodeID(segment)
		}

		_, _ = workflow.Compile(
			t.Context(),
			definition,
			registry,
			workflow.WithInterruptBeforeNodes(workflow.NewNodePath(nodes...)),
		)
	})
}

func FuzzResumeTargetID(f *testing.F) {
	stringSchema, err := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	if err != nil {
		f.Fatalf("ParsePortSchema() error = %v", err)
	}

	action := &fakeAction{
		spec: actionSpec(
			"fuzz_target_action",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(
					ctx,
					workflow.MustValueOf("waiting"),
				)
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("done"),
			}}, nil
		},
	}

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		f.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := workflow.Definition{
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-target", Revision: "v1", Name: "Fuzz Target",
		Inputs: map[string]workflow.WorkflowInput{},
		Outputs: map[string]workflow.OutputBinding{
			"result": {
				Schema: stringSchema,
				Binding: workflow.Binding{
					Source: workflow.BindingNodeOutput, Node: "work", Port: "result",
				},
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"fuzz_target_action","version":"v1"}`),
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: workflow.RouteSuccess}, To: "work"},
			{From: workflow.NodeRoute{Node: "work", Route: workflow.RouteSuccess}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}

	plan, err := workflow.Compile(context.Background(), definition, registry)
	if err != nil {
		f.Fatalf("Compile() error = %v", err)
	}

	seedStore := &memoryCheckpointStore{}

	seedRunner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(seedStore),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "fuzz-target-run", nil }),
	)
	if err != nil {
		f.Fatalf("NewRunner() error = %v", err)
	}

	result, err := seedRunner.Run(context.Background(), plan, map[string]workflow.Value{})
	if err == nil || result.Interruption == nil || len(result.Interruption.Contexts) != 1 {
		f.Fatalf("Runner.Run() result = %#v, error = %v", result, err)
	}

	checkpoint := seedStore.value("fuzz-target-run")
	validID := result.Interruption.Contexts[0].ID

	f.Add(validID)
	f.Add("")
	f.Add("unknown")

	f.Fuzz(func(t *testing.T, interruptID string) {
		if len(interruptID) > 1_000 {
			return
		}

		store := &memoryCheckpointStore{values: map[string][]byte{
			"fuzz-target-run": checkpoint,
		}}

		runner, err := workflow.NewRunner(workflow.WithCheckpointStore(store))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		_, _ = runner.Resume(
			context.Background(),
			plan,
			"fuzz-target-run",
			[]workflow.ResumeTarget{{InterruptID: interruptID}},
		)
	})
}

func FuzzDecodeDefinition(f *testing.F) {
	f.Add([]byte(`{"schema":"pips.workflow/v1alpha2","id":"fuzz","revision":"v1","name":"Fuzz","inputs":{"value":{"schema":{"type":["string","null"]},"default":"fallback"}},"outputs":{"result":{"schema":{"type":["string","null"]},"binding":{"source":"workflow_input","port":"value"}}},"nodes":[{"id":"start","type":"start","version":"v1"},{"id":"end","type":"end","version":"v1"}],"edges":[{"from":{"node":"start","route":"success"},"to":"end"}],"limits":{"max_concurrency":1,"max_steps":10}}`))
	f.Add([]byte(`{"schema":"unknown"}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		definition, err := workflow.DecodeDefinition(data)
		if err != nil {
			return
		}

		encoded, err := json.Marshal(definition)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		if _, err := workflow.DecodeDefinition(encoded); err != nil {
			t.Fatalf("round-trip DecodeDefinition() error = %v", err)
		}
	})
}

func FuzzValueLookup(f *testing.F) {
	value := workflow.MustValueOf(map[string]any{
		"object": map[string]any{"name": "Ada"},
		"array":  []any{"first", "second"},
	})

	f.Add("object.name")
	f.Add("array.1")
	f.Add("array.-1")

	f.Fuzz(func(_ *testing.T, path string) {
		_, _ = value.Lookup(strings.Split(path, ".")...)
	})
}

func FuzzConditionConfig(f *testing.F) {
	f.Add([]byte(`{"predicate":{"op":"exists","input":"value"},"true_route":"yes","false_route":"no"}`))
	f.Add([]byte(`{"expression":"value == true"}`))

	f.Fuzz(func(t *testing.T, config []byte) {
		value := workflow.MustValueOf(true)
		definition := workflow.NodeDefinition{
			ID:      "condition",
			Type:    workflow.NodeTypeCondition,
			Version: workflow.BuiltinNodeVersion,
			Config:  config,
			Inputs: map[string]workflow.Binding{
				"value": {Source: workflow.BindingLiteral, Value: &value},
			},
		}

		executor, err := (workflow.ConditionNode{}).Compile(
			t.Context(),
			testCompileContext{},
			definition,
		)
		if err != nil {
			return
		}

		_, _ = executor.Invoke(t.Context(), workflow.NodeInput{
			Values: map[string]workflow.Value{"value": value},
		})
	})
}

func FuzzSelectorConfig(f *testing.F) {
	f.Add([]byte(`{"cases":[{"route":"yes","predicate":{"op":"exists","input":"value"}}],"default_route":"no"}`))
	f.Add([]byte(`{"cases":[],"default_route":"fallback"}`))

	f.Fuzz(func(t *testing.T, config []byte) {
		value := workflow.MustValueOf(true)
		definition := workflow.NodeDefinition{
			ID:      "selector",
			Type:    workflow.NodeTypeSelector,
			Version: workflow.BuiltinNodeVersion,
			Config:  config,
			Inputs: map[string]workflow.Binding{
				"value": {Source: workflow.BindingLiteral, Value: &value},
			},
		}

		executor, err := (workflow.SelectorNode{}).Compile(
			t.Context(),
			testCompileContext{},
			definition,
		)
		if err != nil {
			return
		}

		_, _ = executor.Invoke(t.Context(), workflow.NodeInput{
			Values: map[string]workflow.Value{"value": value},
		})
	})
}

func FuzzCompositeNodeConfigs(f *testing.F) {
	f.Add("batch", []byte(`{"mode":"parallel","max_concurrency":0}`))
	f.Add("sub_workflow", []byte(`{"workflow":{"id":"child","revision":"v1","fingerprint":"invalid"}}`))
	f.Add("loop", []byte(`{"mode":"count","max_iterations":100}`))

	f.Fuzz(func(t *testing.T, nodeType string, config []byte) {
		definition := workflow.NodeDefinition{
			ID: "composite", Version: workflow.BuiltinNodeVersion, Config: config,
		}

		switch workflow.NodeTypeKey(nodeType) {
		case workflow.NodeTypeBatch:
			definition.Type = workflow.NodeTypeBatch
			_, _ = (workflow.BatchNode{}).Compile(t.Context(), testCompileContext{}, definition)
		case workflow.NodeTypeSubWorkflow:
			definition.Type = workflow.NodeTypeSubWorkflow
			_, _ = (workflow.SubWorkflowNode{}).Compile(t.Context(), testCompileContext{}, definition)
		case workflow.NodeTypeLoop:
			definition.Type = workflow.NodeTypeLoop
			_, _ = (workflow.LoopNode{}).Compile(t.Context(), testCompileContext{}, definition)
		default:
			return
		}
	})
}
