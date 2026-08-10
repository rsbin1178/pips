package workflow_test

import (
	"errors"
	"testing"

	"github.com/rsbin/pips/workflow"
)

func TestPortSchemaValidate(t *testing.T) {
	t.Parallel()

	schema, err := workflow.ParsePortSchema([]byte(`{
		"type":"object",
		"properties":{"name":{"type":"string"}},
		"required":["name"],
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	valid := workflow.MustValueOf(map[string]any{"name": "Ada"})
	if err := schema.Validate(valid); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	invalid := workflow.MustValueOf(map[string]any{"name": 42})
	if err := schema.Validate(invalid); !errors.Is(err, workflow.ErrSchemaViolation) {
		t.Fatalf("Validate(invalid) error = %v, want ErrSchemaViolation", err)
	}
}

func TestPortSchemaRejectsExternalReference(t *testing.T) {
	t.Parallel()

	_, err := workflow.ParsePortSchema([]byte(`{"$ref":"https://example.com/schema.json"}`))
	if !errors.Is(err, workflow.ErrInvalidSchema) {
		t.Fatalf("ParsePortSchema() error = %v, want ErrInvalidSchema", err)
	}
}

func TestPortSchemaMutationIsolation(t *testing.T) {
	t.Parallel()

	schema, err := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	raw := schema.RawJSON()
	raw[0] = '['

	if string(schema.RawJSON()) != `{"type":"string"}` {
		t.Fatalf("schema changed through RawJSON: %s", schema.RawJSON())
	}
}
