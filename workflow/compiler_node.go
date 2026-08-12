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
	if err := p.preflightNodes(session.registry, interrupts); err != nil {
		return err
	}

	outputSchemas := make(map[string]PortSchema, len(p.definition.Outputs))
	for name, output := range p.definition.Outputs {
		outputSchemas[name] = output.Schema
	}

	baseContext := nodeCompileContext{
		inputs:   workflowInputSchemas(p.definition.Inputs),
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

	return nil
}

func (p *Plan) preflightNodes(registry *Registry, interrupts *interruptPolicy) error {
	var collector compileErrorCollector

	startCount := 0
	endCount := 0

	for _, definition := range p.definition.Nodes {
		_, ok := registry.NodeType(definition.Type, definition.Version)
		if !ok {
			collector.addIssue(newNodeCompileIssue(
				CompileIssueUnknownNodeType,
				NewNodePath(definition.ID),
				fmt.Sprintf(
					"node %q: unknown node type %q@%q",
					definition.ID,
					definition.Type,
					definition.Version,
				),
			), nil)
		}

		switch definition.Type {
		case NodeTypeStart:
			startCount++
		case NodeTypeEnd:
			endCount++
		default:
		}
	}

	if startCount != 1 {
		collector.addIssue(newDefinitionCompileIssue(
			CompileIssueInvalidDefinition,
			fmt.Sprintf("got %d Start nodes, want exactly one", startCount),
		), nil)
	}

	if endCount != 1 {
		collector.addIssue(newDefinitionCompileIssue(
			CompileIssueInvalidDefinition,
			fmt.Sprintf("got %d End nodes, want exactly one", endCount),
		), nil)
	}

	p.preflightInterrupts(registry, interrupts, &collector)

	return collector.err()
}

func (p *Plan) preflightInterrupts(
	registry *Registry,
	interrupts *interruptPolicy,
	collector *compileErrorCollector,
) {
	if interrupts == nil {
		return
	}

	definitions := make(map[NodeID]NodeDefinition, len(p.definition.Nodes))
	for _, definition := range p.definition.Nodes {
		definitions[definition.ID] = definition
	}

	for _, nodeID := range sortedNodeIDs(interrupts.children) {
		definition, ok := definitions[nodeID]
		if !ok {
			path := NewNodePath(nodeID)
			collector.addIssue(newNodeCompileIssue(
				CompileIssueInvalidInterrupt,
				path,
				nodeCompileMessage(path, "interrupt path references unknown node"),
			), nil)

			continue
		}

		child := interrupts.children[nodeID]
		if _, registered := registry.NodeType(definition.Type, definition.Version); registered &&
			len(child.children) > 0 && !isBuiltinCompositeDefinition(definition) {
			path := NewNodePath(nodeID)
			collector.addIssue(newNodeCompileIssue(
				CompileIssueInvalidInterrupt,
				path,
				nodeCompileMessage(path, "interrupt path continues through non-composite node"),
			), nil)
		}
	}
}

func isBuiltinCompositeDefinition(definition NodeDefinition) bool {
	return definition.Version == BuiltinNodeVersion && isCompositeNodeType(definition.Type)
}

func sortedNodeIDs(values map[NodeID]*interruptPolicy) []NodeID {
	ids := make([]NodeID, 0, len(values))
	for nodeID := range values {
		ids = append(ids, nodeID)
	}

	slices.Sort(ids)

	return ids
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
		return compileNodeFailure(
			CompileIssueInvalidInterrupt,
			definition.ID,
			nil,
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

func (p *Plan) compileNode(
	ctx context.Context,
	registry *Registry,
	compileContext nodeCompileContext,
	definition NodeDefinition,
) (planNode, error) {
	nodeType, ok := registry.NodeType(definition.Type, definition.Version)
	if !ok {
		return planNode{}, compileNodeFailure(
			CompileIssueUnknownNodeType,
			definition.ID,
			nil,
			"unknown node type %q@%q",
			definition.Type,
			definition.Version,
		)
	}

	executor, err := nodeType.Compile(ctx, compileContext, cloneNodeDefinition(definition))
	if err != nil {
		if ctxErr := compileContextError(ctx, err); ctxErr != nil {
			return planNode{}, ctxErr
		}

		fallbackPath := NewNodePath(definition.ID)
		fallback := newNodeCompileIssue(
			CompileIssueInvalidNodeConfig,
			fallbackPath,
			nodeCompileMessage(fallbackPath, err.Error()),
		)

		return planNode{}, compileErrorFromFailure(err, fallback)
	}

	if isNilInterface(executor) {
		return planNode{}, compileNodeFailure(
			CompileIssueInvalidNodeSpec,
			definition.ID,
			nil,
			"node type returned nil executor",
		)
	}

	spec := cloneNodeSpec(executor.Spec())
	if err := validateNodeSpec(spec); err != nil {
		return planNode{}, compileNodeFailure(
			CompileIssueInvalidNodeSpec,
			definition.ID,
			err,
			"invalid compiled spec: %v",
			err,
		)
	}

	return planNode{
		definition:  definition,
		executor:    executor,
		spec:        spec,
		bindings:    p.nodeBindings(definition, spec),
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
			return compileNodeFailure(
				CompileIssueInvalidNodeSpec,
				definition.ID,
				nil,
				"multiple Start nodes",
			)
		}

		p.startIndex = index
	case NodeTypeEnd:
		if p.endIndex >= 0 {
			return compileNodeFailure(
				CompileIssueInvalidNodeSpec,
				definition.ID,
				nil,
				"multiple End nodes",
			)
		}

		p.endIndex = index
	case NodeTypeAction, NodeTypeCondition, NodeTypeMerge, NodeTypeSelector,
		NodeTypeSubWorkflow, NodeTypeBatch, NodeTypeLoop, NodeTypeBreak,
		NodeTypeContinue, NodeTypeSetVariable:
	}

	return nil
}

func (p *Plan) nodeBindings(definition NodeDefinition, spec NodeSpec) map[string]Binding {
	bindings := make(map[string]Binding, len(spec.Inputs))

	switch definition.Type {
	case NodeTypeStart:
		for name := range spec.Inputs {
			bindings[name] = Binding{Source: BindingWorkflowInput, Port: name}
		}
	case NodeTypeEnd:
		for name, output := range p.definition.Outputs {
			bindings[name] = cloneBinding(output.Binding)
		}
	default:
		for name, binding := range definition.Inputs {
			bindings[name] = cloneBinding(binding)
		}
	}

	return bindings
}

func (p *Plan) compileEdges() error {
	seen := make(map[string]struct{}, len(p.definition.Edges))
	edges := make([]planEdge, 0, len(p.definition.Edges))
	incoming := make([][]int, len(p.nodes))
	outgoing := make([][]int, len(p.nodes))

	var collector compileErrorCollector

	for _, definition := range p.definition.Edges {
		from, fromOK := p.nodeIndex[definition.From.Node]
		to, toOK := p.nodeIndex[definition.To]

		if !fromOK {
			collector.add(
				compileControlPathFailure(
					CompileIssueInvalidControlPath,
					definition,
					nil,
					"references unknown source node %q",
					definition.From.Node,
				),
				CompileIssue{},
			)
		}

		if !toOK {
			collector.add(
				compileControlPathFailure(
					CompileIssueInvalidControlPath,
					definition,
					nil,
					"references unknown target node %q",
					definition.To,
				),
				CompileIssue{},
			)
		}

		if !fromOK || !toOK {
			continue
		}

		if from == to {
			collector.add(
				compileControlPathFailure(
					CompileIssueInvalidControlPath,
					definition,
					nil,
					"node %q has a self edge",
					definition.To,
				),
				CompileIssue{},
			)
		}

		if !p.routeAllowed(from, definition.From.Route) {
			collector.add(
				compileControlPathFailure(
					CompileIssueInvalidControlPath,
					definition,
					nil,
					"unknown route %q",
					definition.From.Route,
				),
				CompileIssue{},
			)
		}

		identity := fmt.Sprintf(
			"%s/%s/%s",
			definition.From.Node,
			definition.From.Route,
			definition.To,
		)
		if _, duplicate := seen[identity]; duplicate {
			collector.add(
				compileControlPathFailure(
					CompileIssueInvalidControlPath,
					definition,
					nil,
					"duplicate control edge",
				),
				CompileIssue{},
			)
		}

		seen[identity] = struct{}{}

		edgeIndex := len(edges)
		edges = append(edges, planEdge{from: from, to: to, route: definition.From.Route})
		outgoing[from] = append(outgoing[from], edgeIndex)
		incoming[to] = append(incoming[to], edgeIndex)
	}

	if err := collector.err(); err != nil {
		return err
	}

	p.edges = edges
	p.incoming = incoming
	p.outgoing = outgoing

	return nil
}

func (p *Plan) routeAllowed(nodeIndex int, route string) bool {
	node := p.nodes[nodeIndex]
	if route == RouteError {
		return node.definition.Policy.Error == ErrorRoute
	}

	return slices.Contains(node.spec.Routes, route)
}
