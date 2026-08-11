package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
)

// NodeTypeSpec identifies one registered node implementation.
type NodeTypeSpec struct {
	Key         NodeTypeKey
	Version     string
	DisplayName string
}

// NodeSpec is the immutable input, output, and route contract produced while
// compiling one node instance.
type NodeSpec struct {
	Inputs  map[string]PortSchema
	Outputs map[string]PortSchema
	Routes  []string
}

// NodeInput contains resolved immutable input values for one invocation.
type NodeInput struct {
	Values map[string]Value

	runtime *nodeRuntime
}

// NodeOutput contains immutable output values and the selected control route.
type NodeOutput struct {
	Values map[string]Value
	Route  string
}

// CompiledNode is an immutable, concurrent-safe node executor. Invoke must not
// retain or mutate NodeInput.Values.
type CompiledNode interface {
	Spec() NodeSpec
	Invoke(context.Context, NodeInput) (NodeOutput, error)
}

// CompileContext exposes Definition contracts and registered Actions without
// exposing mutable Registry internals.
type CompileContext interface {
	WorkflowInputs() map[string]PortSchema
	WorkflowOutputs() map[string]PortSchema
	Action(ActionKey, string) (Action, bool)
}

// NodeType compiles one trusted, application-linked node implementation.
type NodeType interface {
	Spec() NodeTypeSpec
	Compile(context.Context, CompileContext, NodeDefinition) (CompiledNode, error)
}

// ActionSpec identifies one host Action and its data contract.
type ActionSpec struct {
	Key     ActionKey
	Version string
	Inputs  map[string]PortSchema
	Outputs map[string]PortSchema
}

// ActionInput carries resolved values and optional immutable node parameters.
type ActionInput struct {
	Values     map[string]Value
	Parameters Value
}

// ActionOutput contains values produced by an Action.
type ActionOutput struct {
	Values map[string]Value
}

// Action is trusted host behavior referenced by key and exact version from an
// ActionNode Definition. Implementations must be safe for concurrent Runs,
// honor context cancellation, and avoid retaining or mutating ActionInput.
type Action interface {
	Spec() ActionSpec
	Run(context.Context, ActionInput) (ActionOutput, error)
}

type registryKey struct {
	name    string
	version string
}

// Registry is an immutable, concurrent-safe snapshot of NodeTypes and Actions.
type Registry struct {
	nodeTypes   map[registryKey]NodeType
	actions     map[registryKey]Action
	fingerprint string
}

// NewRegistry validates and freezes explicitly supplied NodeTypes and Actions.
func NewRegistry(nodeTypes []NodeType, actions []Action) (*Registry, error) {
	registry := &Registry{
		nodeTypes: make(map[registryKey]NodeType, len(nodeTypes)),
		actions:   make(map[registryKey]Action, len(actions)),
	}

	for _, nodeType := range nodeTypes {
		if isNilInterface(nodeType) {
			return nil, fmt.Errorf("%w: nil node type", ErrInvalidRegistry)
		}

		spec := nodeType.Spec()
		if !validNodeTypeSpec(spec) {
			return nil, fmt.Errorf("%w: invalid node type %q@%q", ErrInvalidRegistry, spec.Key, spec.Version)
		}

		key := registryKey{name: string(spec.Key), version: spec.Version}
		if _, duplicate := registry.nodeTypes[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate node type %q@%q", ErrInvalidRegistry, spec.Key, spec.Version)
		}

		registry.nodeTypes[key] = nodeTypeSnapshot{spec: spec, nodeType: nodeType}
	}

	for _, action := range actions {
		if isNilInterface(action) {
			return nil, fmt.Errorf("%w: nil action", ErrInvalidRegistry)
		}

		spec := action.Spec()
		if err := validateActionSpec(spec); err != nil {
			return nil, err
		}

		key := registryKey{name: string(spec.Key), version: spec.Version}
		if _, duplicate := registry.actions[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate action %q@%q", ErrInvalidRegistry, spec.Key, spec.Version)
		}

		registry.actions[key] = actionSnapshot{spec: cloneActionSpec(spec), action: action}
	}

	fingerprint, err := registryFingerprint(registry)
	if err != nil {
		return nil, err
	}

	registry.fingerprint = fingerprint

	return registry, nil
}

// NodeType resolves one exact node implementation version.
func (r *Registry) NodeType(key NodeTypeKey, version string) (NodeType, bool) {
	if r == nil {
		return nil, false
	}

	nodeType, ok := r.nodeTypes[registryKey{name: string(key), version: version}]

	return nodeType, ok
}

// Action resolves one exact Action implementation version.
func (r *Registry) Action(key ActionKey, version string) (Action, bool) {
	if r == nil {
		return nil, false
	}

	action, ok := r.actions[registryKey{name: string(key), version: version}]

	return action, ok
}

// Fingerprint identifies the Registry contracts pinned into a Plan.
func (r *Registry) Fingerprint() string {
	if r == nil {
		return ""
	}

	return r.fingerprint
}

func validateActionSpec(spec ActionSpec) error {
	if !validIdentifier(string(spec.Key)) || !validIdentifier(spec.Version) {
		return fmt.Errorf("%w: invalid action %q@%q", ErrInvalidRegistry, spec.Key, spec.Version)
	}

	if spec.Inputs == nil || spec.Outputs == nil {
		return fmt.Errorf("%w: action %q requires input and output contracts", ErrInvalidRegistry, spec.Key)
	}

	for name, schema := range spec.Inputs {
		if !validIdentifier(name) || !schema.IsValid() {
			return fmt.Errorf("%w: action %q has invalid input %q", ErrInvalidRegistry, spec.Key, name)
		}
	}

	for name, schema := range spec.Outputs {
		if !validIdentifier(name) || !schema.IsValid() {
			return fmt.Errorf("%w: action %q has invalid output %q", ErrInvalidRegistry, spec.Key, name)
		}
	}

	return nil
}

func validNodeTypeSpec(spec NodeTypeSpec) bool {
	return validIdentifier(string(spec.Key)) && validIdentifier(spec.Version) && spec.DisplayName != ""
}

func validateNodeSpec(spec NodeSpec) error {
	if spec.Inputs == nil || spec.Outputs == nil || len(spec.Routes) == 0 {
		return fmt.Errorf("%w: node spec requires inputs, outputs, and routes", ErrCompile)
	}

	for name, schema := range spec.Inputs {
		if !validIdentifier(name) || !schema.IsValid() {
			return fmt.Errorf("%w: invalid node input %q", ErrCompile, name)
		}
	}

	for name, schema := range spec.Outputs {
		if !validIdentifier(name) || !schema.IsValid() {
			return fmt.Errorf("%w: invalid node output %q", ErrCompile, name)
		}
	}

	seen := make(map[string]struct{}, len(spec.Routes))
	for _, route := range spec.Routes {
		if !validIdentifier(route) {
			return fmt.Errorf("%w: invalid node route %q", ErrCompile, route)
		}

		if _, duplicate := seen[route]; duplicate {
			return fmt.Errorf("%w: duplicate node route %q", ErrCompile, route)
		}

		seen[route] = struct{}{}
	}

	return nil
}

func cloneNodeSpec(spec NodeSpec) NodeSpec {
	return NodeSpec{
		Inputs:  cloneSchemas(spec.Inputs),
		Outputs: cloneSchemas(spec.Outputs),
		Routes:  slices.Clone(spec.Routes),
	}
}

func cloneActionSpec(spec ActionSpec) ActionSpec {
	return ActionSpec{
		Key:     spec.Key,
		Version: spec.Version,
		Inputs:  cloneSchemas(spec.Inputs),
		Outputs: cloneSchemas(spec.Outputs),
	}
}

func cloneSchemas(schemas map[string]PortSchema) map[string]PortSchema {
	cloned := make(map[string]PortSchema, len(schemas))
	maps.Copy(cloned, schemas)

	return cloned
}

func registryFingerprint(registry *Registry) (string, error) {
	contracts := make([]registryContract, 0, len(registry.nodeTypes)+len(registry.actions))
	for _, nodeType := range registry.nodeTypes {
		spec := nodeType.Spec()
		contracts = append(contracts, registryContract{
			Kind:    "node",
			Key:     string(spec.Key),
			Version: spec.Version,
		})
	}

	for _, action := range registry.actions {
		spec := cloneActionSpec(action.Spec())
		contracts = append(contracts, registryContract{
			Kind:    "action",
			Key:     string(spec.Key),
			Version: spec.Version,
			Inputs:  schemaFingerprints(spec.Inputs),
			Outputs: schemaFingerprints(spec.Outputs),
		})
	}

	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].Kind != contracts[j].Kind {
			return contracts[i].Kind < contracts[j].Kind
		}

		if contracts[i].Key != contracts[j].Key {
			return contracts[i].Key < contracts[j].Key
		}

		return contracts[i].Version < contracts[j].Version
	})

	data, err := json.Marshal(contracts)
	if err != nil {
		return "", fmt.Errorf("%w: encode fingerprint: %w", ErrInvalidRegistry, err)
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
}

type registryContract struct {
	Kind    string            `json:"kind"`
	Key     string            `json:"key"`
	Version string            `json:"version"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

func schemaFingerprints(schemas map[string]PortSchema) map[string]string {
	fingerprints := make(map[string]string, len(schemas))
	for name, schema := range schemas {
		fingerprints[name] = schema.Fingerprint()
	}

	return fingerprints
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type nodeCompileContext struct {
	inputs     map[string]PortSchema
	outputs    map[string]PortSchema
	registry   *Registry
	session    *compileSession
	loop       *loopCompileScope
	interrupts *interruptPolicy
}

type nodeTypeSnapshot struct {
	spec     NodeTypeSpec
	nodeType NodeType
}

func (n nodeTypeSnapshot) Spec() NodeTypeSpec {
	return n.spec
}

func (n nodeTypeSnapshot) Compile(
	ctx context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	return n.nodeType.Compile(ctx, compileContext, definition)
}

type actionSnapshot struct {
	spec   ActionSpec
	action Action
}

func (a actionSnapshot) Spec() ActionSpec {
	return cloneActionSpec(a.spec)
}

func (a actionSnapshot) Run(ctx context.Context, input ActionInput) (ActionOutput, error) {
	return a.action.Run(ctx, input)
}

func (c nodeCompileContext) WorkflowInputs() map[string]PortSchema {
	return cloneSchemas(c.inputs)
}

func (c nodeCompileContext) WorkflowOutputs() map[string]PortSchema {
	return cloneSchemas(c.outputs)
}

func (c nodeCompileContext) Action(key ActionKey, version string) (Action, bool) {
	return c.registry.Action(key, version)
}

func (c nodeCompileContext) compileDefinition(
	ctx context.Context,
	definition Definition,
) (*Plan, error) {
	return c.session.compile(ctx, definition, c.interrupts)
}

func (c nodeCompileContext) compileLoopDefinition(
	ctx context.Context,
	definition Definition,
	scope loopCompileScope,
) (*Plan, error) {
	return c.session.compileScoped(ctx, definition, &scope, c.interrupts)
}

func (c nodeCompileContext) resolveDefinition(
	ctx context.Context,
	reference DefinitionRef,
) (*Plan, error) {
	return c.session.resolve(ctx, reference, c.interrupts)
}

func (c nodeCompileContext) loopCompileScope() *loopCompileScope {
	return c.loop
}
