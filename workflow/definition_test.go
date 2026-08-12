package workflow_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestDecodeDefinition(t *testing.T) {
	t.Parallel()

	definition := minimalDefinition(t)

	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	decoded, err := workflow.DecodeDefinition(data)
	if err != nil {
		t.Fatalf("DecodeDefinition() error = %v", err)
	}

	roundTrip, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("json.Marshal(decoded) error = %v", err)
	}

	if !bytes.Equal(data, roundTrip) {
		t.Fatalf("round trip changed definition\ngot:  %s\nwant: %s", roundTrip, data)
	}
}

func TestDecodeDefinitionRejectsUnknownAndDuplicateFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown", data: `{"schema":"pips.workflow/v1alpha1","unknown":true}`},
		{name: "duplicate", data: `{"schema":"pips.workflow/v1alpha1","schema":"pips.workflow/v1alpha1"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.DecodeDefinition([]byte(test.data))
			if !errors.Is(err, workflow.ErrInvalidDefinition) {
				t.Fatalf("DecodeDefinition() error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
}

func TestDecodeDefinitionRejectsUnknownSchemaAndLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*workflow.Definition)
	}{
		{
			name: "schema",
			mutate: func(definition *workflow.Definition) {
				definition.Schema = "pips.workflow/v2"
			},
		},
		{
			name: "concurrency limit",
			mutate: func(definition *workflow.Definition) {
				definition.Limits.MaxConcurrency = 257
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition := minimalDefinition(t)
			test.mutate(&definition)

			if test.name == "schema" {
				data := []byte(`{"schema":"pips.workflow/v2","id":"example","revision":"v1","name":"Example","inputs":{},"outputs":{},"nodes":[],"edges":[],"limits":{"max_concurrency":1,"max_steps":1}}`)

				_, err := workflow.DecodeDefinition(data)
				if !errors.Is(err, workflow.ErrInvalidDefinition) {
					t.Fatalf("DecodeDefinition() error = %v, want ErrInvalidDefinition", err)
				}

				return
			}

			data, err := json.Marshal(definition)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}

			_, err = workflow.DecodeDefinition(data)
			if !errors.Is(err, workflow.ErrInvalidDefinition) {
				t.Fatalf("DecodeDefinition() error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
}

func TestDecodeDefinitionRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte(" "), (2<<20)+1)

	_, err := workflow.DecodeDefinition(data)
	if !errors.Is(err, workflow.ErrInvalidDefinition) {
		t.Fatalf("DecodeDefinition() error = %v, want ErrInvalidDefinition", err)
	}
}

func TestDefinitionFingerprintIgnoresIdentity(t *testing.T) {
	t.Parallel()

	first := minimalDefinition(t)
	second := minimalDefinition(t)
	second.ID = "renamed"
	second.Revision = "v2"
	second.Name = "Renamed"

	firstFingerprint, err := first.Fingerprint()
	if err != nil {
		t.Fatalf("first.Fingerprint() error = %v", err)
	}

	secondFingerprint, err := second.Fingerprint()
	if err != nil {
		t.Fatalf("second.Fingerprint() error = %v", err)
	}

	if firstFingerprint != secondFingerprint {
		t.Fatalf("fingerprints differ: %s != %s", firstFingerprint, secondFingerprint)
	}
}

func TestDefinitionInputContractsUseStrictVersionedWireShapes(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	nullableStringSchema := mustSchema(t, `{"type":["string","null"]}`)
	defaultValue := workflow.MustValueOf("photo")

	v1 := minimalDefinition(t)

	v1Data, err := json.Marshal(v1)
	if err != nil {
		t.Fatalf("json.Marshal(v1alpha1) error = %v", err)
	}

	if bytes.Contains(v1Data, []byte(`"required"`)) || bytes.Contains(v1Data, []byte(`"input":{"schema"`)) {
		t.Fatalf("v1alpha1 wire changed shape: %s", v1Data)
	}

	v2 := minimalDefinition(t)
	v2.Schema = workflow.SchemaV1Alpha2
	v2.Inputs = map[string]workflow.WorkflowInput{
		"prompt": {Schema: stringSchema, Required: true},
		"style":  {Schema: nullableStringSchema, Default: &defaultValue},
	}
	v2.Outputs["result"] = workflow.OutputBinding{
		Schema:  stringSchema,
		Binding: workflow.Binding{Source: workflow.BindingWorkflowInput, Port: "prompt"},
	}

	v2Data, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("json.Marshal(v1alpha2) error = %v", err)
	}

	decoded, err := workflow.DecodeDefinition(v2Data)
	if err != nil {
		t.Fatalf("DecodeDefinition(v1alpha2) error = %v", err)
	}

	if !decoded.Inputs["prompt"].Required || decoded.Inputs["style"].Required ||
		decoded.Inputs["style"].Default == nil ||
		!decoded.Inputs["style"].Default.Equal(defaultValue) {
		t.Fatalf("decoded input contracts = %#v", decoded.Inputs)
	}

	wrongShapes := []string{
		`{"schema":"pips.workflow/v1alpha1","id":"example","revision":"v1","name":"Example","inputs":{"input":{"schema":{"type":"string"},"required":true}},"outputs":{},"nodes":[],"edges":[],"limits":{"max_concurrency":1,"max_steps":1}}`,
		`{"schema":"pips.workflow/v1alpha2","id":"example","revision":"v1","name":"Example","inputs":{"input":{"type":"string"}},"outputs":{},"nodes":[],"edges":[],"limits":{"max_concurrency":1,"max_steps":1}}`,
		`{"schema":"pips.workflow/v1alpha2","id":"example","revision":"v1","name":"Example","inputs":{"input":{"schema":{"type":"string"},"unknown":true}},"outputs":{},"nodes":[],"edges":[],"limits":{"max_concurrency":1,"max_steps":1}}`,
		`{"schema":"pips.workflow/v1alpha2","id":"example","revision":"v1","name":"Example","inputs":{"input":{"schema":{"type":"string"},"schema":{"type":"number"}}},"outputs":{},"nodes":[],"edges":[],"limits":{"max_concurrency":1,"max_steps":1}}`,
	}
	for _, data := range wrongShapes {
		if _, err := workflow.DecodeDefinition([]byte(data)); !errors.Is(err, workflow.ErrInvalidDefinition) {
			t.Fatalf("DecodeDefinition(%s) error = %v, want ErrInvalidDefinition", data, err)
		}
	}
}

func TestDefinitionInputContractValidationAndFingerprint(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	nullableStringSchema := mustSchema(t, `{"type":["string","null"]}`)
	number := workflow.MustValueOf(1)
	null := workflow.MustValueOf(nil)
	defaultValue := workflow.MustValueOf("default")

	tests := []struct {
		name  string
		input workflow.WorkflowInput
	}{
		{name: "optional non-nullable without default", input: workflow.WorkflowInput{Schema: stringSchema}},
		{name: "invalid default schema", input: workflow.WorkflowInput{Schema: stringSchema, Default: &number}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			definition := minimalDefinition(t)
			definition.Schema = workflow.SchemaV1Alpha2

			definition.Inputs = map[string]workflow.WorkflowInput{"input": test.input}
			if _, err := definition.Fingerprint(); !errors.Is(err, workflow.ErrInvalidDefinition) {
				t.Fatalf("Fingerprint() error = %v, want ErrInvalidDefinition", err)
			}
		})
	}

	definition := minimalDefinition(t)
	definition.Schema = workflow.SchemaV1Alpha2
	definition.Inputs = map[string]workflow.WorkflowInput{
		"input": {Schema: nullableStringSchema, Default: &null},
	}

	snapshot, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("json.Marshal(null default) error = %v", err)
	}

	if bytes.Contains(snapshot, []byte(`"default"`)) {
		t.Fatalf("canonical null default was not omitted: %s", snapshot)
	}

	decodedNull, err := workflow.DecodeDefinition([]byte(`{"schema":"pips.workflow/v1alpha2","id":"null-default","revision":"v1","name":"Null Default","inputs":{"input":{"schema":{"type":["string","null"]},"default":null}},"outputs":{"result":{"schema":{"type":["string","null"]},"binding":{"source":"workflow_input","port":"input"}}},"nodes":[{"id":"start","type":"start","version":"v1"},{"id":"end","type":"end","version":"v1"}],"edges":[{"from":{"node":"start","route":"success"},"to":"end"}],"limits":{"max_concurrency":1,"max_steps":10}}`))
	if err != nil {
		t.Fatalf("DecodeDefinition(null default) error = %v", err)
	}

	decodedNullWire, err := json.Marshal(decodedNull)
	if err != nil || bytes.Contains(decodedNullWire, []byte(`"default"`)) {
		t.Fatalf("canonical decoded null default = %s, %v", decodedNullWire, err)
	}

	invalid := workflow.Value{}

	definition.Inputs["input"] = workflow.WorkflowInput{
		Schema: nullableStringSchema, Default: &invalid,
	}
	if _, err := json.Marshal(definition); !errors.Is(err, workflow.ErrInvalidDefinition) {
		t.Fatalf("json.Marshal(invalid default) error = %v, want ErrInvalidDefinition", err)
	}

	base := minimalDefinition(t)
	base.Schema = workflow.SchemaV1Alpha2
	base.Inputs = map[string]workflow.WorkflowInput{
		"input": {Schema: nullableStringSchema},
	}
	changed := base
	changed.Inputs = map[string]workflow.WorkflowInput{
		"input": {Schema: nullableStringSchema, Default: &defaultValue},
	}
	baseFingerprint, _ := base.Fingerprint()

	changedFingerprint, _ := changed.Fingerprint()
	if baseFingerprint == changedFingerprint {
		t.Fatal("input default did not change Definition fingerprint")
	}
}

func minimalDefinition(t *testing.T) workflow.Definition {
	t.Helper()

	stringSchema, err := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	return workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "example",
		Revision: "v1",
		Name:     "Example",
		Inputs:   map[string]workflow.WorkflowInput{"input": {Schema: stringSchema, Required: true}},
		Outputs: map[string]workflow.OutputBinding{
			"result": {
				Schema: stringSchema,
				Binding: workflow.Binding{
					Source: workflow.BindingWorkflowInput,
					Port:   "input",
				},
			},
		},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: "start", Version: "v1"},
			{ID: "end", Type: "end", Version: "v1"},
		},
		Edges: []workflow.ControlEdge{
			{From: workflow.NodeRoute{Node: "start", Route: "success"}, To: "end"},
		},
		Limits: workflow.DefaultLimits(),
	}
}
