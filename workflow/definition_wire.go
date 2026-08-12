package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type definitionV1Alpha1Wire struct {
	Schema   string                   `json:"schema"`
	ID       DefinitionID             `json:"id"`
	Revision Revision                 `json:"revision"`
	Name     string                   `json:"name"`
	Inputs   map[string]PortSchema    `json:"inputs"`
	Outputs  map[string]OutputBinding `json:"outputs"`
	Nodes    []NodeDefinition         `json:"nodes"`
	Edges    []ControlEdge            `json:"edges"`
	Limits   Limits                   `json:"limits"`
}

type workflowInputV1Alpha2Wire struct {
	Schema   PortSchema      `json:"schema"`
	Required bool            `json:"required,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

type definitionV1Alpha2Wire struct {
	Schema   string                               `json:"schema"`
	ID       DefinitionID                         `json:"id"`
	Revision Revision                             `json:"revision"`
	Name     string                               `json:"name"`
	Inputs   map[string]workflowInputV1Alpha2Wire `json:"inputs"`
	Outputs  map[string]OutputBinding             `json:"outputs"`
	Nodes    []NodeDefinition                     `json:"nodes"`
	Edges    []ControlEdge                        `json:"edges"`
	Limits   Limits                               `json:"limits"`
}

// MarshalJSON writes the exact wire shape selected by Definition.Schema.
func (d Definition) MarshalJSON() ([]byte, error) {
	switch d.Schema {
	case SchemaV1Alpha1:
		inputs := make(map[string]PortSchema, len(d.Inputs))
		for name, input := range d.Inputs {
			if !input.Required || effectiveWorkflowInputDefault(input) != nil {
				return nil, fmt.Errorf(
					"%w: v1alpha1 input %q must be required without a default",
					ErrInvalidDefinition,
					name,
				)
			}

			inputs[name] = input.Schema
		}

		return json.Marshal(definitionV1Alpha1Wire{
			Schema: d.Schema, ID: d.ID, Revision: d.Revision, Name: d.Name,
			Inputs: inputs, Outputs: d.Outputs, Nodes: d.Nodes, Edges: d.Edges, Limits: d.Limits,
		})
	case SchemaV1Alpha2:
		inputs := make(map[string]workflowInputV1Alpha2Wire, len(d.Inputs))
		for name, input := range d.Inputs {
			if input.Default != nil && !input.Default.IsValid() {
				return nil, fmt.Errorf(
					"%w: input %q has invalid default",
					ErrInvalidDefinition,
					name,
				)
			}

			wire := workflowInputV1Alpha2Wire{Schema: input.Schema, Required: input.Required}
			if defaultValue := effectiveWorkflowInputDefault(input); defaultValue != nil {
				wire.Default = defaultValue.RawJSON()
			}

			inputs[name] = wire
		}

		return json.Marshal(definitionV1Alpha2Wire{
			Schema: d.Schema, ID: d.ID, Revision: d.Revision, Name: d.Name,
			Inputs: inputs, Outputs: d.Outputs, Nodes: d.Nodes, Edges: d.Edges, Limits: d.Limits,
		})
	default:
		return nil, fmt.Errorf("%w: unsupported schema %q", ErrInvalidDefinition, d.Schema)
	}
}

// UnmarshalJSON decodes only the exact wire shape selected by the schema field.
func (d *Definition) UnmarshalJSON(data []byte) error {
	if d == nil {
		return fmt.Errorf("%w: nil Definition", ErrInvalidDefinition)
	}

	var header struct {
		Schema string `json:"schema"`
	}
	if err := decodeJSON(data, &header, definitionJSONLimits(), false); err != nil {
		return err
	}

	switch header.Schema {
	case SchemaV1Alpha1:
		var wire definitionV1Alpha1Wire
		if err := decodeJSON(data, &wire, definitionJSONLimits(), true); err != nil {
			return err
		}

		inputs := make(map[string]WorkflowInput, len(wire.Inputs))
		for name, schema := range wire.Inputs {
			inputs[name] = WorkflowInput{Schema: schema, Required: true}
		}

		*d = definitionFromV1Alpha1Wire(wire, inputs)
	case SchemaV1Alpha2:
		var wire definitionV1Alpha2Wire
		if err := decodeJSON(data, &wire, definitionJSONLimits(), true); err != nil {
			return err
		}

		inputs := make(map[string]WorkflowInput, len(wire.Inputs))
		for name, input := range wire.Inputs {
			contract := WorkflowInput{Schema: input.Schema, Required: input.Required}
			if len(input.Default) != 0 && !bytes.Equal(bytes.TrimSpace(input.Default), []byte("null")) {
				value, err := ParseValue(input.Default)
				if err != nil {
					return fmt.Errorf("input %q default: %w", name, err)
				}

				contract.Default = &value
			}

			inputs[name] = contract
		}

		*d = definitionFromV1Alpha2Wire(wire, inputs)
	default:
		return fmt.Errorf("unsupported schema %q", header.Schema)
	}

	return nil
}

func definitionJSONLimits() jsonLimits {
	limits := defaultJSONLimits
	limits.maxBytes = maxDefinitionBytes

	return limits
}

func definitionFromV1Alpha1Wire(
	wire definitionV1Alpha1Wire,
	inputs map[string]WorkflowInput,
) Definition {
	return Definition{
		Schema: wire.Schema, ID: wire.ID, Revision: wire.Revision, Name: wire.Name,
		Inputs: inputs, Outputs: wire.Outputs, Nodes: wire.Nodes, Edges: wire.Edges, Limits: wire.Limits,
	}
}

func definitionFromV1Alpha2Wire(
	wire definitionV1Alpha2Wire,
	inputs map[string]WorkflowInput,
) Definition {
	return Definition{
		Schema: wire.Schema, ID: wire.ID, Revision: wire.Revision, Name: wire.Name,
		Inputs: inputs, Outputs: wire.Outputs, Nodes: wire.Nodes, Edges: wire.Edges, Limits: wire.Limits,
	}
}
