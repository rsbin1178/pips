package workflow_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

func FuzzResumePartialCheckpoint(f *testing.F) {
	action := &fakeAction{
		spec: actionSpec(
			"fuzz_partial_checkpoint",
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
		Schema: workflow.SchemaV1Alpha1, ID: "fuzz-partial", Revision: "v1",
		Name:   "Fuzz Partial",
		Inputs: map[string]workflow.PortSchema{}, Outputs: map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "work", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: json.RawMessage(`{"action":"fuzz_partial_checkpoint","version":"v1"}`),
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
		workflow.WithRunIDSource(func(time.Time) (string, error) { return "fuzz-partial-run", nil }),
	)
	if err != nil {
		f.Fatalf("NewRunner() error = %v", err)
	}

	_, err = seedRunner.RunPartial(
		context.Background(),
		plan,
		"work",
		workflow.PartialRunInput{
			Inputs: map[string]workflow.Value{},
			Pins: workflow.PinData{
				"start": {},
				"work":  {},
			},
		},
	)
	if err == nil {
		f.Fatal("Runner.RunPartial() unexpectedly completed")
	}

	f.Add(seedStore.value("fuzz-partial-run"))
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`not-json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		store := &memoryCheckpointStore{values: map[string][]byte{"fuzz-partial-run": data}}

		runner, err := workflow.NewRunner(workflow.WithCheckpointStore(store))
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}

		result, err := runner.ResumePartial(
			context.Background(),
			plan,
			"work",
			"fuzz-partial-run",
			nil,
		)
		if err == nil && result.Status != workflow.RunStatusSucceeded {
			t.Fatalf("successful ResumePartial() status = %s", result.Status)
		}
	})
}
