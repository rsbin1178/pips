package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

func (p *Plan) validateBindings(topological []int) error {
	dominators := p.dominators(topological)
	for targetIndex, node := range p.nodes {
		if err := p.validateNodeBindings(targetIndex, node, dominators); err != nil {
			return err
		}

		if node.isMerge {
			if err := p.validateMergeNode(targetIndex); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *Plan) validateNodeBindings(targetIndex int, node planNode, dominators [][]bool) error {
	for name, binding := range node.bindings {
		if err := p.validateBindingContract(targetIndex, node, name, binding, dominators); err != nil {
			return err
		}
	}

	return nil
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
		return compileNodeError(node.definition.ID, "input %q: %v", name, err)
	}

	if binding.Source == BindingLiteral {
		if err := targetSchema.Validate(*binding.Value); err != nil {
			return compileNodeError(node.definition.ID, "input %q: %v", name, err)
		}

		return nil
	}

	if !schemaCompatible(sourceSchema, targetSchema) {
		return compileNodeError(node.definition.ID, "input %q has incompatible schema", name)
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
				return compileNodeError(
					node.definition.ID,
					"merge input %q is not from a route-relevant upstream node",
					name,
				)
			}

			return nil
		}

		if !p.isDirectPredecessor(sourceIndex, targetIndex) {
			return compileNodeError(
				node.definition.ID,
				"merge input %q is not from an incoming node",
				name,
			)
		}

		return nil
	}

	if !dominators[targetIndex][sourceIndex] ||
		p.reachableFromRoute(sourceIndex, RouteError, targetIndex) {
		return compileNodeError(node.definition.ID, "input %q is not guaranteed to be available", name)
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
		return compileNodeError(
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
		return compileNodeError(
			node.definition.ID,
			"built-in Merge executor has unexpected type",
		)
	}

	if merge.version == MergeNodeVersionV2 && merge.config.Mode == MergeExclusive {
		if !p.reachableFromRoute(sourceIndex, RouteError, targetIndex) {
			return compileNodeError(
				node.definition.ID,
				"merge input %q is not from a route-relevant upstream error route",
				name,
			)
		}

		return nil
	}

	if !p.isDirectPredecessorRoute(sourceIndex, targetIndex, RouteError) {
		return compileNodeError(
			node.definition.ID,
			"merge input %q is not from an incoming error route",
			name,
		)
	}

	if merge.config.Mode == MergeExclusive {
		return nil
	}

	if !p.nodeErrorGuaranteed(sourceIndex, targetIndex, dominators) {
		return compileNodeError(
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
				return compileNodeError(
					node.definition.ID,
					"success edge must target the Loop body End",
				)
			}
		}

		if successEdges != 1 {
			return compileNodeError(
				node.definition.ID,
				"requires exactly one success edge to the Loop body End",
			)
		}
	}

	return nil
}

type loopVariableAccess struct {
	node  int
	write bool
}

func (p *Plan) validateLoopVariableAccess(topological []int) error {
	if p.loop == nil {
		return nil
	}

	accesses := p.loopVariableAccesses()
	dominators := p.dominators(topological)

	for variable, variableAccesses := range accesses {
		if err := p.validateVariableAccesses(variable, variableAccesses, dominators); err != nil {
			return err
		}
	}

	return nil
}

func (p *Plan) loopVariableAccesses() map[string][]loopVariableAccess {
	accesses := make(map[string][]loopVariableAccess, len(p.loop.variables))

	for index, node := range p.nodes {
		for _, binding := range node.bindings {
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

		for variable := range setter.targets {
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
	for left := range accesses {
		for right := left + 1; right < len(accesses); right++ {
			first := accesses[left]
			second := accesses[right]

			if p.loopAccessesOrderedOrIndependent(first, second, dominators) {
				continue
			}

			return fmt.Errorf(
				"%w: loop variable %q has unordered access between nodes %q and %q",
				ErrCompile,
				variable,
				p.nodes[first.node].definition.ID,
				p.nodes[second.node].definition.ID,
			)
		}
	}

	return nil
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
		return compileNodeError(node.definition.ID, "built-in Merge executor has unexpected type")
	}

	predecessors := make(map[int]struct{}, len(p.incoming[nodeIndex]))
	for _, edgeIndex := range p.incoming[nodeIndex] {
		predecessors[p.edges[edgeIndex].from] = struct{}{}
	}

	usedPredecessors := make(map[int]struct{}, len(predecessors))
	allowIndirect := p.isV2ExclusiveMerge(node)

	for outputName, output := range merge.config.Outputs {
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
				return err
			}
		}
	}

	if !allowIndirect && len(usedPredecessors) != len(predecessors) {
		return compileNodeError(node.definition.ID, "every incoming node must provide a mapped source")
	}

	return nil
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
		return compileNodeError(
			node.definition.ID,
			"merge source %q must bind node output or error data",
			inputName,
		)
	}

	sourceSchema, sourceIndex, err := p.bindingSchema(binding)
	if err != nil {
		return compileNodeError(node.definition.ID, "merge source %q: %v", inputName, err)
	}

	_, directPredecessor := predecessors[sourceIndex]
	if !allowIndirect && !directPredecessor {
		return compileNodeError(node.definition.ID, "merge source %q is not an incoming node", inputName)
	}

	if directPredecessor {
		usedPredecessors[sourceIndex] = struct{}{}
	}

	if !schemaCompatible(sourceSchema, outputSchema) {
		return compileNodeError(
			node.definition.ID,
			"merge output %q source %q has incompatible schema",
			outputName,
			inputName,
		)
	}

	return nil
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
