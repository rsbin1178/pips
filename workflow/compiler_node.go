package workflow

import (
	"context"
	"fmt"
	"slices"
)

func (p *Plan) compileNodes(
	ctx context.Context,
	session *compileSession,
	interrupts *interruptPolicy,
	actions *actionLookupRecorder,
) error {
	outputSchemas := make(map[string]PortSchema, len(p.definition.Outputs))
	for name, output := range p.definition.Outputs {
		outputSchemas[name] = output.Schema
	}

	baseContext := nodeCompileContext{
		inputs:   p.definition.Inputs,
		outputs:  outputSchemas,
		registry: session.registry,
		actions:  actions,
		session:  session,
		loop:     p.loop,
	}

	for index, definition := range p.definition.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}

		compileContext := baseContext
		if interrupts != nil {
			compileContext.interrupts = interrupts.children[definition.ID]
		}

		if err := p.configureNodeInterrupt(index, definition, compileContext.interrupts); err != nil {
			return err
		}

		node, err := p.compileNode(ctx, session.registry, compileContext, definition)
		if err != nil {
			return err
		}

		p.nodeIndex[definition.ID] = index
		p.nodes[index] = node

		if err := p.recordEndpoint(index, definition); err != nil {
			return err
		}
	}

	if p.startIndex < 0 || p.endIndex < 0 {
		return fmt.Errorf("%w: exactly one Start and one End node are required", ErrCompile)
	}

	if err := p.rejectUnknownInterruptNodes(interrupts); err != nil {
		return err
	}

	return nil
}

func (p *Plan) configureNodeInterrupt(
	index int,
	definition NodeDefinition,
	interrupts *interruptPolicy,
) error {
	if interrupts == nil {
		return nil
	}

	if len(interrupts.children) > 0 && !isCompositeNodeType(definition.Type) {
		return compileNodeError(
			definition.ID,
			"interrupt path continues through non-composite node",
		)
	}

	if interrupts.before {
		if p.interruptBefore == nil {
			p.interruptBefore = make(map[int]struct{})
		}

		p.interruptBefore[index] = struct{}{}
	}

	if interrupts.after {
		if p.interruptAfter == nil {
			p.interruptAfter = make(map[int]struct{})
		}

		p.interruptAfter[index] = struct{}{}
	}

	return nil
}

func (p *Plan) rejectUnknownInterruptNodes(interrupts *interruptPolicy) error {
	if interrupts == nil {
		return nil
	}

	for nodeID := range interrupts.children {
		if _, ok := p.nodeIndex[nodeID]; !ok {
			return compileNodeError(nodeID, "interrupt path references unknown node")
		}
	}

	return nil
}

func (p *Plan) compileNode(
	ctx context.Context,
	registry *Registry,
	compileContext nodeCompileContext,
	definition NodeDefinition,
) (planNode, error) {
	nodeType, ok := registry.NodeType(definition.Type, definition.Version)
	if !ok {
		return planNode{}, compileNodeError(
			definition.ID,
			"unknown node type %q@%q",
			definition.Type,
			definition.Version,
		)
	}

	executor, err := nodeType.Compile(ctx, compileContext, cloneNodeDefinition(definition))
	if err != nil {
		return planNode{}, err
	}

	if isNilInterface(executor) {
		return planNode{}, compileNodeError(definition.ID, "node type returned nil executor")
	}

	spec := cloneNodeSpec(executor.Spec())
	if err := validateNodeSpec(spec); err != nil {
		return planNode{}, compileNodeError(definition.ID, "invalid compiled spec: %v", err)
	}

	bindings, err := p.nodeBindings(definition, spec)
	if err != nil {
		return planNode{}, err
	}

	if err := validateDefaultOutputs(definition, spec); err != nil {
		return planNode{}, err
	}

	return planNode{
		definition:  definition,
		executor:    executor,
		spec:        spec,
		bindings:    bindings,
		isMerge:     definition.Type == NodeTypeMerge,
		isComposite: isCompiledComposite(executor),
	}, nil
}

func isCompiledComposite(executor CompiledNode) bool {
	_, ok := executor.(compiledCompositeNode)

	return ok
}

func (p *Plan) recordEndpoint(index int, definition NodeDefinition) error {
	switch definition.Type {
	case NodeTypeStart:
		if p.startIndex >= 0 {
			return compileNodeError(definition.ID, "multiple Start nodes")
		}

		p.startIndex = index
	case NodeTypeEnd:
		if p.endIndex >= 0 {
			return compileNodeError(definition.ID, "multiple End nodes")
		}

		p.endIndex = index
	case NodeTypeAction, NodeTypeCondition, NodeTypeMerge, NodeTypeSelector,
		NodeTypeSubWorkflow, NodeTypeBatch, NodeTypeLoop, NodeTypeBreak,
		NodeTypeContinue, NodeTypeSetVariable:
	}

	return nil
}

func (p *Plan) nodeBindings(definition NodeDefinition, spec NodeSpec) (map[string]Binding, error) {
	bindings := make(map[string]Binding, len(spec.Inputs))

	switch definition.Type {
	case NodeTypeStart:
		if len(definition.Inputs) != 0 {
			return nil, compileNodeError(definition.ID, "Start inputs are declared by the Workflow")
		}

		for name := range spec.Inputs {
			bindings[name] = Binding{Source: BindingWorkflowInput, Port: name}
		}
	case NodeTypeEnd:
		if len(definition.Inputs) != 0 {
			return nil, compileNodeError(definition.ID, "End inputs are declared by Workflow outputs")
		}

		for name, output := range p.definition.Outputs {
			bindings[name] = cloneBinding(output.Binding)
		}
	default:
		for name, binding := range definition.Inputs {
			bindings[name] = cloneBinding(binding)
		}
	}

	if len(bindings) != len(spec.Inputs) {
		return nil, compileNodeError(
			definition.ID,
			"got %d input bindings, want %d",
			len(bindings),
			len(spec.Inputs),
		)
	}

	for name := range spec.Inputs {
		if _, ok := bindings[name]; !ok {
			return nil, compileNodeError(definition.ID, "missing input binding %q", name)
		}
	}

	for name := range bindings {
		if _, ok := spec.Inputs[name]; !ok {
			return nil, compileNodeError(definition.ID, "unknown input binding %q", name)
		}
	}

	return bindings, nil
}

func validateDefaultOutputs(definition NodeDefinition, spec NodeSpec) error {
	if definition.Policy.Error != ErrorContinueWithDefault {
		return nil
	}

	if err := validatePortValues(definition.Policy.DefaultOutputs, spec.Outputs); err != nil {
		return compileNodeError(definition.ID, "default outputs: %v", err)
	}

	return nil
}

func (p *Plan) compileEdges() error {
	seen := make(map[string]struct{}, len(p.definition.Edges))
	for _, definition := range p.definition.Edges {
		from, fromOK := p.nodeIndex[definition.From.Node]

		to, toOK := p.nodeIndex[definition.To]
		if !fromOK || !toOK {
			return fmt.Errorf(
				"%w: edge %q/%q -> %q references unknown node",
				ErrCompile,
				definition.From.Node,
				definition.From.Route,
				definition.To,
			)
		}

		if from == to {
			return fmt.Errorf("%w: node %q has a self edge", ErrCompile, definition.To)
		}

		if !p.routeAllowed(from, definition.From.Route) {
			return compileNodeError(
				definition.From.Node,
				"unknown route %q",
				definition.From.Route,
			)
		}

		identity := fmt.Sprintf("%d/%s/%d", from, definition.From.Route, to)
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("%w: duplicate control edge", ErrCompile)
		}

		seen[identity] = struct{}{}

		edgeIndex := len(p.edges)
		p.edges = append(p.edges, planEdge{from: from, to: to, route: definition.From.Route})
		p.outgoing[from] = append(p.outgoing[from], edgeIndex)
		p.incoming[to] = append(p.incoming[to], edgeIndex)
	}

	return nil
}

func (p *Plan) routeAllowed(nodeIndex int, route string) bool {
	node := p.nodes[nodeIndex]
	if route == RouteError {
		return node.definition.Policy.Error == ErrorRoute
	}

	return slices.Contains(node.spec.Routes, route)
}
