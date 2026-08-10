package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rsbin/pips/workflow"
)

func ExampleSelectorNode() {
	trueValue := workflow.MustValueOf(true)
	config, _ := json.Marshal(workflow.SelectorConfig{
		Cases: []workflow.SelectorCase{
			{
				Route: "approved",
				Predicate: workflow.Predicate{
					Op: workflow.PredicateEqual, Input: "value", Value: &trueValue,
				},
			},
		},
		DefaultRoute: "rejected",
	})
	executor, _ := (workflow.SelectorNode{}).Compile(
		context.Background(),
		nil,
		workflow.NodeDefinition{
			ID:     "selector",
			Config: config,
			Inputs: map[string]workflow.Binding{
				"value": {
					Source: workflow.BindingLiteral,
					Value:  &trueValue,
				},
			},
		},
	)
	output, _ := executor.Invoke(context.Background(), workflow.NodeInput{
		Values: map[string]workflow.Value{"value": trueValue},
	})

	fmt.Println(output.Route)
	// Output: approved
}

func ExampleBatchConfig() {
	config := workflow.BatchConfig{
		Mode:           workflow.BatchParallel,
		MaxConcurrency: 4,
		ErrorMode:      workflow.BatchContinueWithNull,
		MaxItems:       100,
	}

	fmt.Println(config.Mode, config.MaxConcurrency, config.ErrorMode)
	// Output: parallel 4 continue_with_null
}

func ExampleDefinitionRef() {
	reference := workflow.DefinitionRef{
		ID:          "thumbnail-flow",
		Revision:    "v3",
		Fingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	fmt.Println(reference.ID, reference.Revision)
	// Output: thumbnail-flow v3
}
