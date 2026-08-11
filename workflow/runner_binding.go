package workflow

import "fmt"

func (e *execution) resolveInputs(index int) (map[string]Value, error) {
	node := e.plan.nodes[index]

	values := make(map[string]Value, len(node.bindings))
	for name, binding := range node.bindings {
		value, present := e.resolveBinding(binding)
		if !present {
			if node.isMerge {
				continue
			}

			return nil, fmt.Errorf("input %q is unavailable", name)
		}

		if err := node.spec.Inputs[name].Validate(value); err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		values[name] = value
	}

	return values, nil
}

func (e *execution) resolveBinding(binding Binding) (Value, bool) {
	var value Value

	switch binding.Source {
	case BindingLiteral:
		value = *binding.Value
	case BindingWorkflowInput:
		var ok bool

		value, ok = e.input[binding.Port]
		if !ok {
			return Value{}, false
		}
	case BindingNodeOutput:
		sourceIndex := e.plan.nodeIndex[binding.Node]

		var ok bool

		value, ok = e.outputs[sourceIndex][binding.Port]
		if !ok {
			return Value{}, false
		}
	case BindingLoopVariable:
		if e.loop == nil {
			return Value{}, false
		}

		var ok bool

		value, ok = e.loop.value(binding.Port)
		if !ok {
			return Value{}, false
		}
	default:
		return Value{}, false
	}

	if len(binding.Path) == 0 {
		return value, true
	}

	return value.Lookup(binding.Path...)
}
