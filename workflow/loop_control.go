package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
)

// SetVariableAssignment maps one node input to one Loop variable.
type SetVariableAssignment struct {
	Target string `json:"target"`
	Input  string `json:"input"`
}

// SetVariableConfig declares an atomic group of Loop-variable assignments.
type SetVariableConfig struct {
	Assignments []SetVariableAssignment `json:"assignments"`
}

// BreakNode terminates the owning Loop after the current iteration commits.
type BreakNode struct{}

// Spec implements [NodeType].
func (BreakNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeBreak, Version: BuiltinNodeVersion, DisplayName: "Break"}
}

// Compile implements [NodeType].
func (BreakNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	if err := rejectNodeConfig(definition); err != nil {
		return nil, err
	}

	if _, err := requireLoopCompileScope(definition.ID, compileContext); err != nil {
		return nil, err
	}

	return &compiledBreak{}, nil
}

type compiledBreak struct{}

func (*compiledBreak) Spec() NodeSpec {
	return loopControlNodeSpec()
}

func (*compiledBreak) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil {
		return NodeOutput{}, errors.New("break runtime is unavailable")
	}

	if err := input.runtime.breakLoop(); err != nil {
		return NodeOutput{}, err
	}

	return NodeOutput{Values: map[string]Value{}, Route: RouteSuccess}, nil
}

// ContinueNode terminates the current Loop iteration without exiting the Loop.
type ContinueNode struct{}

// Spec implements [NodeType].
func (ContinueNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key: NodeTypeContinue, Version: BuiltinNodeVersion, DisplayName: "Continue",
	}
}

// Compile implements [NodeType].
func (ContinueNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	if err := rejectNodeConfig(definition); err != nil {
		return nil, err
	}

	if _, err := requireLoopCompileScope(definition.ID, compileContext); err != nil {
		return nil, err
	}

	return &compiledContinue{}, nil
}

type compiledContinue struct{}

func (*compiledContinue) Spec() NodeSpec {
	return loopControlNodeSpec()
}

func (*compiledContinue) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil || input.runtime.loopState() == nil {
		return NodeOutput{}, errors.New("continue runtime is unavailable")
	}

	return NodeOutput{Values: map[string]Value{}, Route: RouteSuccess}, nil
}

// SetVariableNode atomically assigns values to Loop-local variables.
type SetVariableNode struct{}

// Spec implements [NodeType].
func (SetVariableNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key: NodeTypeSetVariable, Version: BuiltinNodeVersion, DisplayName: "Set Variable",
	}
}

// Compile implements [NodeType].
func (SetVariableNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	scope, err := requireLoopCompileScope(definition.ID, compileContext)
	if err != nil {
		return nil, err
	}

	var config SetVariableConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	inputs, targets, err := setVariableContract(config, scope)
	if err != nil {
		return nil, compileNodeError(definition.ID, "set variable config: %v", err)
	}

	return &compiledSetVariable{
		spec:        NodeSpec{Inputs: inputs, Outputs: map[string]PortSchema{}, Routes: []string{RouteSuccess}},
		assignments: config.Assignments,
		targets:     targets,
	}, nil
}

type compiledSetVariable struct {
	spec        NodeSpec
	assignments []SetVariableAssignment
	targets     map[string]struct{}
}

func (n *compiledSetVariable) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledSetVariable) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil {
		return NodeOutput{}, errors.New("set variable runtime is unavailable")
	}

	updates := make(map[string]Value, len(n.assignments))
	for _, assignment := range n.assignments {
		updates[assignment.Target] = input.Values[assignment.Input]
	}

	if err := input.runtime.setLoopVariables(updates); err != nil {
		return NodeOutput{}, err
	}

	return NodeOutput{Values: map[string]Value{}, Route: RouteSuccess}, nil
}

func requireLoopCompileScope(
	nodeID NodeID,
	compileContext CompileContext,
) (*loopCompileScope, error) {
	loopContext, ok := compileContext.(loopNodeCompileContext)
	if !ok || loopContext.loopCompileScope() == nil {
		return nil, compileNodeError(nodeID, "node is only available in a direct Loop body")
	}

	return loopContext.loopCompileScope(), nil
}

func loopControlNodeSpec() NodeSpec {
	return NodeSpec{
		Inputs: map[string]PortSchema{}, Outputs: map[string]PortSchema{},
		Routes: []string{RouteSuccess},
	}
}

func setVariableContract(
	config SetVariableConfig,
	scope *loopCompileScope,
) (map[string]PortSchema, map[string]struct{}, error) {
	if len(config.Assignments) == 0 {
		return nil, nil, errors.New("at least one assignment is required")
	}

	inputs := make(map[string]PortSchema, len(config.Assignments))
	targets := make(map[string]struct{}, len(config.Assignments))

	for _, assignment := range config.Assignments {
		if !validIdentifier(assignment.Target) || !validIdentifier(assignment.Input) {
			return nil, nil, errors.New("assignment target and input must be valid names")
		}

		schema, present := scope.variables[assignment.Target]
		if !present {
			return nil, nil, fmt.Errorf("unknown loop variable %q", assignment.Target)
		}

		if _, duplicate := targets[assignment.Target]; duplicate {
			return nil, nil, fmt.Errorf("duplicate target %q", assignment.Target)
		}

		if _, duplicate := inputs[assignment.Input]; duplicate {
			return nil, nil, fmt.Errorf("duplicate input %q", assignment.Input)
		}

		targets[assignment.Target] = struct{}{}
		inputs[assignment.Input] = schema
	}

	return inputs, targets, nil
}

type loopIterationState struct {
	mu          sync.RWMutex
	variables   map[string]Value
	shouldBreak bool
}

func newLoopIterationState(variables map[string]Value) *loopIterationState {
	return &loopIterationState{variables: cloneValues(variables)}
}

func (s *loopIterationState) value(name string) (Value, bool) {
	if s == nil {
		return Value{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	value, ok := s.variables[name]

	return value, ok
}

func (s *loopIterationState) assign(updates map[string]Value) error {
	if s == nil {
		return errors.New("loop iteration state is unavailable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for name, value := range updates {
		if _, present := s.variables[name]; !present || !value.IsValid() {
			return fmt.Errorf("invalid loop variable assignment %q", name)
		}
	}

	maps.Copy(s.variables, updates)

	return nil
}

func (s *loopIterationState) requestBreak() error {
	if s == nil {
		return errors.New("loop iteration state is unavailable")
	}

	s.mu.Lock()
	s.shouldBreak = true
	s.mu.Unlock()

	return nil
}

func (s *loopIterationState) snapshot() (map[string]Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return cloneValues(s.variables), s.shouldBreak
}

var (
	_ CompiledNode = (*compiledBreak)(nil)
	_ CompiledNode = (*compiledContinue)(nil)
	_ CompiledNode = (*compiledSetVariable)(nil)
)
