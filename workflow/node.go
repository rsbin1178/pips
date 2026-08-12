package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
)

const (
	// BuiltinNodeVersion is the exact version of all first-version built-ins.
	BuiltinNodeVersion = "v1"
	// MergeNodeVersionV2 selects the first non-null candidate in source order.
	MergeNodeVersionV2 = "v2"

	// NodeTypeStart identifies [StartNode].
	NodeTypeStart NodeTypeKey = "start"
	// NodeTypeEnd identifies [EndNode].
	NodeTypeEnd NodeTypeKey = "end"
	// NodeTypeAction identifies [ActionNode].
	NodeTypeAction NodeTypeKey = "action"
	// NodeTypeCondition identifies [ConditionNode].
	NodeTypeCondition NodeTypeKey = "condition"
	// NodeTypeMerge identifies [MergeNode].
	NodeTypeMerge NodeTypeKey = "merge"
	// NodeTypeSelector identifies [SelectorNode].
	NodeTypeSelector NodeTypeKey = "selector"
	// NodeTypeSubWorkflow identifies [SubWorkflowNode].
	NodeTypeSubWorkflow NodeTypeKey = "sub_workflow"
	// NodeTypeBatch identifies [BatchNode].
	NodeTypeBatch NodeTypeKey = "batch"
	// NodeTypeLoop identifies [LoopNode].
	NodeTypeLoop NodeTypeKey = "loop"
	// NodeTypeBreak identifies [BreakNode].
	NodeTypeBreak NodeTypeKey = "break"
	// NodeTypeContinue identifies [ContinueNode].
	NodeTypeContinue NodeTypeKey = "continue"
	// NodeTypeSetVariable identifies [SetVariableNode].
	NodeTypeSetVariable NodeTypeKey = "set_variable"

	// RouteSuccess is the normal completion route.
	RouteSuccess = "success"
	// RouteError is selected by ErrorRoute after invocation failure.
	RouteError = "error"
)

// BuiltinNodeTypes returns independent registrations for the built-in
// Workflow nodes.
func BuiltinNodeTypes() []NodeType {
	return []NodeType{
		StartNode{},
		EndNode{},
		ActionNode{},
		ConditionNode{},
		MergeNode{},
		mergeNodeV2{},
		SelectorNode{},
		SubWorkflowNode{},
		BatchNode{},
		LoopNode{},
		BreakNode{},
		ContinueNode{},
		SetVariableNode{},
	}
}

// NewDefaultRegistry creates a Registry containing the built-in node types and
// the supplied Actions.
func NewDefaultRegistry(actions ...Action) (*Registry, error) {
	return NewRegistry(BuiltinNodeTypes(), actions)
}

// StartNode declares Workflow inputs and starts control flow.
type StartNode struct{}

// Spec implements [NodeType].
func (StartNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeStart, Version: BuiltinNodeVersion, DisplayName: "Start"}
}

// Compile implements [NodeType].
func (StartNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	if err := rejectNodeConfig(definition); err != nil {
		return nil, err
	}

	schemas := compileContext.WorkflowInputs()

	return &passthroughNode{spec: NodeSpec{
		Inputs: schemas, Outputs: cloneSchemas(schemas), Routes: []string{RouteSuccess},
	}}, nil
}

// EndNode validates and returns Workflow outputs.
type EndNode struct{}

// Spec implements [NodeType].
func (EndNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeEnd, Version: BuiltinNodeVersion, DisplayName: "End"}
}

// Compile implements [NodeType].
func (EndNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	if err := rejectNodeConfig(definition); err != nil {
		return nil, err
	}

	schemas := compileContext.WorkflowOutputs()

	return &passthroughNode{spec: NodeSpec{
		Inputs: schemas, Outputs: cloneSchemas(schemas), Routes: []string{RouteSuccess},
	}}, nil
}

type passthroughNode struct {
	spec NodeSpec
}

func (n *passthroughNode) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *passthroughNode) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	return NodeOutput{Values: cloneValues(input.Values), Route: RouteSuccess}, nil
}

// ActionConfig selects one exact Action and carries optional immutable
// parameters that are separate from data bindings.
type ActionConfig struct {
	Action     ActionKey `json:"action"`
	Version    string    `json:"version"`
	Parameters *Value    `json:"parameters,omitempty"`
}

// ActionNode invokes one host-registered Action.
type ActionNode struct{}

// Spec implements [NodeType].
func (ActionNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeAction, Version: BuiltinNodeVersion, DisplayName: "Action"}
}

// Compile implements [NodeType].
func (ActionNode) Compile(
	_ context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	var config ActionConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if !validIdentifier(string(config.Action)) || !validIdentifier(config.Version) {
		return nil, compileNodeError(definition.ID, "invalid action reference")
	}

	if config.Parameters != nil && !config.Parameters.IsValid() {
		return nil, compileNodeError(definition.ID, "invalid action parameters")
	}

	action, ok := compileContext.Action(config.Action, config.Version)
	if !ok {
		return nil, compileNodeError(
			definition.ID,
			"unknown action %q@%q",
			config.Action,
			config.Version,
		)
	}

	spec := cloneActionSpec(action.Spec())

	compiled := &compiledAction{
		action: action,
		spec: NodeSpec{
			Inputs: spec.Inputs, Outputs: spec.Outputs, Routes: []string{RouteSuccess},
		},
	}
	if config.Parameters != nil {
		compiled.parameters = *config.Parameters
	}

	return compiled, nil
}

type compiledAction struct {
	action     Action
	spec       NodeSpec
	parameters Value
}

func (n *compiledAction) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledAction) Invoke(ctx context.Context, input NodeInput) (NodeOutput, error) {
	output, err := n.action.Run(ctx, ActionInput{
		Values: cloneValues(input.Values), Parameters: n.parameters,
	})
	if err != nil {
		return NodeOutput{}, err
	}

	if err := validatePortValues(output.Values, n.spec.Outputs); err != nil {
		return NodeOutput{}, fmt.Errorf("action output: %w", err)
	}

	return NodeOutput{Values: cloneValues(output.Values), Route: RouteSuccess}, nil
}

// PredicateOp identifies one safe Condition predicate operation.
type PredicateOp string

// Condition predicate operations.
const (
	PredicateExists         PredicateOp = "exists"
	PredicateEqual          PredicateOp = "eq"
	PredicateNotEqual       PredicateOp = "ne"
	PredicateGreater        PredicateOp = "gt"
	PredicateGreaterOrEqual PredicateOp = "gte"
	PredicateLess           PredicateOp = "lt"
	PredicateLessOrEqual    PredicateOp = "lte"
	PredicateIn             PredicateOp = "in"
	PredicateAll            PredicateOp = "all"
	PredicateAny            PredicateOp = "any"
	PredicateNot            PredicateOp = "not"
)

// Predicate is a bounded, serializable Condition expression. Leaf operations
// read Input and Path; boolean operations contain Args.
type Predicate struct {
	Op     PredicateOp `json:"op"`
	Input  string      `json:"input,omitempty"`
	Path   []string    `json:"path,omitempty"`
	Value  *Value      `json:"value,omitempty"`
	Values []Value     `json:"values,omitempty"`
	Args   []Predicate `json:"args,omitempty"`
}

// ConditionConfig selects named routes for a boolean Predicate result.
type ConditionConfig struct {
	Predicate  Predicate `json:"predicate"`
	TrueRoute  string    `json:"true_route"`
	FalseRoute string    `json:"false_route"`
}

// ConditionNode chooses one named route using a structured Predicate.
type ConditionNode struct{}

// Spec implements [NodeType].
func (ConditionNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeCondition, Version: BuiltinNodeVersion, DisplayName: "Condition"}
}

// Compile implements [NodeType].
func (ConditionNode) Compile(
	_ context.Context,
	_ CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	var config ConditionConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if !validIdentifier(config.TrueRoute) || !validIdentifier(config.FalseRoute) ||
		config.TrueRoute == config.FalseRoute {
		return nil, compileNodeError(definition.ID, "condition routes must be distinct valid names")
	}

	if err := validatePredicate(config.Predicate, definition.Inputs, 0, new(int)); err != nil {
		return nil, compileNodeError(definition.ID, "predicate: %v", err)
	}

	anySchema, err := ParsePortSchema([]byte("true"))
	if err != nil {
		return nil, compileNodeError(definition.ID, "prepare input schema: %v", err)
	}

	inputs := make(map[string]PortSchema, len(definition.Inputs))
	for name := range definition.Inputs {
		inputs[name] = anySchema
	}

	return &compiledCondition{
		spec: NodeSpec{
			Inputs: inputs, Outputs: map[string]PortSchema{},
			Routes: []string{config.TrueRoute, config.FalseRoute},
		},
		config: config,
	}, nil
}

type compiledCondition struct {
	spec   NodeSpec
	config ConditionConfig
}

func (n *compiledCondition) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledCondition) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	matched, err := evaluatePredicate(n.config.Predicate, input.Values)
	if err != nil {
		return NodeOutput{}, err
	}

	route := n.config.FalseRoute
	if matched {
		route = n.config.TrueRoute
	}

	return NodeOutput{Values: map[string]Value{}, Route: route}, nil
}

// MergeMode selects mutually exclusive or parallel fan-in behavior.
type MergeMode string

// Merge modes.
const (
	MergeExclusive MergeMode = "exclusive"
	MergeParallel  MergeMode = "parallel"
)

// MergeOutputConfig maps one output to explicitly named node input candidates.
type MergeOutputConfig struct {
	Schema  PortSchema `json:"schema"`
	Sources []string   `json:"sources"`
}

// MergeConfig defines explicit output mappings for one Merge mode.
type MergeConfig struct {
	Mode    MergeMode                    `json:"mode"`
	Outputs map[string]MergeOutputConfig `json:"outputs"`
}

// MergeNode joins mutually exclusive branches or wait-all parallel inputs.
type MergeNode struct{}

// Spec implements [NodeType].
func (MergeNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{Key: NodeTypeMerge, Version: BuiltinNodeVersion, DisplayName: "Merge"}
}

// Compile implements [NodeType].
func (MergeNode) Compile(
	_ context.Context,
	_ CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	return compileMergeNode(definition, BuiltinNodeVersion)
}

type mergeNodeV2 struct{}

func (mergeNodeV2) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key: NodeTypeMerge, Version: MergeNodeVersionV2, DisplayName: "Merge",
	}
}

func (mergeNodeV2) Compile(
	_ context.Context,
	_ CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	return compileMergeNode(definition, MergeNodeVersionV2)
}

func compileMergeNode(definition NodeDefinition, version string) (CompiledNode, error) {
	var config MergeConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if err := validateMergeConfig(config, definition.Inputs); err != nil {
		return nil, compileNodeError(definition.ID, "merge config: %v", err)
	}

	anySchema, err := ParsePortSchema([]byte("true"))
	if err != nil {
		return nil, compileNodeError(definition.ID, "prepare input schema: %v", err)
	}

	inputs := make(map[string]PortSchema, len(definition.Inputs))
	for name := range definition.Inputs {
		inputs[name] = anySchema
	}

	outputs := make(map[string]PortSchema, len(config.Outputs))
	for name, output := range config.Outputs {
		outputs[name] = output.Schema
	}

	return &compiledMerge{
		spec:    NodeSpec{Inputs: inputs, Outputs: outputs, Routes: []string{RouteSuccess}},
		config:  config,
		version: version,
	}, nil
}

type compiledMerge struct {
	spec    NodeSpec
	config  MergeConfig
	version string
}

func (n *compiledMerge) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledMerge) Invoke(_ context.Context, input NodeInput) (NodeOutput, error) {
	values := make(map[string]Value, len(n.config.Outputs))
	for name, output := range n.config.Outputs {
		value, err := mergeOutputValue(n.version, n.config.Mode, output.Sources, input.Values)
		if err != nil {
			return NodeOutput{}, fmt.Errorf("merge output %q: %w", name, err)
		}

		if err := output.Schema.Validate(value); err != nil {
			return NodeOutput{}, fmt.Errorf("merge output %q: %w", name, err)
		}

		values[name] = value
	}

	return NodeOutput{Values: values, Route: RouteSuccess}, nil
}

func rejectNodeConfig(definition NodeDefinition) error {
	if len(bytes.TrimSpace(definition.Config)) != 0 {
		return compileNodeError(definition.ID, "config is not supported")
	}

	return nil
}

func decodeNodeConfig(definition NodeDefinition, destination any) error {
	if len(bytes.TrimSpace(definition.Config)) == 0 {
		return compileNodeError(definition.ID, "config is required")
	}

	limits := defaultJSONLimits

	limits.maxBytes = maxConfigBytes
	if err := decodeJSON(definition.Config, destination, limits, true); err != nil {
		return compileNodeError(definition.ID, "decode config: %v", err)
	}

	return nil
}

func validatePredicate(
	predicate Predicate,
	inputs map[string]Binding,
	depth int,
	count *int,
) error {
	(*count)++
	if depth > 16 || *count > 128 {
		return errors.New("predicate exceeds depth or node limit")
	}

	if err := validatePredicateOperation(predicate, inputs); err != nil {
		return err
	}

	for _, argument := range predicate.Args {
		if err := validatePredicate(argument, inputs, depth+1, count); err != nil {
			return err
		}
	}

	return nil
}

func validatePredicateOperation(predicate Predicate, inputs map[string]Binding) error {
	switch predicate.Op {
	case PredicateExists:
		return validatePredicateLeaf(predicate, inputs, false, false)
	case PredicateEqual, PredicateNotEqual, PredicateGreater, PredicateGreaterOrEqual,
		PredicateLess, PredicateLessOrEqual:
		return validatePredicateLeaf(predicate, inputs, true, false)
	case PredicateIn:
		return validatePredicateLeaf(predicate, inputs, false, true)
	case PredicateAll, PredicateAny:
		return validatePredicateArgs(predicate, false)
	case PredicateNot:
		return validatePredicateArgs(predicate, true)
	default:
		return fmt.Errorf("unknown operation %q", predicate.Op)
	}
}

func validatePredicateArgs(predicate Predicate, requireOne bool) error {
	validCount := len(predicate.Args) > 0
	if requireOne {
		validCount = len(predicate.Args) == 1
	}

	if !validCount || predicate.Input != "" || predicate.Value != nil || len(predicate.Values) != 0 {
		if requireOne {
			return errors.New("not requires exactly one arg")
		}

		return fmt.Errorf("%s requires only non-empty args", predicate.Op)
	}

	return nil
}

func validatePredicateLeaf(
	predicate Predicate,
	inputs map[string]Binding,
	requiresValue bool,
	requiresValues bool,
) error {
	if !validIdentifier(predicate.Input) {
		return fmt.Errorf("invalid input %q", predicate.Input)
	}

	if _, ok := inputs[predicate.Input]; !ok {
		return fmt.Errorf("unknown input %q", predicate.Input)
	}

	if len(predicate.Args) != 0 {
		return fmt.Errorf("%s does not accept args", predicate.Op)
	}

	for _, segment := range predicate.Path {
		if segment == "" || len(segment) > 128 {
			return fmt.Errorf("%s has invalid path segment %q", predicate.Op, segment)
		}
	}

	if requiresValue != (predicate.Value != nil) {
		return fmt.Errorf("%s has invalid value", predicate.Op)
	}

	if requiresValues != (len(predicate.Values) > 0) {
		return fmt.Errorf("%s has invalid values", predicate.Op)
	}

	for _, value := range predicate.Values {
		if !value.IsValid() {
			return fmt.Errorf("%s contains invalid value", predicate.Op)
		}
	}

	return nil
}

func evaluatePredicate(predicate Predicate, inputs map[string]Value) (bool, error) {
	switch predicate.Op {
	case PredicateAll:
		return evaluateAll(predicate.Args, inputs)
	case PredicateAny:
		return evaluateAny(predicate.Args, inputs)
	case PredicateNot:
		matched, err := evaluatePredicate(predicate.Args[0], inputs)
		return !matched, err
	case PredicateExists, PredicateEqual, PredicateNotEqual, PredicateGreater,
		PredicateGreaterOrEqual, PredicateLess, PredicateLessOrEqual, PredicateIn:
		return evaluatePredicateLeaf(predicate, inputs)
	}

	return false, fmt.Errorf("condition: unsupported operation %q", predicate.Op)
}

func evaluateAll(arguments []Predicate, inputs map[string]Value) (bool, error) {
	for _, argument := range arguments {
		matched, err := evaluatePredicate(argument, inputs)
		if err != nil || !matched {
			return false, err
		}
	}

	return true, nil
}

func evaluateAny(arguments []Predicate, inputs map[string]Value) (bool, error) {
	for _, argument := range arguments {
		matched, err := evaluatePredicate(argument, inputs)
		if err != nil {
			return false, err
		}

		if matched {
			return true, nil
		}
	}

	return false, nil
}

func evaluatePredicateLeaf(predicate Predicate, inputs map[string]Value) (bool, error) {
	actual, present := predicateValue(predicate, inputs)
	if predicate.Op == PredicateExists {
		return present, nil
	}

	if !present {
		return false, nil
	}

	switch predicate.Op {
	case PredicateEqual:
		return actual.Equal(*predicate.Value), nil
	case PredicateNotEqual:
		return !actual.Equal(*predicate.Value), nil
	case PredicateIn:
		return slices.ContainsFunc(predicate.Values, actual.Equal), nil
	case PredicateGreater, PredicateGreaterOrEqual, PredicateLess, PredicateLessOrEqual:
		comparison, ok := compareValues(actual, *predicate.Value)
		if !ok {
			return false, nil
		}

		return predicateComparison(predicate.Op, comparison), nil
	case PredicateExists, PredicateAll, PredicateAny, PredicateNot:
		return false, fmt.Errorf("condition: unsupported leaf operation %q", predicate.Op)
	}

	return false, fmt.Errorf("condition: unsupported operation %q", predicate.Op)
}

func predicateValue(predicate Predicate, inputs map[string]Value) (Value, bool) {
	value, ok := inputs[predicate.Input]
	if !ok {
		return Value{}, false
	}

	if len(predicate.Path) == 0 {
		return value, true
	}

	return value.Lookup(predicate.Path...)
}

func compareValues(left, right Value) (int, bool) {
	leftValue, leftErr := left.Any()

	rightValue, rightErr := right.Any()
	if leftErr != nil || rightErr != nil {
		return 0, false
	}

	switch typedLeft := leftValue.(type) {
	case string:
		typedRight, ok := rightValue.(string)
		if !ok {
			return 0, false
		}

		return bytes.Compare([]byte(typedLeft), []byte(typedRight)), true
	case json.Number:
		typedRight, ok := rightValue.(json.Number)
		if !ok {
			return 0, false
		}

		leftNumber, leftOK := new(big.Rat).SetString(typedLeft.String())

		rightNumber, rightOK := new(big.Rat).SetString(typedRight.String())
		if !leftOK || !rightOK {
			return 0, false
		}

		return leftNumber.Cmp(rightNumber), true
	default:
		return 0, false
	}
}

func predicateComparison(operation PredicateOp, comparison int) bool {
	switch operation {
	case PredicateGreater:
		return comparison > 0
	case PredicateGreaterOrEqual:
		return comparison >= 0
	case PredicateLess:
		return comparison < 0
	case PredicateLessOrEqual:
		return comparison <= 0
	default:
		return false
	}
}

func validateMergeConfig(config MergeConfig, inputs map[string]Binding) error {
	if config.Mode != MergeExclusive && config.Mode != MergeParallel {
		return fmt.Errorf("unknown mode %q", config.Mode)
	}

	if len(config.Outputs) == 0 {
		return errors.New("outputs are required")
	}

	used := make(map[string]struct{}, len(inputs))
	for name, output := range config.Outputs {
		if err := validateMergeOutput(config.Mode, name, output, inputs, used); err != nil {
			return err
		}
	}

	if len(used) != len(inputs) {
		return errors.New("every merge input must be mapped exactly once")
	}

	return nil
}

func validateMergeOutput(
	mode MergeMode,
	name string,
	output MergeOutputConfig,
	inputs map[string]Binding,
	used map[string]struct{},
) error {
	if !validIdentifier(name) || !output.Schema.IsValid() || len(output.Sources) == 0 {
		return fmt.Errorf("invalid output %q", name)
	}

	if mode == MergeParallel && len(output.Sources) != 1 {
		return fmt.Errorf("parallel output %q requires exactly one source", name)
	}

	for _, source := range output.Sources {
		if _, ok := inputs[source]; !ok {
			return fmt.Errorf("output %q references unknown source %q", name, source)
		}

		if _, duplicate := used[source]; duplicate {
			return fmt.Errorf("source %q is mapped more than once", source)
		}

		used[source] = struct{}{}
	}

	return nil
}

func mergeOutputValue(
	version string,
	mode MergeMode,
	sources []string,
	inputs map[string]Value,
) (Value, error) {
	if mode == MergeParallel {
		value, ok := inputs[sources[0]]
		if !ok {
			return Value{}, errors.New("parallel source is missing")
		}

		return value, nil
	}

	if version == MergeNodeVersionV2 {
		for _, source := range sources {
			value, ok := inputs[source]
			if ok && value.Kind() != ValueNull {
				return value, nil
			}
		}

		return ValueOf(nil)
	}

	var selected Value

	count := 0

	for _, source := range sources {
		value, ok := inputs[source]
		if !ok {
			continue
		}

		selected = value
		count++
	}

	if count != 1 {
		return Value{}, fmt.Errorf("exclusive merge requires one value, got %d", count)
	}

	return selected, nil
}

func validatePortValues(values map[string]Value, schemas map[string]PortSchema) error {
	if values == nil {
		return errors.New("nil values")
	}

	if len(values) != len(schemas) {
		return fmt.Errorf("got %d ports, want %d", len(values), len(schemas))
	}

	for name, schema := range schemas {
		value, ok := values[name]
		if !ok {
			return fmt.Errorf("missing port %q", name)
		}

		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("port %q: %w", name, err)
		}
	}

	return nil
}

func cloneValues(values map[string]Value) map[string]Value {
	cloned := make(map[string]Value, len(values))
	maps.Copy(cloned, values)

	return cloned
}

func compileNodeError(nodeID NodeID, format string, values ...any) error {
	return fmt.Errorf("%w: node %q: %s", ErrCompile, nodeID, fmt.Sprintf(format, values...))
}

func wrapCompileNodeError(nodeID NodeID, operation string, err error) error {
	return fmt.Errorf("%w: node %q: %s: %w", ErrCompile, nodeID, operation, err)
}

var (
	_ NodeType = StartNode{}
	_ NodeType = EndNode{}
	_ NodeType = ActionNode{}
	_ NodeType = ConditionNode{}
	_ NodeType = MergeNode{}
	_ NodeType = SelectorNode{}
	_ NodeType = SubWorkflowNode{}
	_ NodeType = BatchNode{}
	_ NodeType = LoopNode{}
	_ NodeType = BreakNode{}
	_ NodeType = ContinueNode{}
	_ NodeType = SetVariableNode{}

	_ CompiledNode = (*passthroughNode)(nil)
	_ CompiledNode = (*compiledAction)(nil)
	_ CompiledNode = (*compiledCondition)(nil)
	_ CompiledNode = (*compiledMerge)(nil)
)
