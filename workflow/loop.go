package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
)

const (
	defaultLoopIterations = 100
	maxLoopIterations     = 1_000
	loopCountInput        = "count"
	loopIndexInput        = "index"
)

// LoopMode controls how a Loop determines its iteration count.
type LoopMode string

// Loop iteration modes.
const (
	LoopArray    LoopMode = "array"
	LoopCount    LoopMode = "count"
	LoopInfinite LoopMode = "infinite"
)

// LoopVariable declares one Loop-local mutable value and its schema.
type LoopVariable struct {
	Name   string     `json:"name"`
	Schema PortSchema `json:"schema"`
}

// LoopOutputSource identifies the value projected by a Loop output.
type LoopOutputSource string

// Loop output sources.
const (
	LoopOutputBody     LoopOutputSource = "body_output"
	LoopOutputVariable LoopOutputSource = "loop_variable"
)

// LoopOutput projects a body output or final Loop variable onto the Loop node.
type LoopOutput struct {
	Name   string           `json:"name"`
	Source LoopOutputSource `json:"source"`
	Port   string           `json:"port"`
}

// LoopConfig defines an inline sequential Loop body.
type LoopConfig struct {
	Body          Definition     `json:"body"`
	Mode          LoopMode       `json:"mode"`
	Arrays        []string       `json:"arrays,omitempty"`
	Variables     []LoopVariable `json:"variables,omitempty"`
	Outputs       []LoopOutput   `json:"outputs,omitempty"`
	MaxIterations int            `json:"max_iterations,omitempty"`
}

// LoopNode repeatedly executes an inline Workflow with transactional local
// variables.
type LoopNode struct{}

// Spec implements [NodeType].
func (LoopNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeLoop, Version: BuiltinNodeVersion, DisplayName: "Loop"}
}

// Compile implements [NodeType].
func (LoopNode) Compile(
	ctx context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	if loopContext, ok := compileContext.(loopNodeCompileContext); ok &&
		loopContext.loopCompileScope() != nil {
		return nil, compileNodeError(definition.ID, "Loop is not allowed inside a Loop body")
	}

	var config LoopConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if config.MaxIterations == 0 {
		config.MaxIterations = defaultLoopIterations
	}

	scope, err := validateLoopConfig(config)
	if err != nil {
		return nil, compileNodeError(definition.ID, "loop config: %v", err)
	}

	compositeContext, ok := compileContext.(compositeCompileContext)
	if !ok {
		return nil, compileNodeError(definition.ID, "composite compiler is unavailable")
	}

	child, err := compositeContext.compileLoopDefinition(ctx, config.Body, scope)
	if err != nil {
		return nil, wrapCompileNodeError(definition.ID, "loop body", err)
	}

	if child.containsNodeType(NodeTypeLoop) || child.containsNodeType(NodeTypeBatch) {
		return nil, compileNodeError(definition.ID, "loop body must not contain Loop or Batch")
	}

	if config.Mode == LoopInfinite && !child.hasDirectNodeType(NodeTypeBreak) {
		return nil, compileNodeError(definition.ID, "infinite loop body requires Break")
	}

	spec, lifted, err := loopNodeSpec(config, child, scope, definition.Inputs)
	if err != nil {
		return nil, compileNodeError(definition.ID, "loop contract: %v", err)
	}

	return &compiledLoop{
		spec: spec, config: config, child: child, lifted: lifted,
		variables: cloneSchemas(scope.variables),
	}, nil
}

type compiledLoop struct {
	spec      NodeSpec
	config    LoopConfig
	child     *Plan
	lifted    []string
	variables map[string]PortSchema
}

func (n *compiledLoop) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledLoop) Invoke(ctx context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil {
		return NodeOutput{}, errors.New("loop runtime is unavailable")
	}

	iterations, arrays, err := n.iterationInputs(input.Values)
	if err != nil {
		return NodeOutput{}, err
	}

	committed, aggregated, startIndex, resumed := n.initialLoopState(input)

	for index := startIndex; index < iterations; index++ {
		state, childCheckpoint := resumeLoopIteration(committed, resumed, index)
		resumed = nil

		bodyInputs := n.bodyInputs(input.Values, arrays, index)

		outputs, err := input.runtime.runLoopChild(
			ctx,
			n.child,
			bodyInputs,
			ScopeFrame{Kind: ScopeLoopIteration, NodeID: input.runtime.nodeID, Index: index},
			state,
			childCheckpoint,
		)
		if err != nil {
			return NodeOutput{}, loopIterationError(
				err,
				state,
				iterations,
				index,
				committed,
				aggregated,
			)
		}

		var shouldBreak bool

		committed, shouldBreak = state.snapshot()

		n.appendLoopOutputs(aggregated, outputs)

		if shouldBreak {
			return n.output(committed, aggregated)
		}
	}

	if n.config.Mode == LoopInfinite {
		return NodeOutput{}, errLoopLimit
	}

	return n.output(committed, aggregated)
}

func (n *compiledLoop) initialLoopState(
	input NodeInput,
) (map[string]Value, map[string][]Value, int, *loopCheckpoint) {
	committed := make(map[string]Value, len(n.variables))
	for name := range n.variables {
		committed[name] = input.Values[name]
	}

	aggregated := make(map[string][]Value)

	for _, output := range n.config.Outputs {
		if output.Source == LoopOutputBody {
			aggregated[output.Name] = []Value{}
		}
	}

	if input.runtime.resume == nil || input.runtime.resume.Loop == nil {
		return committed, aggregated, 0, nil
	}

	resumed := cloneLoopCheckpoint(input.runtime.resume.Loop)

	return cloneValues(resumed.Committed), cloneValueSlices(resumed.Aggregated),
		resumed.Index, resumed
}

func resumeLoopIteration(
	committed map[string]Value,
	resumed *loopCheckpoint,
	index int,
) (*loopIterationState, *executionCheckpoint) {
	if resumed == nil || resumed.Index != index {
		return newLoopIterationState(committed), nil
	}

	return newLoopIterationStateSnapshot(
		resumed.IterationState,
		resumed.ShouldBreak,
	), resumed.Child
}

func loopIterationError(
	err error,
	state *loopIterationState,
	iterations int,
	index int,
	committed map[string]Value,
	aggregated map[string][]Value,
) error {
	var pause *executionPauseError
	if !errors.As(err, &pause) {
		return err
	}

	iterationState, shouldBreak := state.snapshot()
	child := pause.checkpoint
	checkpoint := &loopCheckpoint{
		Iterations: iterations, Index: index,
		Committed: cloneValues(committed), Aggregated: cloneValueSlices(aggregated),
		IterationState: iterationState, ShouldBreak: shouldBreak,
		Child: &child, Dynamic: cloneDynamicInterrupts(pause.dynamic),
		Info: cloneInterruptInfo(pause.info),
	}

	return &executionPauseError{
		loop: checkpoint, info: cloneInterruptInfo(pause.info),
		dynamic: cloneDynamicInterrupts(pause.dynamic),
	}
}

func (n *compiledLoop) appendLoopOutputs(
	aggregated map[string][]Value,
	outputs map[string]Value,
) {
	for _, output := range n.config.Outputs {
		if output.Source == LoopOutputBody {
			aggregated[output.Name] = append(aggregated[output.Name], outputs[output.Port])
		}
	}
}

func (n *compiledLoop) childPlans() []*Plan {
	return []*Plan{n.child}
}

func (n *compiledLoop) iterationInputs(
	inputs map[string]Value,
) (int, map[string][]Value, error) {
	switch n.config.Mode {
	case LoopArray:
		arrays := make(map[string][]Value, len(n.config.Arrays))
		iterations := -1

		for _, name := range n.config.Arrays {
			items, err := DecodeValue[[]Value](inputs[name])
			if err != nil {
				return 0, nil, fmt.Errorf("decode loop array %q: %w", name, err)
			}

			arrays[name] = items
			if iterations < 0 || len(items) < iterations {
				iterations = len(items)
			}
		}

		if iterations > n.config.MaxIterations {
			return 0, nil, fmt.Errorf(
				"%w: got %d, max %d",
				errLoopLimit,
				iterations,
				n.config.MaxIterations,
			)
		}

		return iterations, arrays, nil
	case LoopCount:
		iterations, err := DecodeValue[int](inputs[loopCountInput])
		if err != nil {
			return 0, nil, fmt.Errorf("decode loop count: %w", err)
		}

		if iterations > n.config.MaxIterations {
			return 0, nil, fmt.Errorf(
				"%w: got %d, max %d",
				errLoopLimit,
				iterations,
				n.config.MaxIterations,
			)
		}

		return iterations, nil, nil
	case LoopInfinite:
		return n.config.MaxIterations, nil, nil
	default:
		return 0, nil, fmt.Errorf("unknown loop mode %q", n.config.Mode)
	}
}

func (n *compiledLoop) bodyInputs(
	inputs map[string]Value,
	arrays map[string][]Value,
	index int,
) map[string]Value {
	values := make(map[string]Value, len(n.child.definition.Inputs))
	values[loopIndexInput] = MustValueOf(index)

	for _, name := range n.config.Arrays {
		values[name] = arrays[name][index]
	}

	for _, name := range n.lifted {
		values[name] = inputs[name]
	}

	return values
}

func (n *compiledLoop) output(
	variables map[string]Value,
	aggregated map[string][]Value,
) (NodeOutput, error) {
	values := make(map[string]Value, len(n.config.Outputs))
	for _, output := range n.config.Outputs {
		switch output.Source {
		case LoopOutputBody:
			value, err := ValueOf(aggregated[output.Name])
			if err != nil {
				return NodeOutput{}, fmt.Errorf("encode loop output %q: %w", output.Name, err)
			}

			values[output.Name] = value
		case LoopOutputVariable:
			values[output.Name] = variables[output.Port]
		}
	}

	return NodeOutput{Values: values, Route: RouteSuccess}, nil
}

func validateLoopConfig(config LoopConfig) (loopCompileScope, error) {
	if config.MaxIterations < 1 || config.MaxIterations > maxLoopIterations {
		return loopCompileScope{}, fmt.Errorf(
			"max iterations must be between 1 and %d",
			maxLoopIterations,
		)
	}

	variables, err := validateLoopVariables(config.Variables)
	if err != nil {
		return loopCompileScope{}, err
	}

	if err := validateLoopArrays(config.Arrays, variables); err != nil {
		return loopCompileScope{}, err
	}

	if err := validateLoopMode(config.Mode, config.Arrays); err != nil {
		return loopCompileScope{}, err
	}

	if err := validateLoopOutputs(config.Outputs); err != nil {
		return loopCompileScope{}, err
	}

	return loopCompileScope{variables: variables}, nil
}

func validateLoopVariables(variables []LoopVariable) (map[string]PortSchema, error) {
	schemas := make(map[string]PortSchema, len(variables))

	for _, variable := range variables {
		if !validIdentifier(variable.Name) || !variable.Schema.IsValid() {
			return nil, fmt.Errorf("invalid variable %q", variable.Name)
		}

		if variable.Name == loopCountInput || variable.Name == loopIndexInput {
			return nil, fmt.Errorf("variable %q uses a reserved name", variable.Name)
		}

		if _, duplicate := schemas[variable.Name]; duplicate {
			return nil, fmt.Errorf("duplicate variable %q", variable.Name)
		}

		schemas[variable.Name] = variable.Schema
	}

	return schemas, nil
}

func validateLoopArrays(arrays []string, variables map[string]PortSchema) error {
	arrayNames := make(map[string]struct{}, len(arrays))

	for _, name := range arrays {
		if !validIdentifier(name) || name == loopCountInput || name == loopIndexInput {
			return fmt.Errorf("invalid array name %q", name)
		}

		if _, duplicate := arrayNames[name]; duplicate {
			return fmt.Errorf("duplicate array %q", name)
		}

		if _, collision := variables[name]; collision {
			return fmt.Errorf("array %q conflicts with a variable", name)
		}

		arrayNames[name] = struct{}{}
	}

	return nil
}

func validateLoopMode(mode LoopMode, arrays []string) error {
	switch mode {
	case LoopArray:
		if len(arrays) == 0 {
			return errors.New("array mode requires at least one array")
		}
	case LoopCount, LoopInfinite:
		if len(arrays) != 0 {
			return fmt.Errorf("mode %q does not accept arrays", mode)
		}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}

	return nil
}

func validateLoopOutputs(outputs []LoopOutput) error {
	outputNames := make(map[string]struct{}, len(outputs))

	for _, output := range outputs {
		if !validIdentifier(output.Name) || !validIdentifier(output.Port) {
			return fmt.Errorf("invalid output %q", output.Name)
		}

		if _, duplicate := outputNames[output.Name]; duplicate {
			return fmt.Errorf("duplicate output %q", output.Name)
		}

		switch output.Source {
		case LoopOutputBody, LoopOutputVariable:
		default:
			return fmt.Errorf(
				"output %q has unknown source %q",
				output.Name,
				output.Source,
			)
		}

		outputNames[output.Name] = struct{}{}
	}

	return nil
}

func loopNodeSpec(
	config LoopConfig,
	child *Plan,
	scope loopCompileScope,
	bindings map[string]Binding,
) (NodeSpec, []string, error) {
	if err := validateLoopBodyInputs(child); err != nil {
		return NodeSpec{}, nil, err
	}

	inputs, lifted, err := loopInputContract(config, child, scope, bindings)
	if err != nil {
		return NodeSpec{}, nil, err
	}

	outputs, err := loopOutputContract(config.Outputs, child, scope)
	if err != nil {
		return NodeSpec{}, nil, err
	}

	return NodeSpec{Inputs: inputs, Outputs: outputs, Routes: []string{RouteSuccess}}, lifted, nil
}

func validateLoopBodyInputs(child *Plan) error {
	indexInput, ok := child.definition.Inputs[loopIndexInput]
	if !ok || !indexInput.Required {
		return errors.New("body input index is required")
	}

	integerSchema, err := ParsePortSchema([]byte(`{"type":"integer"}`))
	if err != nil {
		return fmt.Errorf("prepare index schema: %w", err)
	}

	if !schemaCompatible(integerSchema, indexInput.Schema) {
		return errors.New("body input index must accept an integer")
	}

	if _, present := child.definition.Inputs[loopCountInput]; present {
		return errors.New("body input count is reserved")
	}

	return nil
}

func loopInputContract(
	config LoopConfig,
	child *Plan,
	scope loopCompileScope,
	bindings map[string]Binding,
) (map[string]PortSchema, []string, error) {
	inputs := make(map[string]PortSchema)
	maps.Copy(inputs, scope.variables)

	arrayNames := make(map[string]struct{}, len(config.Arrays))

	for _, name := range config.Arrays {
		itemInput, present := child.definition.Inputs[name]
		if !present {
			return nil, nil, fmt.Errorf("body input %q is required for array mode", name)
		}

		arraySchema, err := arrayPortSchema(itemInput.Schema)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare array input %q: %w", name, err)
		}

		inputs[name] = arraySchema
		arrayNames[name] = struct{}{}
	}

	lifted := make([]string, 0, len(child.definition.Inputs))

	for name, input := range child.definition.Inputs {
		if name == loopIndexInput {
			continue
		}

		if _, array := arrayNames[name]; array {
			continue
		}

		if _, collision := scope.variables[name]; collision {
			return nil, nil, fmt.Errorf("body input %q conflicts with a variable", name)
		}

		if input.Required {
			inputs[name] = input.Schema
			lifted = append(lifted, name)

			continue
		}

		if _, bound := bindings[name]; bound {
			inputs[name] = input.Schema
			lifted = append(lifted, name)
		}
	}

	if config.Mode == LoopCount {
		countSchema, err := ParsePortSchema([]byte(`{"type":"integer","minimum":1}`))
		if err != nil {
			return nil, nil, fmt.Errorf("prepare count schema: %w", err)
		}

		inputs[loopCountInput] = countSchema
	}

	return inputs, lifted, nil
}

func loopOutputContract(
	projections []LoopOutput,
	child *Plan,
	scope loopCompileScope,
) (map[string]PortSchema, error) {
	outputs := make(map[string]PortSchema, len(projections))

	for _, output := range projections {
		switch output.Source {
		case LoopOutputBody:
			bodyOutput, present := child.definition.Outputs[output.Port]
			if !present {
				return nil, fmt.Errorf("unknown body output %q", output.Port)
			}

			arraySchema, err := arrayPortSchema(bodyOutput.Schema)
			if err != nil {
				return nil, fmt.Errorf("prepare output %q: %w", output.Name, err)
			}

			outputs[output.Name] = arraySchema
		case LoopOutputVariable:
			variableSchema, present := scope.variables[output.Port]
			if !present {
				return nil, fmt.Errorf("unknown loop variable %q", output.Port)
			}

			outputs[output.Name] = variableSchema
		}
	}

	return outputs, nil
}

type loopCompileScope struct {
	variables map[string]PortSchema
}

func cloneLoopCompileScope(scope *loopCompileScope) *loopCompileScope {
	if scope == nil {
		return nil
	}

	return &loopCompileScope{variables: cloneSchemas(scope.variables)}
}

type loopNodeCompileContext interface {
	loopCompileScope() *loopCompileScope
}

func (p *Plan) hasDirectNodeType(nodeType NodeTypeKey) bool {
	for _, node := range p.nodes {
		if node.definition.Type == nodeType {
			return true
		}
	}

	return false
}

var errLoopLimit = errors.New("workflow loop iteration limit exceeded")

var (
	_ CompiledNode          = (*compiledLoop)(nil)
	_ compiledCompositeNode = (*compiledLoop)(nil)
)
