package workflow_test

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

//go:embed testdata/*.json
var workflowFixtures embed.FS

func TestWorkflowFixturesRoundTripAndRun(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	boolSchema := mustSchema(t, `{"type":"boolean"}`)
	errorTypeSchema := mustSchema(
		t,
		`{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`,
	)

	tests := []struct {
		name     string
		actions  []workflow.Action
		inputs   map[string]workflow.Value
		expected map[string]string
		status   workflow.RunStatus
	}{
		{
			name: "linear",
			actions: []workflow.Action{
				fixtureAction("normalize", "v1", map[string]workflow.PortSchema{"value": stringSchema}, map[string]workflow.PortSchema{"value": stringSchema}, func(input workflow.ActionInput) (workflow.ActionOutput, error) {
					value, err := workflow.DecodeValue[string](input.Values["value"])
					if err != nil {
						return workflow.ActionOutput{}, err
					}

					return workflow.ActionOutput{Values: map[string]workflow.Value{"value": workflow.MustValueOf("normalized:" + value)}}, nil
				}),
				fixtureAction("save", "v2", map[string]workflow.PortSchema{"value": stringSchema}, map[string]workflow.PortSchema{"result": stringSchema}, func(input workflow.ActionInput) (workflow.ActionOutput, error) {
					return workflow.ActionOutput{Values: map[string]workflow.Value{"result": input.Values["value"]}}, nil
				}),
			},
			inputs:   map[string]workflow.Value{"input": workflow.MustValueOf("fixture")},
			expected: map[string]string{"result": `"normalized:fixture"`},
		},
		{
			name: "condition",
			actions: []workflow.Action{
				fixtureAction("validate", "v1", map[string]workflow.PortSchema{"value": boolSchema}, map[string]workflow.PortSchema{"valid": boolSchema}, func(input workflow.ActionInput) (workflow.ActionOutput, error) {
					return workflow.ActionOutput{Values: map[string]workflow.Value{"valid": input.Values["value"]}}, nil
				}),
				constantAction("accept", "accepted", stringSchema),
				constantAction("reject", "rejected", stringSchema),
			},
			inputs:   map[string]workflow.Value{"approved": workflow.MustValueOf(false)},
			expected: map[string]string{"result": `"rejected"`},
		},
		{
			name: "parallel",
			actions: []workflow.Action{
				constantAction("profile", "profile", stringSchema),
				constantAction("policy", "policy", stringSchema),
			},
			inputs:   map[string]workflow.Value{},
			expected: map[string]string{"profile": `"profile"`, "policy": `"policy"`},
		},
		{
			name: "failure",
			actions: []workflow.Action{
				fixtureAction("unreliable", "v1", map[string]workflow.PortSchema{}, map[string]workflow.PortSchema{"result": stringSchema}, func(workflow.ActionInput) (workflow.ActionOutput, error) {
					return workflow.ActionOutput{}, errors.New("expected fixture failure")
				}),
				fixtureAction(
					"fallback",
					"v1",
					map[string]workflow.PortSchema{
						"message": stringSchema,
						"type":    errorTypeSchema,
					},
					map[string]workflow.PortSchema{"result": stringSchema},
					func(input workflow.ActionInput) (workflow.ActionOutput, error) {
						message, err := workflow.DecodeValue[string](input.Values["message"])
						if err != nil {
							return workflow.ActionOutput{}, err
						}

						failureType, err := workflow.DecodeValue[string](input.Values["type"])
						if err != nil {
							return workflow.ActionOutput{}, err
						}

						return workflow.ActionOutput{Values: map[string]workflow.Value{
							"result": workflow.MustValueOf(message + "|" + failureType),
						}}, nil
					},
				),
			},
			inputs:   map[string]workflow.Value{},
			expected: map[string]string{"result": `"expected fixture failure|error"`},
			status:   workflow.RunStatusPartialSucceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			data, err := workflowFixtures.ReadFile("testdata/" + test.name + ".json")
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}

			definition, err := workflow.DecodeDefinition(data)
			if err != nil {
				t.Fatalf("DecodeDefinition() error = %v", err)
			}

			fingerprint, err := definition.Fingerprint()
			if err != nil {
				t.Fatalf("Fingerprint() error = %v", err)
			}

			encoded, err := json.Marshal(definition)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}

			roundTripped, err := workflow.DecodeDefinition(encoded)
			if err != nil {
				t.Fatalf("round-trip DecodeDefinition() error = %v", err)
			}

			roundTripFingerprint, err := roundTripped.Fingerprint()
			if err != nil {
				t.Fatalf("round-trip Fingerprint() error = %v", err)
			}

			if fingerprint != roundTripFingerprint {
				t.Fatalf("fingerprint changed: %s != %s", fingerprint, roundTripFingerprint)
			}

			registry, err := workflow.NewDefaultRegistry(test.actions...)
			if err != nil {
				t.Fatalf("NewDefaultRegistry() error = %v", err)
			}

			plan, err := workflow.Compile(t.Context(), roundTripped, registry)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}

			runner, err := workflow.NewRunner(
				workflow.WithRunIDSource(func(time.Time) (string, error) { return "fixture-run", nil }),
			)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			result, err := runner.Run(t.Context(), plan, test.inputs)
			if err != nil {
				t.Fatalf("Runner.Run() error = %v", err)
			}

			wantStatus := test.status
			if wantStatus == "" {
				wantStatus = workflow.RunStatusSucceeded
			}

			if result.Status != wantStatus {
				t.Fatalf("RunResult.Status = %s, want %s", result.Status, wantStatus)
			}

			for name, expected := range test.expected {
				if actual := result.Outputs[name].String(); actual != expected {
					t.Fatalf("output %q = %s, want %s", name, actual, expected)
				}
			}
		})
	}
}

func fixtureAction(
	key workflow.ActionKey,
	version string,
	inputs map[string]workflow.PortSchema,
	outputs map[string]workflow.PortSchema,
	run func(workflow.ActionInput) (workflow.ActionOutput, error),
) *fakeAction {
	return &fakeAction{
		spec: workflow.ActionSpec{Key: key, Version: version, Inputs: inputs, Outputs: outputs},
		run: func(_ context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			return run(input)
		},
	}
}
