package workflow

import (
	"context"
	"errors"
)

const maxSelectorCases = 32

// SelectorCase maps one structured predicate to a control route.
type SelectorCase struct {
	Route     string    `json:"route"`
	Predicate Predicate `json:"predicate"`
}

// SelectorConfig defines ordered cases and one fallback route.
type SelectorConfig struct {
	Cases        []SelectorCase `json:"cases"`
	DefaultRoute string         `json:"default_route"`
}

// SelectorNode chooses the first matching route or its default route.
type SelectorNode struct{}

// Spec implements [NodeType].
func (SelectorNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key:         NodeTypeSelector,
		Version:     BuiltinNodeVersion,
		DisplayName: "Selector",
	}
}

// Compile implements [NodeType].
func (SelectorNode) Compile(
	_ context.Context,
	_ CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	var config SelectorConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if err := validateSelectorConfig(config, definition.Inputs); err != nil {
		return nil, compileNodeError(definition.ID, "selector config: %v", err)
	}

	anySchema, err := ParsePortSchema([]byte("true"))
	if err != nil {
		return nil, compileNodeError(definition.ID, "prepare input schema: %v", err)
	}

	inputs := make(map[string]PortSchema, len(definition.Inputs))
	for name := range definition.Inputs {
		inputs[name] = anySchema
	}

	routes := make([]string, 0, len(config.Cases)+1)
	for _, selectorCase := range config.Cases {
		routes = append(routes, selectorCase.Route)
	}

	routes = append(routes, config.DefaultRoute)

	return &compiledSelector{
		spec: NodeSpec{
			Inputs:  inputs,
			Outputs: map[string]PortSchema{},
			Routes:  routes,
		},
		config: config,
	}, nil
}

type compiledSelector struct {
	spec   NodeSpec
	config SelectorConfig
}

func (n *compiledSelector) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledSelector) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	for _, selectorCase := range n.config.Cases {
		matched, err := evaluatePredicate(selectorCase.Predicate, input.Values)
		if err != nil {
			return NodeOutput{}, err
		}

		if matched {
			return NodeOutput{
				Values: map[string]Value{},
				Route:  selectorCase.Route,
			}, nil
		}
	}

	return NodeOutput{
		Values: map[string]Value{},
		Route:  n.config.DefaultRoute,
	}, nil
}

func validateSelectorConfig(config SelectorConfig, inputs map[string]Binding) error {
	if len(config.Cases) == 0 {
		return errors.New("cases are required")
	}

	if len(config.Cases) > maxSelectorCases {
		return errors.New("case limit exceeded")
	}

	if !validIdentifier(config.DefaultRoute) {
		return errors.New("default route must be a valid name")
	}

	seen := map[string]struct{}{config.DefaultRoute: {}}
	for _, selectorCase := range config.Cases {
		if !validIdentifier(selectorCase.Route) {
			return errors.New("case route must be a valid name")
		}

		if _, duplicate := seen[selectorCase.Route]; duplicate {
			return errors.New("routes must be distinct")
		}

		seen[selectorCase.Route] = struct{}{}

		if err := validatePredicate(selectorCase.Predicate, inputs, 0, new(int)); err != nil {
			return err
		}
	}

	return nil
}

var _ CompiledNode = (*compiledSelector)(nil)
