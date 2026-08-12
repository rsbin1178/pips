package workflow

import (
	"bytes"
	"errors"
	"fmt"
)

var workflowNullValue = MustValueOf(nil)

func normalizeWorkflowInputs(
	contracts map[string]WorkflowInput,
	values map[string]Value,
	scope map[string]struct{},
) (map[string]Value, error) {
	if values == nil {
		return nil, errors.New("nil values")
	}

	normalized := make(map[string]Value, len(values)+len(contracts))
	for name, value := range values {
		if _, ok := contracts[name]; !ok {
			return nil, fmt.Errorf("unknown port %q", name)
		}

		if !value.IsValid() {
			return nil, fmt.Errorf("port %q: %w", name, ErrInvalidValue)
		}

		normalized[name] = value
	}

	for name, contract := range contracts {
		if !workflowInputInScope(name, scope) {
			continue
		}

		value, err := normalizeWorkflowInputValue(contract, normalized[name])
		if err != nil {
			return nil, fmt.Errorf("port %q: %w", name, err)
		}

		normalized[name] = value
	}

	if err := validateWorkflowInputValues(normalized, contracts, scope, false); err != nil {
		return nil, err
	}

	return normalized, nil
}

func normalizeWorkflowInputValue(contract WorkflowInput, value Value) (Value, error) {
	defaultValue := effectiveWorkflowInputDefault(contract)
	if !value.IsValid() {
		if contract.Required {
			return Value{}, errors.New("is missing")
		}

		if defaultValue == nil {
			return workflowNullValue, nil
		}

		return *defaultValue, nil
	}

	if defaultValue != nil && inputValueIsEmpty(value) && value.Kind() == defaultValue.Kind() {
		return *defaultValue, nil
	}

	return value, nil
}

func workflowInputInScope(name string, scope map[string]struct{}) bool {
	if scope == nil {
		return true
	}

	_, ok := scope[name]

	return ok
}

func validateNormalizedWorkflowInputs(
	values map[string]Value,
	contracts map[string]WorkflowInput,
	scope map[string]struct{},
) error {
	return validateWorkflowInputValues(values, contracts, scope, true)
}

func validateWorkflowInputValues(
	values map[string]Value,
	contracts map[string]WorkflowInput,
	scope map[string]struct{},
	requireComplete bool,
) error {
	if values == nil {
		return errors.New("nil values")
	}

	for name, value := range values {
		contract, ok := contracts[name]
		if !ok {
			return fmt.Errorf("unknown port %q", name)
		}

		if err := contract.Schema.Validate(value); err != nil {
			return fmt.Errorf("port %q: %w", name, err)
		}
	}

	if !requireComplete {
		return nil
	}

	for name := range contracts {
		if scope != nil {
			if _, needed := scope[name]; !needed {
				continue
			}
		}

		if _, ok := values[name]; !ok {
			return fmt.Errorf("missing port %q", name)
		}
	}

	return nil
}

func inputValueIsEmpty(value Value) bool {
	switch value.Kind() {
	case ValueString:
		return bytes.Equal(value.raw, []byte(`""`))
	case ValueArray:
		return bytes.Equal(value.raw, []byte(`[]`))
	case ValueObject:
		return bytes.Equal(value.raw, []byte(`{}`))
	case ValueInvalid, ValueNull, ValueBool, ValueNumber:
		return false
	}

	return false
}

func projectCompositeInputSchemas(
	inputs map[string]WorkflowInput,
	bindings map[string]Binding,
) map[string]PortSchema {
	projected := make(map[string]PortSchema, len(inputs))
	for name, input := range inputs {
		if input.Required {
			projected[name] = input.Schema

			continue
		}

		if _, bound := bindings[name]; bound {
			projected[name] = input.Schema
		}
	}

	return projected
}

func requiredWorkflowInputs(schemas map[string]PortSchema) map[string]WorkflowInput {
	inputs := make(map[string]WorkflowInput, len(schemas))
	for name, schema := range schemas {
		inputs[name] = WorkflowInput{Schema: schema, Required: true}
	}

	return inputs
}
