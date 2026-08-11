package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const (
	nodeDebugDefinitionStrategy = "pips.workflow/node-debug-definition/v1"
	nodeDebugPlanStrategy       = "pips.workflow/node-debug-plan/v2"
	legacyNodeDebugPlanStrategy = "pips.workflow/node-debug-plan/v1"
)

func resolveNodeDebugTarget(source *Plan, path []NodeID) (*Plan, int, error) {
	current := source

	for position, nodeID := range path {
		index, ok := current.nodeIndex[nodeID]
		if !ok {
			return nil, 0, fmt.Errorf(
				"%w: node debug target path references unknown node %q",
				ErrCompile,
				nodeID,
			)
		}

		if position == len(path)-1 {
			return current, index, nil
		}

		node := current.nodes[index]
		if !isCompositeNodeType(node.definition.Type) {
			return nil, 0, fmt.Errorf(
				"%w: node debug target path continues through non-composite node %q",
				ErrCompile,
				nodeID,
			)
		}

		children := childPlans(node.executor)
		if len(children) != 1 || children[0] == nil {
			return nil, 0, fmt.Errorf(
				"%w: node debug composite %q does not have exactly one child plan",
				ErrCompile,
				nodeID,
			)
		}

		current = children[0]
	}

	return nil, 0, fmt.Errorf("%w: node debug target path is empty", ErrCompile)
}

func validateNodeDebugTarget(definition NodeDefinition) error {
	switch definition.Type {
	case NodeTypeStart, NodeTypeEnd, NodeTypeBreak, NodeTypeContinue, NodeTypeSetVariable:
		return fmt.Errorf(
			"%w: node %q of type %q cannot be debugged outside its workflow context",
			ErrCompile,
			definition.ID,
			definition.Type,
		)
	default:
		return nil
	}
}

func buildNodeDebugPlan(
	source *Plan,
	containing *Plan,
	targetIndex int,
	target NodePath,
) (*NodeDebugPlan, error) {
	parts, err := prepareNodeDebugPlanParts(source, containing, targetIndex)
	if err != nil {
		return nil, err
	}

	definitionFingerprint, err := nodeDebugDefinitionFingerprint(
		source,
		target,
		parts.definition,
		parts.spec,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint node debug definition: %w", ErrCompile, err)
	}

	_, before := containing.interruptBefore[targetIndex]
	_, after := containing.interruptAfter[targetIndex]

	planFingerprint, err := nodeDebugPlanFingerprint(
		source,
		definitionFingerprint,
		parts.spec.OptionalInputs,
		before,
		after,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint node debug plan: %w", ErrCompile, err)
	}

	legacyPlanFingerprint, err := legacyNodeDebugPlanFingerprint(
		source,
		definitionFingerprint,
		parts.spec.OptionalInputs,
		before,
		after,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint legacy node debug plan: %w", ErrCompile, err)
	}

	internalNode := planNode{
		definition:  cloneNodeDefinition(parts.definition),
		executor:    parts.selected.executor,
		spec:        cloneNodeSpec(parts.selected.spec),
		bindings:    cloneBindings(parts.bindings),
		isMerge:     parts.selected.isMerge,
		isComposite: parts.selected.isComposite,
	}
	internalPlan := &Plan{
		definition:                    parts.snapshot,
		definitionFingerprint:         definitionFingerprint,
		registryFingerprint:           source.registryFingerprint,
		referencedContractFingerprint: source.referencedContractFingerprint,
		fingerprint:                   planFingerprint,
		legacyFingerprint:             legacyPlanFingerprint,
		nodes:                         []planNode{internalNode},
		nodeIndex:                     map[NodeID]int{parts.definition.ID: 0},
		edges:                         []planEdge{},
		incoming:                      [][]int{{}},
		outgoing:                      [][]int{{}},
		startIndex:                    0,
		endIndex:                      0,
		interruptBefore:               interruptSet(before),
		interruptAfter:                interruptSet(after),
	}
	internalPlan.nodeDebug = &nodeDebugPlanMetadata{
		fingerprint:       planFingerprint,
		legacyFingerprint: legacyPlanFingerprint,
		target:            NewNodePath(target.nodes...),
		spec:              cloneNodeDebugSpec(parts.spec),
	}

	return &NodeDebugPlan{
		sourcePlanFingerprint: source.fingerprint,
		target:                NewNodePath(target.nodes...),
		spec:                  cloneNodeDebugSpec(parts.spec),
		plan:                  internalPlan,
		globalLimits:          source.definition.Limits,
	}, nil
}

type nodeDebugPlanParts struct {
	selected   planNode
	definition NodeDefinition
	bindings   map[string]Binding
	spec       NodeDebugSpec
	snapshot   Definition
}

func prepareNodeDebugPlanParts(
	source *Plan,
	containing *Plan,
	targetIndex int,
) (nodeDebugPlanParts, error) {
	selected := containing.nodes[targetIndex]
	definition := cloneNodeDefinition(selected.definition)
	bindings := make(map[string]Binding, len(selected.bindings))
	exposedInputs := make(map[string]PortSchema)
	optionalInputs := make([]string, 0)

	for name, sourceBinding := range selected.bindings {
		binding := cloneBinding(sourceBinding)
		if binding.Source != BindingLiteral {
			binding = Binding{Source: BindingWorkflowInput, Port: name}
			exposedInputs[name] = selected.spec.Inputs[name]

			if selected.isMerge {
				optionalInputs = append(optionalInputs, name)
			}
		}

		bindings[name] = binding
	}

	slices.Sort(optionalInputs)

	definition.Inputs = cloneBindings(bindings)

	spec := NodeDebugSpec{
		Inputs:         cloneSchemas(exposedInputs),
		OptionalInputs: slices.Clone(optionalInputs),
		Outputs:        cloneSchemas(selected.spec.Outputs),
		Routes:         slices.Clone(selected.spec.Routes),
	}
	internalDefinition := Definition{
		Schema:   source.definition.Schema,
		ID:       source.definition.ID,
		Revision: source.definition.Revision,
		Name:     source.definition.Name,
		Inputs:   cloneSchemas(exposedInputs),
		Outputs:  map[string]OutputBinding{},
		Nodes:    []NodeDefinition{cloneNodeDefinition(definition)},
		Edges:    []ControlEdge{},
		Limits:   containing.definition.Limits,
	}

	snapshot, err := cloneDefinition(internalDefinition)
	if err != nil {
		return nodeDebugPlanParts{}, fmt.Errorf(
			"%w: node debug definition: %w",
			ErrCompile,
			err,
		)
	}

	return nodeDebugPlanParts{
		selected:   selected,
		definition: definition,
		bindings:   bindings,
		spec:       spec,
		snapshot:   snapshot,
	}, nil
}

func nodeDebugDefinitionFingerprint(
	source *Plan,
	target NodePath,
	definition NodeDefinition,
	spec NodeDebugSpec,
) (string, error) {
	return nodeDebugFingerprint(struct {
		Strategy         string                `json:"strategy"`
		SourceDefinition string                `json:"source_definition"`
		Target           []NodeID              `json:"target"`
		Node             NodeDefinition        `json:"node"`
		Inputs           map[string]PortSchema `json:"inputs"`
		Outputs          map[string]PortSchema `json:"outputs"`
		Routes           []string              `json:"routes"`
	}{
		Strategy:         nodeDebugDefinitionStrategy,
		SourceDefinition: source.definitionFingerprint,
		Target:           target.Nodes(),
		Node:             cloneNodeDefinition(definition),
		Inputs:           cloneSchemas(spec.Inputs),
		Outputs:          cloneSchemas(spec.Outputs),
		Routes:           slices.Clone(spec.Routes),
	})
}

func nodeDebugPlanFingerprint(
	source *Plan,
	definitionFingerprint string,
	optionalInputs []string,
	before bool,
	after bool,
) (string, error) {
	return nodeDebugFingerprint(struct {
		Strategy        string   `json:"strategy"`
		Definition      string   `json:"definition"`
		Contract        string   `json:"contract"`
		SourcePlan      string   `json:"source_plan"`
		OptionalInputs  []string `json:"optional_inputs"`
		InterruptBefore bool     `json:"interrupt_before"`
		InterruptAfter  bool     `json:"interrupt_after"`
	}{
		Strategy:        nodeDebugPlanStrategy,
		Definition:      definitionFingerprint,
		Contract:        source.referencedContractFingerprint,
		SourcePlan:      source.fingerprint,
		OptionalInputs:  slices.Clone(optionalInputs),
		InterruptBefore: before,
		InterruptAfter:  after,
	})
}

func legacyNodeDebugPlanFingerprint(
	source *Plan,
	definitionFingerprint string,
	optionalInputs []string,
	before bool,
	after bool,
) (string, error) {
	return nodeDebugFingerprint(struct {
		Strategy        string   `json:"strategy"`
		Definition      string   `json:"definition"`
		Registry        string   `json:"registry"`
		SourcePlan      string   `json:"source_plan"`
		OptionalInputs  []string `json:"optional_inputs"`
		InterruptBefore bool     `json:"interrupt_before"`
		InterruptAfter  bool     `json:"interrupt_after"`
	}{
		Strategy:        legacyNodeDebugPlanStrategy,
		Definition:      definitionFingerprint,
		Registry:        source.registryFingerprint,
		SourcePlan:      source.legacyFingerprint,
		OptionalInputs:  slices.Clone(optionalInputs),
		InterruptBefore: before,
		InterruptAfter:  after,
	})
}

func cloneBindings(bindings map[string]Binding) map[string]Binding {
	cloned := make(map[string]Binding, len(bindings))
	for name, binding := range bindings {
		cloned[name] = cloneBinding(binding)
	}

	return cloned
}

func interruptSet(enabled bool) map[int]struct{} {
	if !enabled {
		return nil
	}

	return map[int]struct{}{0: {}}
}

func nodeDebugFingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
}

func validateNodeDebugInputs(values map[string]Value, spec NodeDebugSpec) error {
	if values == nil {
		return errors.New("nil values")
	}

	optional := make(map[string]struct{}, len(spec.OptionalInputs))
	for _, name := range spec.OptionalInputs {
		optional[name] = struct{}{}
	}

	for name, value := range values {
		schema, ok := spec.Inputs[name]
		if !ok {
			return fmt.Errorf("unknown port %q", name)
		}

		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("port %q: %w", name, err)
		}
	}

	for name := range spec.Inputs {
		if _, ok := values[name]; ok {
			continue
		}

		if _, ok := optional[name]; !ok {
			return fmt.Errorf("missing port %q", name)
		}
	}

	return nil
}

func materializeNodeDebugInputs(
	plan *NodeDebugPlan,
	values map[string]Value,
) (map[string]Value, error) {
	if err := validateNodeDebugInputs(values, plan.spec); err != nil {
		return nil, err
	}

	node := plan.plan.nodes[0]

	resolved := make(map[string]Value, len(node.bindings))
	for name, binding := range node.bindings {
		var (
			value   Value
			present bool
		)

		switch binding.Source {
		case BindingLiteral:
			if binding.Value != nil {
				value = *binding.Value
				present = true
			}
		case BindingWorkflowInput:
			value, present = values[binding.Port]
		case BindingNodeOutput, BindingLoopVariable:
			return nil, fmt.Errorf("input %q has an invalid node debug binding", name)
		default:
			return nil, fmt.Errorf("input %q has unknown binding source %q", name, binding.Source)
		}

		if present && len(binding.Path) > 0 {
			value, present = value.Lookup(binding.Path...)
		}

		if !present {
			if node.isMerge {
				continue
			}

			return nil, fmt.Errorf("input %q is unavailable", name)
		}

		if err := node.spec.Inputs[name].Validate(value); err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		resolved[name] = value
	}

	return resolved, nil
}
