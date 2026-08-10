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
		Inputs:   map[string]workflow.PortSchema{"input": stringSchema},
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
