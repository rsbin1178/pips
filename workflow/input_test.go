package workflow

import "testing"

func TestNormalizeWorkflowInputs(t *testing.T) {
	t.Parallel()

	stringSchema := mustInputSchema(t, `{"type":"string"}`)
	nullableStringSchema := mustInputSchema(t, `{"type":["string","null"]}`)
	arraySchema := mustInputSchema(t, `{"type":"array"}`)
	objectSchema := mustInputSchema(t, `{"type":"object"}`)
	booleanSchema := mustInputSchema(t, `{"type":"boolean"}`)
	numberSchema := mustInputSchema(t, `{"type":"number"}`)
	stringDefault := MustValueOf("default")
	arrayDefault := MustValueOf([]string{"default"})
	objectDefault := MustValueOf(map[string]string{"value": "default"})
	boolDefault := MustValueOf(true)
	numberDefault := MustValueOf(1)

	tests := []struct {
		name      string
		contract  WorkflowInput
		values    map[string]Value
		want      Value
		wantError bool
	}{
		{name: "missing required", contract: WorkflowInput{Schema: stringSchema, Required: true}, values: map[string]Value{}, wantError: true},
		{name: "missing optional default", contract: WorkflowInput{Schema: stringSchema, Default: &stringDefault}, values: map[string]Value{}, want: stringDefault},
		{name: "missing optional null", contract: WorkflowInput{Schema: nullableStringSchema}, values: map[string]Value{}, want: workflowNullValue},
		{name: "required default still rejects missing", contract: WorkflowInput{Schema: stringSchema, Required: true, Default: &stringDefault}, values: map[string]Value{}, wantError: true},
		{name: "empty string uses same-kind default", contract: WorkflowInput{Schema: stringSchema, Default: &stringDefault}, values: map[string]Value{"value": MustValueOf("")}, want: stringDefault},
		{name: "empty array uses same-kind default", contract: WorkflowInput{Schema: arraySchema, Default: &arrayDefault}, values: map[string]Value{"value": MustValueOf([]string{})}, want: arrayDefault},
		{name: "empty object uses same-kind default", contract: WorkflowInput{Schema: objectSchema, Default: &objectDefault}, values: map[string]Value{"value": MustValueOf(map[string]string{})}, want: objectDefault},
		{name: "false is retained", contract: WorkflowInput{Schema: booleanSchema, Default: &boolDefault}, values: map[string]Value{"value": MustValueOf(false)}, want: MustValueOf(false)},
		{name: "zero is retained", contract: WorkflowInput{Schema: numberSchema, Default: &numberDefault}, values: map[string]Value{"value": MustValueOf(0)}, want: MustValueOf(0)},
		{name: "explicit null is retained and validated", contract: WorkflowInput{Schema: nullableStringSchema, Default: &stringDefault}, values: map[string]Value{"value": workflowNullValue}, want: workflowNullValue},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			normalized, err := normalizeWorkflowInputs(
				map[string]WorkflowInput{"value": test.contract},
				test.values,
				nil,
			)
			if test.wantError {
				if err == nil {
					t.Fatal("normalizeWorkflowInputs() error = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("normalizeWorkflowInputs() error = %v", err)
			}

			if !normalized["value"].Equal(test.want) {
				t.Fatalf("normalized value = %s, want %s", normalized["value"].String(), test.want.String())
			}
		})
	}
}

func TestNormalizeWorkflowInputsRejectsUnknownAndCrossKindDefault(t *testing.T) {
	t.Parallel()

	unionSchema := mustInputSchema(t, `{"type":["string","array"]}`)
	arrayDefault := MustValueOf([]string{"default"})

	normalized, err := normalizeWorkflowInputs(
		map[string]WorkflowInput{
			"value": {Schema: unionSchema, Default: &arrayDefault},
		},
		map[string]Value{"value": MustValueOf("")},
		nil,
	)
	if err != nil {
		t.Fatalf("normalizeWorkflowInputs(cross kind) error = %v", err)
	}

	if got := normalized["value"].String(); got != `""` {
		t.Fatalf("cross-kind normalized value = %s, want empty string", got)
	}

	_, err = normalizeWorkflowInputs(
		map[string]WorkflowInput{"value": {Schema: unionSchema}},
		map[string]Value{"unknown": MustValueOf("value")},
		nil,
	)
	if err == nil {
		t.Fatal("normalizeWorkflowInputs(unknown) error = nil")
	}
}

func mustInputSchema(t *testing.T, schema string) PortSchema {
	t.Helper()

	value, err := ParsePortSchema([]byte(schema))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	return value
}
