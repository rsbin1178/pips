package workflow_test

import (
	"errors"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestSelectorNodeSelectsFirstMatchingCase(t *testing.T) {
	t.Parallel()

	trueValue := workflow.MustValueOf(true)
	config := workflow.SelectorConfig{
		Cases: []workflow.SelectorCase{
			{
				Route: "first",
				Predicate: workflow.Predicate{
					Op: workflow.PredicateEqual, Input: "value", Value: &trueValue,
				},
			},
			{
				Route: "second",
				Predicate: workflow.Predicate{
					Op: workflow.PredicateEqual, Input: "value", Value: &trueValue,
				},
			},
		},
		DefaultRoute: "fallback",
	}

	executor, err := (workflow.SelectorNode{}).Compile(
		t.Context(),
		nil,
		workflow.NodeDefinition{
			ID:     "selector",
			Config: mustJSON(t, config),
			Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
		},
	)
	if err != nil {
		t.Fatalf("SelectorNode.Compile() error = %v", err)
	}

	output, err := executor.Invoke(t.Context(), workflow.NodeInput{
		Values: map[string]workflow.Value{"value": trueValue},
	})
	if err != nil {
		t.Fatalf("CompiledNode.Invoke() error = %v", err)
	}

	if output.Route != "first" {
		t.Fatalf("route = %q, want first", output.Route)
	}
}

func TestSelectorNodeUsesDefaultRoute(t *testing.T) {
	t.Parallel()

	trueValue := workflow.MustValueOf(true)
	config := workflow.SelectorConfig{
		Cases: []workflow.SelectorCase{
			{
				Route: "matched",
				Predicate: workflow.Predicate{
					Op: workflow.PredicateEqual, Input: "value", Value: &trueValue,
				},
			},
		},
		DefaultRoute: "fallback",
	}

	executor, err := (workflow.SelectorNode{}).Compile(
		t.Context(),
		nil,
		workflow.NodeDefinition{
			ID:     "selector",
			Config: mustJSON(t, config),
			Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
		},
	)
	if err != nil {
		t.Fatalf("SelectorNode.Compile() error = %v", err)
	}

	output, err := executor.Invoke(t.Context(), workflow.NodeInput{
		Values: map[string]workflow.Value{"value": workflow.MustValueOf(false)},
	})
	if err != nil {
		t.Fatalf("CompiledNode.Invoke() error = %v", err)
	}

	if output.Route != "fallback" {
		t.Fatalf("route = %q, want fallback", output.Route)
	}
}

func TestSelectorNodeRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	trueValue := workflow.MustValueOf(true)
	predicate := workflow.Predicate{
		Op: workflow.PredicateEqual, Input: "value", Value: &trueValue,
	}

	casesOverLimit := make([]workflow.SelectorCase, 33)
	for index := range casesOverLimit {
		casesOverLimit[index] = workflow.SelectorCase{
			Route:     string(rune('a' + index)),
			Predicate: predicate,
		}
	}

	tests := []struct {
		name   string
		config workflow.SelectorConfig
	}{
		{
			name:   "missing cases",
			config: workflow.SelectorConfig{DefaultRoute: "fallback"},
		},
		{
			name: "duplicate route",
			config: workflow.SelectorConfig{
				Cases:        []workflow.SelectorCase{{Route: "same", Predicate: predicate}},
				DefaultRoute: "same",
			},
		},
		{
			name: "unknown predicate input",
			config: workflow.SelectorConfig{
				Cases: []workflow.SelectorCase{{
					Route: "matched",
					Predicate: workflow.Predicate{
						Op: workflow.PredicateEqual, Input: "missing", Value: &trueValue,
					},
				}},
				DefaultRoute: "fallback",
			},
		},
		{
			name: "case limit",
			config: workflow.SelectorConfig{
				Cases: casesOverLimit, DefaultRoute: "fallback",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := (workflow.SelectorNode{}).Compile(
				t.Context(),
				nil,
				workflow.NodeDefinition{
					ID:     "selector",
					Config: mustJSON(t, test.config),
					Inputs: map[string]workflow.Binding{"value": workflowInput("value")},
				},
			)
			if !errors.Is(err, workflow.ErrCompile) {
				t.Fatalf("SelectorNode.Compile() error = %v, want ErrCompile", err)
			}
		})
	}
}

func TestBuiltinNodeTypesIncludeCompositeAndLoopNodes(t *testing.T) {
	t.Parallel()

	nodeTypes := workflow.BuiltinNodeTypes()
	if len(nodeTypes) != 12 {
		t.Fatalf("len(BuiltinNodeTypes()) = %d, want 12", len(nodeTypes))
	}

	want := map[workflow.NodeTypeKey]string{
		workflow.NodeTypeSelector:    "Selector",
		workflow.NodeTypeSubWorkflow: "SubWorkflow",
		workflow.NodeTypeBatch:       "Batch",
		workflow.NodeTypeLoop:        "Loop",
		workflow.NodeTypeBreak:       "Break",
		workflow.NodeTypeContinue:    "Continue",
		workflow.NodeTypeSetVariable: "Set Variable",
	}

	for _, nodeType := range nodeTypes {
		spec := nodeType.Spec()
		if displayName, ok := want[spec.Key]; ok {
			if spec.DisplayName != displayName {
				t.Fatalf("display name for %q = %q, want %q", spec.Key, spec.DisplayName, displayName)
			}

			delete(want, spec.Key)
		}
	}

	if len(want) != 0 {
		t.Fatalf("missing built-in node types: %#v", want)
	}
}
