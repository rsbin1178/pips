package workflow_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/workflow"
)

type failureExampleAction struct {
	spec workflow.ActionSpec
	run  func(workflow.ActionInput) (workflow.ActionOutput, error)
}

func (a failureExampleAction) Spec() workflow.ActionSpec {
	return a.spec
}

func (a failureExampleAction) Run(
	_ context.Context,
	input workflow.ActionInput,
) (workflow.ActionOutput, error) {
	return a.run(input)
}

func ExampleBindingNodeError() {
	definition, _ := workflow.DecodeDefinition([]byte(`{
  "schema":"pips.workflow/v1alpha1",
  "id":"typed-failure-example",
  "revision":"v1",
  "name":"Typed failure example",
  "inputs":{},
  "outputs":{
    "result":{
      "schema":{"type":"string"},
      "binding":{"source":"node_output","node":"merge","port":"result"}
    }
  },
  "nodes":[
    {"id":"start","type":"start","version":"v1"},
    {
      "id":"unreliable","type":"action","version":"v1",
      "config":{"action":"unreliable","version":"v1"},
      "policy":{"retry":{"max_attempts":2},"error":"route_error"}
    },
    {
      "id":"success","type":"action","version":"v1",
      "config":{"action":"success","version":"v1"}
    },
    {
      "id":"handler","type":"action","version":"v1",
      "config":{"action":"handler","version":"v1"},
      "inputs":{
        "message":{"source":"node_error","node":"unreliable","port":"error_message"},
        "type":{"source":"node_error","node":"unreliable","port":"error_type"}
      }
    },
    {
      "id":"merge","type":"merge","version":"v1",
      "config":{
        "mode":"exclusive",
        "outputs":{
          "result":{
            "schema":{"type":"string"},
            "sources":["success","failure"]
          }
        }
      },
      "inputs":{
        "success":{"source":"node_output","node":"success","port":"result"},
        "failure":{"source":"node_output","node":"handler","port":"result"}
      }
    },
    {"id":"end","type":"end","version":"v1"}
  ],
  "edges":[
    {"from":{"node":"start","route":"success"},"to":"unreliable"},
    {"from":{"node":"unreliable","route":"success"},"to":"success"},
    {"from":{"node":"unreliable","route":"error"},"to":"handler"},
    {"from":{"node":"success","route":"success"},"to":"merge"},
    {"from":{"node":"handler","route":"success"},"to":"merge"},
    {"from":{"node":"merge","route":"success"},"to":"end"}
  ],
  "limits":{"max_concurrency":1,"max_steps":100}
}`))
	stringSchema, _ := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	errorTypeSchema, _ := workflow.ParsePortSchema([]byte(
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	))
	attempts := 0
	unreliable := failureExampleAction{
		spec: workflow.ActionSpec{
			Key: "unreliable", Version: "v1",
			Inputs:  map[string]workflow.PortSchema{},
			Outputs: map[string]workflow.PortSchema{"result": stringSchema},
		},
		run: func(workflow.ActionInput) (workflow.ActionOutput, error) {
			attempts++

			return workflow.ActionOutput{}, errors.New("final failure")
		},
	}
	handler := failureExampleAction{
		spec: workflow.ActionSpec{
			Key: "handler", Version: "v1",
			Inputs: map[string]workflow.PortSchema{
				"message": stringSchema,
				"type":    errorTypeSchema,
			},
			Outputs: map[string]workflow.PortSchema{"result": stringSchema},
		},
		run: func(input workflow.ActionInput) (workflow.ActionOutput, error) {
			message, _ := workflow.DecodeValue[string](input.Values["message"])
			failureType, _ := workflow.DecodeValue[string](input.Values["type"])

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf(message + "|" + failureType),
			}}, nil
		},
	}
	success := failureExampleAction{
		spec: workflow.ActionSpec{
			Key: "success", Version: "v1",
			Inputs:  map[string]workflow.PortSchema{},
			Outputs: map[string]workflow.PortSchema{"result": stringSchema},
		},
		run: func(workflow.ActionInput) (workflow.ActionOutput, error) {
			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("success"),
			}}, nil
		},
	}
	registry, _ := workflow.NewDefaultRegistry(unreliable, success, handler)
	plan, _ := workflow.Compile(context.Background(), definition, registry)
	runner, _ := workflow.NewRunner()
	result, _ := runner.Run(context.Background(), plan, map[string]workflow.Value{})

	fmt.Println(
		attempts,
		result.Status,
		result.Nodes["unreliable"].Status,
		result.Outputs["result"].String(),
	)
	// Output: 2 partial-succeeded exception "final failure|error"
}
