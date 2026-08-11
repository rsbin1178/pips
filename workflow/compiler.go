package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

type compileConfig struct {
	resolver        DefinitionResolver
	interruptBefore []NodePath
	interruptAfter  []NodePath
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
	loop       *loopCompileScope

	interruptBefore map[int]struct{}
	interruptAfter  map[int]struct{}
	nodeDebug       *nodeDebugPlanMetadata
	partialRun      *partialRunPlanMetadata
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

	interrupts, err := normalizeInterruptPolicy(config)
	if err != nil {
		return nil, fmt.Errorf("%w: interrupt policy: %w", ErrCompile, err)
	}

	session := &compileSession{
		registry: registry,
		resolver: config.resolver,
		stack:    []definitionKey{},
		active:   map[definitionKey]struct{}{},
	}

	return session.compile(ctx, definition, interrupts)
}

func (s *compileSession) compile(
	ctx context.Context,
	definition Definition,
	interrupts *interruptPolicy,
) (*Plan, error) {
	return s.compileScoped(ctx, definition, nil, interrupts)
}

func (s *compileSession) compileScoped(
	ctx context.Context,
	definition Definition,
	loop *loopCompileScope,
	interrupts *interruptPolicy,
) (*Plan, error) {
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
		loop:                  cloneLoopCompileScope(loop),
	}

	if err := plan.compileNodes(ctx, s, interrupts); err != nil {
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

	if err := plan.validateLoopVariableAccess(topological); err != nil {
		return nil, err
	}

	if err := plan.computeFingerprint(); err != nil {
		return nil, err
	}

	return plan, nil
}

func (s *compileSession) resolve(
	ctx context.Context,
	reference DefinitionRef,
	interrupts *interruptPolicy,
) (*Plan, error) {
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

	return s.compile(ctx, definition, interrupts)
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
