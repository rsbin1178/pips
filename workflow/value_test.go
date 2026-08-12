package workflow_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/workflow"
)

func TestParseValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		expected  string
		expectedK workflow.ValueKind
		wantErr   bool
	}{
		{name: "canonical object", input: `{"z":1,"a":[true,null]}`, expected: `{"a":[true,null],"z":1}`, expectedK: workflow.ValueObject},
		{name: "number", input: `1.25`, expected: `1.25`, expectedK: workflow.ValueNumber},
		{name: "duplicate key", input: `{"a":1,"a":2}`, wantErr: true},
		{name: "multiple values", input: `true false`, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			value, err := workflow.ParseValue([]byte(test.input))
			if test.wantErr {
				if !errors.Is(err, workflow.ErrInvalidValue) {
					t.Fatalf("ParseValue() error = %v, want ErrInvalidValue", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseValue() error = %v", err)
			}

			if value.String() != test.expected {
				t.Fatalf("Value.String() = %s, want %s", value.String(), test.expected)
			}

			if value.Kind() != test.expectedK {
				t.Fatalf("Value.Kind() = %v, want %v", value.Kind(), test.expectedK)
			}
		})
	}
}

func TestValueMutationIsolation(t *testing.T) {
	t.Parallel()

	input := map[string]any{"items": []any{"first", "second"}}

	value, err := workflow.ValueOf(input)
	if err != nil {
		t.Fatalf("ValueOf() error = %v", err)
	}

	items, ok := input["items"].([]any)
	if !ok {
		t.Fatal("items is not an array")
	}

	items[0] = "changed"

	item, ok := value.Lookup("items", "0")
	if !ok || item.String() != `"first"` {
		t.Fatalf("Lookup() = %s, %v, want first, true", item.String(), ok)
	}

	raw := value.RawJSON()
	raw[0] = '['

	if !json.Valid(value.RawJSON()) {
		t.Fatal("RawJSON mutation changed Value")
	}
}
