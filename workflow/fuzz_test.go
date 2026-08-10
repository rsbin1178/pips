package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"

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

func FuzzDecodeDefinition(f *testing.F) {
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
