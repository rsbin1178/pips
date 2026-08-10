package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
)

type compileConfig struct {
	resolver DefinitionResolver
}

// CompileOption configures child Workflow resolution during compilation.
type CompileOption func(*compileConfig) error

// WithDefinitionResolver configures exact referenced Workflow resolution.
func WithDefinitionResolver(resolver DefinitionResolver) CompileOption {
	return func(config *compileConfig) error {
		if isNilInterface(resolver) {
			return errors.New("workflow: nil definition resolver")
		}

		config.resolver = resolver

		return nil
	}
}

type compileSession struct {
	registry *Registry
	resolver DefinitionResolver
	stack    []definitionKey
	active   map[definitionKey]struct{}
}

// Plan is an immutable, concurrent-safe compiled Workflow execution plan.
type Plan struct {
	definition            Definition
	definitionFingerprint string
	registryFingerprint   string
	fingerprint           string

	nodes      []planNode
	nodeIndex  map[NodeID]int
	edges      []planEdge
	incoming   [][]int
	outgoing   [][]int
	startIndex int
	endIndex   int
}

type planNode struct {
	definition  NodeDefinition
	executor    CompiledNode
	spec        NodeSpec
	bindings    map[string]Binding
	isMerge     bool
	isComposite bool
}

type planEdge struct {
	from  int
	to    int
	route string
}

// Compile validates definition against registry and returns an immutable Plan.
func Compile(
	ctx context.Context,
	definition Definition,
	registry *Registry,
	options ...CompileOption,
) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if registry == nil || registry.Fingerprint() == "" {
		return nil, fmt.Errorf("%w: nil or invalid registry", ErrCompile)
	}

	config := compileConfig{}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil compile option", ErrCompile)
		}

		if err := option(&config); err != nil {
			return nil, fmt.Errorf("%w: compile option: %w", ErrCompile, err)
		}
	}

	session := &compileSession{
		registry: registry,
		resolver: config.resolver,
		stack:    []definitionKey{},
		active:   map[definitionKey]struct{}{},
	}

	return session.compile(ctx, definition)
}

func (s *compileSession) compile(ctx context.Context, definition Definition) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(s.stack) > maxChildPlanDepth {
		return nil, fmt.Errorf("%w: child workflow nesting exceeds %d", ErrCompile, maxChildPlanDepth)
	}

	key := definitionKey{id: definition.ID, revision: definition.Revision}
	if _, recursive := s.active[key]; recursive {
		return nil, fmt.Errorf(
			"%w: recursive workflow reference %q@%q",
			ErrCompile,
			definition.ID,
			definition.Revision,
		)
	}

	snapshot, err := cloneDefinition(definition)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCompile, err)
	}

	key = definitionKey{id: snapshot.ID, revision: snapshot.Revision}
	if _, recursive := s.active[key]; recursive {
		return nil, fmt.Errorf(
			"%w: recursive workflow reference %q@%q",
			ErrCompile,
			snapshot.ID,
			snapshot.Revision,
		)
	}

	s.stack = append(s.stack, key)
	s.active[key] = struct{}{}

	defer func() {
		delete(s.active, key)
		s.stack = s.stack[:len(s.stack)-1]
	}()

	definitionFingerprint, err := snapshot.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint definition: %w", ErrCompile, err)
	}

	plan := &Plan{
		definition:            snapshot,
		definitionFingerprint: definitionFingerprint,
		registryFingerprint:   s.registry.Fingerprint(),
		nodes:                 make([]planNode, len(snapshot.Nodes)),
		nodeIndex:             make(map[NodeID]int, len(snapshot.Nodes)),
		incoming:              make([][]int, len(snapshot.Nodes)),
		outgoing:              make([][]int, len(snapshot.Nodes)),
		startIndex:            -1,
		endIndex:              -1,
	}

	if err := plan.compileNodes(ctx, s); err != nil {
		return nil, err
	}

	if err := plan.compileEdges(); err != nil {
		return nil, err
	}

	topological, err := plan.validateGraph()
	if err != nil {
		return nil, err
	}

	if err := plan.validateBindings(topological); err != nil {
		return nil, err
	}

	if err := plan.computeFingerprint(); err != nil {
		return nil, err
	}

	return plan, nil
}

func (s *compileSession) resolve(ctx context.Context, reference DefinitionRef) (*Plan, error) {
	if !validIdentifier(string(reference.ID)) ||
		!validIdentifier(string(reference.Revision)) ||
		!validFingerprint(reference.Fingerprint) {
		return nil, fmt.Errorf("%w: invalid workflow reference", ErrCompile)
	}

	if isNilInterface(s.resolver) {
		return nil, fmt.Errorf("%w: definition resolver is required", ErrCompile)
	}

	key := definitionKey{id: reference.ID, revision: reference.Revision}
	if _, recursive := s.active[key]; recursive {
		return nil, fmt.Errorf(
			"%w: recursive workflow reference %q@%q",
			ErrCompile,
			reference.ID,
			reference.Revision,
		)
	}

	definition, err := s.resolver.ResolveDefinition(ctx, reference.ID, reference.Revision)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: resolve workflow %q@%q: %w",
			ErrCompile,
			reference.ID,
			reference.Revision,
			err,
		)
	}

	if definition.ID != reference.ID || definition.Revision != reference.Revision {
		return nil, fmt.Errorf("%w: resolved workflow identity does not match reference", ErrCompile)
	}

	fingerprint, err := definition.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint resolved workflow: %w", ErrCompile, err)
	}

	if fingerprint != reference.Fingerprint {
		return nil, fmt.Errorf("%w: resolved workflow fingerprint does not match reference", ErrCompile)
	}

	return s.compile(ctx, definition)
}

func validFingerprint(fingerprint string) bool {
	if len(fingerprint) != sha256.Size*2 {
		return false
	}

	_, err := hex.DecodeString(fingerprint)

	return err == nil
}

// DefinitionID returns the logical Workflow identity pinned by p.
func (p *Plan) DefinitionID() DefinitionID {
	if p == nil {
		return ""
	}

	return p.definition.ID
}

// Revision returns the exact Definition revision pinned by p.
func (p *Plan) Revision() Revision {
	if p == nil {
		return ""
	}

	return p.definition.Revision
}

// DefinitionFingerprint returns the semantic Definition fingerprint.
func (p *Plan) DefinitionFingerprint() string {
	if p == nil {
		return ""
	}

	return p.definitionFingerprint
}

// RegistryFingerprint returns the Registry contract fingerprint.
func (p *Plan) RegistryFingerprint() string {
	if p == nil {
		return ""
	}

	return p.registryFingerprint
}

// Fingerprint returns the identity of the combined Definition and Registry.
func (p *Plan) Fingerprint() string {
	if p == nil {
		return ""
	}

	return p.fingerprint
}

func (p *Plan) compileNodes(ctx context.Context, session *compileSession) error {
	outputSchemas := make(map[string]PortSchema, len(p.definition.Outputs))
	for name, output := range p.definition.Outputs {
		outputSchemas[name] = output.Schema
	}

	compileContext := nodeCompileContext{
		inputs:   p.definition.Inputs,
		outputs:  outputSchemas,
		registry: session.registry,
		session:  session,
	}

	for index, definition := range p.definition.Nodes {
		if err := ctx.Err(); err != nil {
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
		NodeTypeSubWorkflow, NodeTypeBatch:
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

func (p *Plan) validateGraph() ([]int, error) {
	for index, node := range p.nodes {
		if err := p.validateGraphNode(index, node); err != nil {
			return nil, err
		}
	}

	topological, err := p.topologicalOrder()
	if err != nil {
		return nil, err
	}

	if err := p.validateReachability(); err != nil {
		return nil, err
	}

	return topological, nil
}

func (p *Plan) validateGraphNode(index int, node planNode) error {
	if err := p.validateNodeDegree(index, node); err != nil {
		return err
	}

	if node.definition.Policy.Error == ErrorRoute && !p.hasOutgoingRoute(index, RouteError) {
		return compileNodeError(node.definition.ID, "route_error requires an error edge")
	}

	for _, route := range node.spec.Routes {
		if index != p.endIndex && !p.hasOutgoingRoute(index, route) {
			return compileNodeError(node.definition.ID, "route %q has no outgoing edge", route)
		}
	}

	return nil
}

func (p *Plan) validateNodeDegree(index int, node planNode) error {
	incoming := len(p.incoming[index])
	outgoing := len(p.outgoing[index])

	if index == p.startIndex && incoming != 0 {
		return compileNodeError(node.definition.ID, "Start must not have incoming edges")
	}

	if index != p.startIndex && incoming == 0 {
		return compileNodeError(node.definition.ID, "non-Start node requires an incoming edge")
	}

	if index == p.endIndex && outgoing != 0 {
		return compileNodeError(node.definition.ID, "End must not have outgoing edges")
	}

	if index != p.endIndex && outgoing == 0 {
		return compileNodeError(node.definition.ID, "non-End node requires an outgoing edge")
	}

	if node.isMerge && incoming < 2 {
		return compileNodeError(node.definition.ID, "Merge requires at least two incoming edges")
	}

	if !node.isMerge && incoming > 1 {
		return compileNodeError(node.definition.ID, "multiple incoming edges require Merge")
	}

	return nil
}

func (p *Plan) hasOutgoingRoute(nodeIndex int, route string) bool {
	for _, edgeIndex := range p.outgoing[nodeIndex] {
		if p.edges[edgeIndex].route == route {
			return true
		}
	}

	return false
}

func (p *Plan) topologicalOrder() ([]int, error) {
	indegree := make([]int, len(p.nodes))

	ready := make([]int, 0, len(p.nodes))
	for index := range p.nodes {
		indegree[index] = len(p.incoming[index])
		if indegree[index] == 0 {
			ready = append(ready, index)
		}
	}

	order := make([]int, 0, len(p.nodes))
	for len(ready) > 0 {
		sort.Ints(ready)
		index := ready[0]
		ready = ready[1:]

		order = append(order, index)

		for _, edgeIndex := range p.outgoing[index] {
			to := p.edges[edgeIndex].to

			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
			}
		}
	}

	if len(order) != len(p.nodes) {
		return nil, fmt.Errorf("%w: root graph must be a DAG", ErrCompile)
	}

	return order, nil
}

func (p *Plan) validateReachability() error {
	fromStart := p.walk(p.startIndex, p.outgoing, func(edge planEdge) int { return edge.to })
	toEnd := p.walk(p.endIndex, p.incoming, func(edge planEdge) int { return edge.from })

	for index, node := range p.nodes {
		if !fromStart[index] || !toEnd[index] {
			return compileNodeError(node.definition.ID, "node is not on a Start-to-End path")
		}
	}

	return nil
}

func (p *Plan) walk(start int, adjacency [][]int, next func(planEdge) int) []bool {
	visited := make([]bool, len(p.nodes))
	queue := []int{start}
	visited[start] = true

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, edgeIndex := range adjacency[current] {
			candidate := next(p.edges[edgeIndex])
			if !visited[candidate] {
				visited[candidate] = true
				queue = append(queue, candidate)
			}
		}
	}

	return visited
}

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

	if binding.Source != BindingNodeOutput {
		return nil
	}

	if node.isMerge {
		if !p.isDirectPredecessor(sourceIndex, targetIndex) {
			return compileNodeError(node.definition.ID, "merge input %q is not from an incoming node", name)
		}

		return nil
	}

	if !dominators[targetIndex][sourceIndex] ||
		p.reachableFromRoute(sourceIndex, RouteError, targetIndex) {
		return compileNodeError(node.definition.ID, "input %q is not guaranteed to be available", name)
	}

	return nil
}

func (p *Plan) bindingSchema(binding Binding) (PortSchema, int, error) {
	var schema PortSchema

	sourceIndex := -1

	switch binding.Source {
	case BindingLiteral:
		return PortSchema{}, sourceIndex, nil
	case BindingWorkflowInput:
		var ok bool

		schema, ok = p.definition.Inputs[binding.Port]
		if !ok {
			return PortSchema{}, sourceIndex, fmt.Errorf("unknown Workflow input %q", binding.Port)
		}
	case BindingNodeOutput:
		var ok bool

		sourceIndex, ok = p.nodeIndex[binding.Node]
		if !ok {
			return PortSchema{}, sourceIndex, fmt.Errorf("unknown source node %q", binding.Node)
		}

		schema, ok = p.nodes[sourceIndex].spec.Outputs[binding.Port]
		if !ok {
			return PortSchema{}, sourceIndex, fmt.Errorf("unknown node output %q.%s", binding.Node, binding.Port)
		}
	default:
		return PortSchema{}, sourceIndex, fmt.Errorf("unknown binding source %q", binding.Source)
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

	for outputName, output := range merge.config.Outputs {
		for _, inputName := range output.Sources {
			binding := node.bindings[inputName]
			if binding.Source != BindingNodeOutput {
				return compileNodeError(node.definition.ID, "merge source %q must bind a node output", inputName)
			}

			sourceIndex := p.nodeIndex[binding.Node]
			if _, ok := predecessors[sourceIndex]; !ok {
				return compileNodeError(node.definition.ID, "merge source %q is not an incoming node", inputName)
			}

			usedPredecessors[sourceIndex] = struct{}{}

			sourceSchema, _, err := p.bindingSchema(binding)
			if err != nil {
				return compileNodeError(node.definition.ID, "merge source %q: %v", inputName, err)
			}

			if !schemaCompatible(sourceSchema, output.Schema) {
				return compileNodeError(
					node.definition.ID,
					"merge output %q source %q has incompatible schema",
					outputName,
					inputName,
				)
			}
		}
	}

	if len(usedPredecessors) != len(predecessors) {
		return compileNodeError(node.definition.ID, "every incoming node must provide a mapped source")
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

func (p *Plan) computeFingerprint() error {
	children := make([]childPlanIdentity, 0)

	for _, node := range p.nodes {
		composite, ok := node.executor.(compiledCompositeNode)
		if !ok {
			continue
		}

		for _, child := range composite.childPlans() {
			children = append(children, childPlanIdentity{
				Node:        node.definition.ID,
				Fingerprint: child.Fingerprint(),
			})
		}
	}

	data, err := json.Marshal(struct {
		Definition string              `json:"definition"`
		Registry   string              `json:"registry"`
		Children   []childPlanIdentity `json:"children,omitempty"`
	}{
		Definition: p.definitionFingerprint,
		Registry:   p.registryFingerprint,
		Children:   children,
	})
	if err != nil {
		return fmt.Errorf("%w: encode plan fingerprint: %w", ErrCompile, err)
	}

	digest := sha256.Sum256(data)
	p.fingerprint = hex.EncodeToString(digest[:])

	return nil
}
