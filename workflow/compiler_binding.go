package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

func (p *Plan) validateBindings(topological []int) error {
	dominators := p.dominators(topological)

	var shapeCollector compileErrorCollector

	for _, targetIndex := range topological {
		node := p.nodes[targetIndex]
		shapeCollector.add(
			p.validateNodeBindingShape(node),
			newNodeCompileIssue(
				CompileIssueInvalidBinding,
				NewNodePath(node.definition.ID),
				fmt.Sprintf("node %q: invalid input binding shape", node.definition.ID),
			),
		)
		shapeCollector.add(
			validateDefaultOutputs(node.definition, node.spec),
			newNodeCompileIssue(
				CompileIssueInvalidBinding,
				NewNodePath(node.definition.ID),
				fmt.Sprintf("node %q: invalid default outputs", node.definition.ID),
			),
		)
	}

	if err := shapeCollector.err(); err != nil {
		return err
	}

	var contractCollector compileErrorCollector

	for _, targetIndex := range topological {
		node := p.nodes[targetIndex]
		bindingErr := p.validateNodeBindings(targetIndex, node, dominators)
		contractCollector.add(bindingErr, CompileIssue{})

		if node.isMerge && bindingErr == nil {
			if err := p.validateMergeNode(targetIndex); err != nil {
				contractCollector.add(err, CompileIssue{})
			}
		}
	}

	if err := contractCollector.err(); err != nil {
		return err
	}

	return p.validateLoopVariableAccess(topological)
}

func (p *Plan) validateNodeBindingShape(node planNode) error {
	var collector compileErrorCollector

	switch node.definition.Type {
	case NodeTypeStart:
		if len(node.definition.Inputs) != 0 {
			collector.add(
				compileBindingNodeFailure(
					node.definition.ID,
					"Start inputs are declared by the Workflow",
				),
				CompileIssue{},
			)
		}
	case NodeTypeEnd:
		if len(node.definition.Inputs) != 0 {
			collector.add(
				compileBindingNodeFailure(
					node.definition.ID,
					"End inputs are declared by Workflow outputs",
				),
				CompileIssue{},
			)
		}
	default:
		for _, name := range sortedSchemaNames(node.spec.Inputs) {
			if _, ok := node.bindings[name]; !ok {
				collector.add(
					compileBindingNodeFailure(
						node.definition.ID,
						"missing input binding %q",
						name,
					),
					CompileIssue{},
				)
			}
		}

		for _, name := range sortedBindingNames(node.bindings) {
			if _, ok := node.spec.Inputs[name]; !ok {
				collector.add(
					compileBindingNodeFailure(
						node.definition.ID,
						"unknown input binding %q",
						name,
					),
					CompileIssue{},
				)
			}
		}
	}

	return collector.err()
}

func (p *Plan) validateNodeBindings(targetIndex int, node planNode, dominators [][]bool) error {
	var collector compileErrorCollector

	for _, name := range sortedBindingNames(node.bindings) {
		binding := node.bindings[name]
		if err := p.validateBindingContract(targetIndex, node, name, binding, dominators); err != nil {
			collector.add(err, CompileIssue{})
		}
	}

	return collector.err()
}

func (p *Plan) validateBindingContract(
	targetIndex int,
	node planNode,
	name string,
	binding Binding,
	dominators [][]bool,
) error {
	targetSchema := node.spec.Inputs[name]

	sourceSchema, sourceIndex, err := p.bindingSchema(binding)
	if err != nil {
		return compileBindingNodeFailure(node.definition.ID, "input %q: %v", name, err)
	}

	if binding.Source == BindingLiteral {
		if err := targetSchema.Validate(*binding.Value); err != nil {
			return compileBindingNodeFailure(node.definition.ID, "input %q: %v", name, err)
		}

		return nil
	}

	if !schemaCompatible(sourceSchema, targetSchema) {
		return compileBindingNodeFailure(node.definition.ID, "input %q has incompatible schema", name)
	}

	switch binding.Source {
	case BindingNodeOutput:
		return p.validateNodeOutputAvailability(
			targetIndex,
			node,
			name,
			sourceIndex,
			dominators,
		)
	case BindingNodeError:
		return p.validateNodeErrorAvailability(
			targetIndex,
			node,
			name,
			sourceIndex,
			dominators,
		)
	default:
		return nil
	}
}

func (p *Plan) validateNodeOutputAvailability(
	targetIndex int,
	node planNode,
	name string,
	sourceIndex int,
	dominators [][]bool,
) error {
	if node.isMerge {
		if p.isV2ExclusiveMerge(node) {
			if !p.normalRouteReaches(sourceIndex, targetIndex) {
				return compileBindingNodeFailure(
					node.definition.ID,
					"merge input %q is not from a route-relevant upstream node",
					name,
				)
			}

			return nil
		}

		if !p.isDirectPredecessor(sourceIndex, targetIndex) {
			return compileBindingNodeFailure(
				node.definition.ID,
				"merge input %q is not from an incoming node",
				name,
			)
		}

		return nil
	}

	if !dominators[targetIndex][sourceIndex] ||
		p.reachableFromRoute(sourceIndex, RouteError, targetIndex) {
		return compileBindingNodeFailure(
			node.definition.ID,
			"input %q is not guaranteed to be available",
			name,
		)
	}

	return nil
}

func (p *Plan) validateNodeErrorAvailability(
	targetIndex int,
	node planNode,
	name string,
	sourceIndex int,
	dominators [][]bool,
) error {
	if node.isMerge {
		return p.validateMergeErrorAvailability(targetIndex, node, name, sourceIndex, dominators)
	}

	if !p.nodeErrorGuaranteed(sourceIndex, targetIndex, dominators) {
		return compileBindingNodeFailure(
			node.definition.ID,
			"input %q is not guaranteed to have node error data",
			name,
		)
	}

	return nil
}

func (p *Plan) validateMergeErrorAvailability(
	targetIndex int,
	node planNode,
	name string,
	sourceIndex int,
	dominators [][]bool,
) error {
	merge, ok := node.executor.(*compiledMerge)
	if !ok {
		return compileBindingNodeFailure(
			node.definition.ID,
			"built-in Merge executor has unexpected type",
		)
	}

	if merge.version == MergeNodeVersionV2 && merge.config.Mode == MergeExclusive {
		if !p.reachableFromRoute(sourceIndex, RouteError, targetIndex) {
			return compileBindingNodeFailure(
				node.definition.ID,
				"merge input %q is not from a route-relevant upstream error route",
				name,
			)
		}

		return nil
	}

	if !p.isDirectPredecessorRoute(sourceIndex, targetIndex, RouteError) {
		return compileBindingNodeFailure(
			node.definition.ID,
			"merge input %q is not from an incoming error route",
			name,
		)
	}

	if merge.config.Mode == MergeExclusive {
		return nil
	}

	if !p.nodeErrorGuaranteed(sourceIndex, targetIndex, dominators) {
		return compileBindingNodeFailure(
			node.definition.ID,
			"input %q is not guaranteed to have node error data",
			name,
		)
	}

	return nil
}

func (p *Plan) bindingSchema(binding Binding) (PortSchema, int, error) {
	var (
		schema PortSchema
		err    error
	)

	sourceIndex := -1

	switch binding.Source {
	case BindingLiteral:
		return PortSchema{}, sourceIndex, nil
	case BindingWorkflowInput:
		var ok bool

		input, ok := p.definition.Inputs[binding.Port]
		if !ok {
			return PortSchema{}, sourceIndex, fmt.Errorf("unknown Workflow input %q", binding.Port)
		}

		schema = input.Schema
	case BindingNodeOutput:
		schema, sourceIndex, err = p.nodeOutputBindingSchema(binding)
	case BindingNodeError:
		schema, sourceIndex, err = p.nodeErrorBindingSchema(binding)
	case BindingLoopVariable:
		if p.loop == nil {
			return PortSchema{}, sourceIndex, errors.New("loop variable is unavailable outside a direct Loop body")
		}

		var ok bool

		schema, ok = p.loop.variables[binding.Port]
		if !ok {
			return PortSchema{}, sourceIndex, fmt.Errorf("unknown loop variable %q", binding.Port)
		}
	default:
		return PortSchema{}, sourceIndex, fmt.Errorf("unknown binding source %q", binding.Source)
	}

	if err != nil {
		return PortSchema{}, sourceIndex, err
	}

	if len(binding.Path) == 0 {
		return schema, sourceIndex, nil
	}

	subschema, err := schemaAtPath(schema, binding.Path)
	if err != nil {
		return PortSchema{}, sourceIndex, err
	}

	return subschema, sourceIndex, nil
}

func (p *Plan) nodeOutputBindingSchema(binding Binding) (PortSchema, int, error) {
	sourceIndex, ok := p.nodeIndex[binding.Node]
	if !ok {
		return PortSchema{}, -1, fmt.Errorf("unknown source node %q", binding.Node)
	}

	schema, ok := p.nodes[sourceIndex].spec.Outputs[binding.Port]
	if !ok {
		return PortSchema{}, sourceIndex, fmt.Errorf(
			"unknown node output %q.%s",
			binding.Node,
			binding.Port,
		)
	}

	return schema, sourceIndex, nil
}

func (p *Plan) nodeErrorBindingSchema(binding Binding) (PortSchema, int, error) {
	sourceIndex, ok := p.nodeIndex[binding.Node]
	if !ok {
		return PortSchema{}, -1, fmt.Errorf("unknown source node %q", binding.Node)
	}

	if p.nodes[sourceIndex].definition.Policy.Error != ErrorRoute {
		return PortSchema{}, sourceIndex, fmt.Errorf(
			"source node %q does not use error routing",
			binding.Node,
		)
	}

	schema, err := nodeErrorPortSchema(binding.Port)
	if err != nil {
		return PortSchema{}, sourceIndex, err
	}

	return schema, sourceIndex, nil
}

func (p *Plan) validateLoopControlEdges() error {
	var collector compileErrorCollector

	for index, node := range p.nodes {
		if node.definition.Type != NodeTypeBreak && node.definition.Type != NodeTypeContinue {
			continue
		}

		successEdges := 0

		for _, edgeIndex := range p.outgoing[index] {
			edge := p.edges[edgeIndex]

			if edge.route != RouteSuccess {
				continue
			}

			successEdges++

			if edge.to != p.endIndex {
				definition := ControlEdge{
					From: NodeRoute{Node: node.definition.ID, Route: edge.route},
					To:   p.nodes[edge.to].definition.ID,
				}
				collector.add(
					compileControlPathFailure(
						CompileIssueInvalidGraph,
						definition,
						nil,
						"Break or Continue success edge must target the Loop body End",
					),
					CompileIssue{},
				)
			}
		}

		if successEdges != 1 {
			collector.add(
				compileNodeFailure(
					CompileIssueInvalidGraph,
					node.definition.ID,
					nil,
					"requires exactly one success edge to the Loop body End",
				),
				CompileIssue{},
			)
		}
	}

	return collector.err()
}

type loopVariableAccess struct {
	node  int
	write bool
}

func (p *Plan) validateLoopVariableAccess(topological []int) error {
	if p.loop == nil {
		return nil
	}

	accesses := p.loopVariableAccesses(topological)
	dominators := p.dominators(topological)

	variables := make([]string, 0, len(accesses))
	for variable := range accesses {
		variables = append(variables, variable)
	}

	slices.Sort(variables)

	var collector compileErrorCollector

	for _, variable := range variables {
		variableAccesses := accesses[variable]
		if err := p.validateVariableAccesses(variable, variableAccesses, dominators); err != nil {
			collector.add(err, CompileIssue{})
		}
	}

	return collector.err()
}

func (p *Plan) loopVariableAccesses(topological []int) map[string][]loopVariableAccess {
	accesses := make(map[string][]loopVariableAccess, len(p.loop.variables))

	for _, index := range topological {
		node := p.nodes[index]
		for _, name := range sortedBindingNames(node.bindings) {
			binding := node.bindings[name]
			if binding.Source == BindingLoopVariable {
				accesses[binding.Port] = append(
					accesses[binding.Port],
					loopVariableAccess{node: index},
				)
			}
		}

		setter, ok := node.executor.(*compiledSetVariable)
		if !ok {
			continue
		}

		variables := make([]string, 0, len(setter.targets))
		for variable := range setter.targets {
			variables = append(variables, variable)
		}

		slices.Sort(variables)

		for _, variable := range variables {
			accesses[variable] = append(
				accesses[variable],
				loopVariableAccess{node: index, write: true},
			)
		}
	}

	return accesses
}

func (p *Plan) validateVariableAccesses(
	variable string,
	accesses []loopVariableAccess,
	dominators [][]bool,
) error {
	var collector compileErrorCollector

	for left := range accesses {
		for right := left + 1; right < len(accesses); right++ {
			first := accesses[left]
			second := accesses[right]

			if p.loopAccessesOrderedOrIndependent(first, second, dominators) {
				continue
			}

			collector.add(
				compileDefinitionFailure(
					CompileIssueInvalidBinding,
					nil,
					"loop variable %q has unordered access between nodes %q and %q",
					variable,
					p.nodes[first.node].definition.ID,
					p.nodes[second.node].definition.ID,
				),
				CompileIssue{},
			)
		}
	}

	return collector.err()
}

func (p *Plan) loopAccessesOrderedOrIndependent(
	first loopVariableAccess,
	second loopVariableAccess,
	dominators [][]bool,
) bool {
	if first.node == second.node || (!first.write && !second.write) {
		return true
	}

	if p.reachable(first.node, second.node) || p.reachable(second.node, first.node) {
		return true
	}

	return p.mutuallyExclusive(first.node, second.node, dominators)
}

func (p *Plan) reachable(source, target int) bool {
	if source == target {
		return true
	}

	visited := make([]bool, len(p.nodes))
	queue := []int{source}
	visited[source] = true

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, edgeIndex := range p.outgoing[current] {
			next := p.edges[edgeIndex].to
			if next == target {
				return true
			}

			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}

	return false
}

func (p *Plan) mutuallyExclusive(first, second int, dominators [][]bool) bool {
	for branch := range p.nodes {
		if !dominators[first][branch] || !dominators[second][branch] {
			continue
		}

		firstRoutes := p.routesReaching(branch, first)
		secondRoutes := p.routesReaching(branch, second)

		if len(firstRoutes) == 0 || len(secondRoutes) == 0 {
			continue
		}

		disjoint := true

		for route := range firstRoutes {
			if _, shared := secondRoutes[route]; shared {
				disjoint = false
				break
			}
		}

		if disjoint {
			return true
		}
	}

	return false
}

func (p *Plan) routesReaching(branch, target int) map[string]struct{} {
	routes := make(map[string]struct{})

	for _, edgeIndex := range p.outgoing[branch] {
		edge := p.edges[edgeIndex]
		if edge.to == target || p.reachable(edge.to, target) {
			routes[edge.route] = struct{}{}
		}
	}

	return routes
}

func (p *Plan) dominators(topological []int) [][]bool {
	dominators := make([][]bool, len(p.nodes))
	for index := range p.nodes {
		dominators[index] = make([]bool, len(p.nodes))
	}

	dominators[p.startIndex][p.startIndex] = true

	for _, nodeIndex := range topological {
		if nodeIndex == p.startIndex {
			continue
		}

		for candidate := range p.nodes {
			isCommon := true

			for _, edgeIndex := range p.incoming[nodeIndex] {
				if !dominators[p.edges[edgeIndex].from][candidate] {
					isCommon = false
					break
				}
			}

			dominators[nodeIndex][candidate] = isCommon
		}

		dominators[nodeIndex][nodeIndex] = true
	}

	return dominators
}

func (p *Plan) isDirectPredecessor(source, target int) bool {
	for _, edgeIndex := range p.incoming[target] {
		if p.edges[edgeIndex].from == source {
			return true
		}
	}

	return false
}

func (p *Plan) isDirectPredecessorRoute(source, target int, route string) bool {
	for _, edgeIndex := range p.incoming[target] {
		edge := p.edges[edgeIndex]
		if edge.from == source && edge.route == route {
			return true
		}
	}

	return false
}

func (p *Plan) nodeErrorGuaranteed(source, target int, dominators [][]bool) bool {
	if !dominators[target][source] ||
		!p.reachableFromRoute(source, RouteError, target) {
		return false
	}

	for _, route := range p.nodes[source].spec.Routes {
		if p.reachableFromRoute(source, route, target) {
			return false
		}
	}

	return true
}

func (p *Plan) reachableFromRoute(source int, route string, target int) bool {
	if !p.hasOutgoingRoute(source, route) {
		return false
	}

	visited := make([]bool, len(p.nodes))
	queue := make([]int, 0, len(p.outgoing[source]))

	for _, edgeIndex := range p.outgoing[source] {
		edge := p.edges[edgeIndex]
		if edge.route == route {
			queue = append(queue, edge.to)
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if current == target {
			return true
		}

		if visited[current] {
			continue
		}

		visited[current] = true
		for _, edgeIndex := range p.outgoing[current] {
			queue = append(queue, p.edges[edgeIndex].to)
		}
	}

	return false
}

func (p *Plan) normalRouteReaches(source, target int) bool {
	for _, route := range p.nodes[source].spec.Routes {
		if p.reachableFromRoute(source, route, target) {
			return true
		}
	}

	return false
}

func (p *Plan) isV2ExclusiveMerge(node planNode) bool {
	merge, ok := node.executor.(*compiledMerge)

	return ok && merge.version == MergeNodeVersionV2 && merge.config.Mode == MergeExclusive
}

func (p *Plan) validateMergeNode(nodeIndex int) error {
	node := p.nodes[nodeIndex]

	merge, ok := node.executor.(*compiledMerge)
	if !ok {
		return compileBindingNodeFailure(
			node.definition.ID,
			"built-in Merge executor has unexpected type",
		)
	}

	predecessors := make(map[int]struct{}, len(p.incoming[nodeIndex]))
	for _, edgeIndex := range p.incoming[nodeIndex] {
		predecessors[p.edges[edgeIndex].from] = struct{}{}
	}

	usedPredecessors := make(map[int]struct{}, len(predecessors))
	allowIndirect := p.isV2ExclusiveMerge(node)

	var collector compileErrorCollector

	outputNames := make([]string, 0, len(merge.config.Outputs))
	for outputName := range merge.config.Outputs {
		outputNames = append(outputNames, outputName)
	}

	slices.Sort(outputNames)

	for _, outputName := range outputNames {
		output := merge.config.Outputs[outputName]
		for _, inputName := range output.Sources {
			if err := p.validateMergeSource(
				node,
				outputName,
				inputName,
				output.Schema,
				predecessors,
				usedPredecessors,
				allowIndirect,
			); err != nil {
				collector.add(err, CompileIssue{})
			}
		}
	}

	if !allowIndirect && len(usedPredecessors) != len(predecessors) {
		collector.add(
			compileBindingNodeFailure(
				node.definition.ID,
				"every incoming node must provide a mapped source",
			),
			CompileIssue{},
		)
	}

	return collector.err()
}

func (p *Plan) validateMergeSource(
	node planNode,
	outputName string,
	inputName string,
	outputSchema PortSchema,
	predecessors map[int]struct{},
	usedPredecessors map[int]struct{},
	allowIndirect bool,
) error {
	binding := node.bindings[inputName]
	if binding.Source != BindingNodeOutput && binding.Source != BindingNodeError {
		return compileBindingNodeFailure(
			node.definition.ID,
			"merge source %q must bind node output or error data",
			inputName,
		)
	}

	sourceSchema, sourceIndex, err := p.bindingSchema(binding)
	if err != nil {
		return compileBindingNodeFailure(node.definition.ID, "merge source %q: %v", inputName, err)
	}

	_, directPredecessor := predecessors[sourceIndex]
	if !allowIndirect && !directPredecessor {
		return compileBindingNodeFailure(
			node.definition.ID,
			"merge source %q is not an incoming node",
			inputName,
		)
	}

	if directPredecessor {
		usedPredecessors[sourceIndex] = struct{}{}
	}

	if !schemaCompatible(sourceSchema, outputSchema) {
		return compileBindingNodeFailure(
			node.definition.ID,
			"merge output %q source %q has incompatible schema",
			outputName,
			inputName,
		)
	}

	return nil
}

func compileBindingNodeFailure(nodeID NodeID, format string, values ...any) error {
	return compileNodeFailure(
		CompileIssueInvalidBinding,
		nodeID,
		nil,
		format,
		values...,
	)
}

func validateDefaultOutputs(definition NodeDefinition, spec NodeSpec) error {
	if definition.Policy.Error != ErrorContinueWithDefault {
		return nil
	}

	values := definition.Policy.DefaultOutputs
	if values == nil {
		return compileBindingNodeFailure(definition.ID, "default outputs: nil values")
	}

	if len(values) != len(spec.Outputs) {
		return compileBindingNodeFailure(
			definition.ID,
			"default outputs: got %d ports, want %d",
			len(values),
			len(spec.Outputs),
		)
	}

	var collector compileErrorCollector

	for _, name := range sortedSchemaNames(spec.Outputs) {
		value, ok := values[name]
		if !ok {
			collector.add(
				compileBindingNodeFailure(
					definition.ID,
					"default outputs: missing port %q",
					name,
				),
				CompileIssue{},
			)

			continue
		}

		if err := spec.Outputs[name].Validate(value); err != nil {
			collector.add(
				compileBindingNodeFailure(
					definition.ID,
					"default outputs: port %q: %v",
					name,
					err,
				),
				CompileIssue{},
			)
		}
	}

	return collector.err()
}

func sortedBindingNames(bindings map[string]Binding) []string {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func sortedSchemaNames(schemas map[string]PortSchema) []string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func schemaCompatible(source, target PortSchema) bool {
	return target.Equal(source) || string(target.raw) == "true"
}

func schemaAtPath(schema PortSchema, path []string) (PortSchema, error) {
	var current any
	if err := json.Unmarshal(schema.raw, &current); err != nil {
		return PortSchema{}, fmt.Errorf("decode source schema: %w", err)
	}

	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return PortSchema{}, fmt.Errorf("schema path %q is not declared", segment)
		}

		schemaType, _ := object["type"].(string)
		switch schemaType {
		case "object":
			properties, _ := object["properties"].(map[string]any)

			var found bool

			current, found = properties[segment]
			if !found {
				return PortSchema{}, fmt.Errorf("schema property %q is not declared", segment)
			}
		case "array":
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 {
				return PortSchema{}, fmt.Errorf("schema array index %q is invalid", segment)
			}

			var found bool

			current, found = object["items"]
			if !found {
				return PortSchema{}, errors.New("array schema has no items contract")
			}
		default:
			return PortSchema{}, fmt.Errorf("schema path %q cannot traverse type %q", segment, schemaType)
		}
	}

	data, err := json.Marshal(current)
	if err != nil {
		return PortSchema{}, fmt.Errorf("encode source subschema: %w", err)
	}

	return ParsePortSchema(data)
}
