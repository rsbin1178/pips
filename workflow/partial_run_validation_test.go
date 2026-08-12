package workflow_test

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/workflow"
)

func TestRunPartialRejectsInvalidDestinationInputsPinsAndPreviousData(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	linear := partialRunLinearPlan(t, &firstCalls, &secondCalls)
	branch := partialRunBranchPlan(t, new(atomic.Int32), new(atomic.Int32))

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	valid, err := runner.RunPartial(t.Context(), linear, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(seed) error = %v", err)
	}

	wrongDefinition := valid.Data
	wrongDefinition.DefinitionID = "foreign"
	oversized := valid.Data
	oversized.RunID = strings.Repeat("x", 3<<20)
	invalidRevision := valid.Data
	invalidRevision.Revision = ""

	tests := []struct {
		name        string
		plan        *workflow.Plan
		destination workflow.NodeID
		input       workflow.PartialRunInput
	}{
		{
			name:        "unknown destination",
			plan:        linear,
			destination: "missing",
			input:       workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
		},
		{
			name:        "nil inputs",
			plan:        linear,
			destination: "second",
			input: workflow.PartialRunInput{Pins: workflow.PinData{
				"start":  {"value": workflow.MustValueOf("input")},
				"first":  {"value": workflow.MustValueOf("first:input")},
				"second": {"value": workflow.MustValueOf("second:input")},
			}},
		},
		{
			name:        "invalid pin output",
			plan:        linear,
			destination: "second",
			input: workflow.PartialRunInput{
				Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
				Pins: workflow.PinData{
					"first": {"value": workflow.MustValueOf(42)},
				},
			},
		},
		{
			name:        "multi route pin",
			plan:        branch,
			destination: "condition",
			input: workflow.PartialRunInput{
				Inputs: map[string]workflow.Value{"approved": workflow.MustValueOf(true)},
				Pins:   workflow.PinData{"condition": {}},
			},
		},
		{
			name:        "previous definition mismatch",
			plan:        linear,
			destination: "second",
			input: workflow.PartialRunInput{
				Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
				Previous: &wrongDefinition,
			},
		},
		{
			name:        "oversized previous data",
			plan:        linear,
			destination: "second",
			input: workflow.PartialRunInput{
				Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
				Previous: &oversized,
			},
		},
		{
			name:        "invalid previous revision provenance",
			plan:        linear,
			destination: "second",
			input: workflow.PartialRunInput{
				Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
				Previous: &invalidRevision,
			},
		},
	}

	for _, test := range tests {
		_, runErr := runner.RunPartial(t.Context(), test.plan, test.destination, test.input)
		if !errors.Is(runErr, workflow.ErrRun) && !errors.Is(runErr, workflow.ErrCompile) {
			t.Fatalf("%s: Runner.RunPartial() error = %v, want ErrRun or ErrCompile", test.name, runErr)
		}
	}
}

func TestRunPartialAcceptsPreviousPlanDataOnlyWithCurrentDirtyNode(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	definition, registry := partialRunLinearFixture(t, &firstCalls, &secondCalls)

	original, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile(original) error = %v", err)
	}

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	seed, err := runner.RunPartial(t.Context(), original, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{"value": workflow.MustValueOf("input")},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(seed) error = %v", err)
	}

	definition.Revision = "v2"
	definition.Limits.MaxSteps++

	changed, err := workflow.Compile(t.Context(), definition, registry)
	if err != nil {
		t.Fatalf("Compile(changed) error = %v", err)
	}

	_, err = runner.RunPartial(t.Context(), changed, "second", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
		Previous: &seed.Data,
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.RunPartial(stale without dirty) error = %v, want ErrRun", err)
	}

	_, err = runner.RunPartial(t.Context(), changed, "second", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
		Previous: &seed.Data,
		Dirty:    []workflow.NodeID{"missing"},
	})
	if !errors.Is(err, workflow.ErrRun) {
		t.Fatalf("Runner.RunPartial(stale unknown dirty) error = %v, want ErrRun", err)
	}

	refreshed, err := runner.RunPartial(t.Context(), changed, "second", workflow.PartialRunInput{
		Inputs:   map[string]workflow.Value{"value": workflow.MustValueOf("input")},
		Previous: &seed.Data,
		Dirty:    []workflow.NodeID{"first"},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial(stale with dirty) error = %v", err)
	}

	if refreshed.Data.Revision != "v2" ||
		refreshed.Nodes["start"].Origin != workflow.PartialDataReused ||
		refreshed.Nodes["first"].Origin != workflow.PartialDataExecuted {
		t.Fatalf("refreshed result = %#v", refreshed)
	}
}

func TestRunNeverConsultsPartialRunPinsOrPreviousData(t *testing.T) {
	t.Parallel()

	var (
		firstCalls  atomic.Int32
		secondCalls atomic.Int32
	)

	plan := partialRunLinearPlan(t, &firstCalls, &secondCalls)

	runner, err := workflow.NewRunner()
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.RunPartial(t.Context(), plan, "second", workflow.PartialRunInput{
		Inputs: map[string]workflow.Value{},
		Pins: workflow.PinData{
			"start":  {"value": workflow.MustValueOf("pinned-input")},
			"first":  {"value": workflow.MustValueOf("pinned-first")},
			"second": {"value": workflow.MustValueOf("pinned-second")},
		},
	})
	if err != nil {
		t.Fatalf("Runner.RunPartial() error = %v", err)
	}

	ordinary, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("ordinary"),
	})
	if err != nil {
		t.Fatalf("Runner.Run() error = %v", err)
	}

	if got := ordinary.Outputs["value"].String(); got != `"second:first:ordinary"` {
		t.Fatalf("ordinary output = %s", got)
	}

	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("ordinary calls = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}
}
