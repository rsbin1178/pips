package workflow

import "context"

const maxChildPlanDepth = 8

// DefinitionRef pins one exact immutable Workflow definition.
type DefinitionRef struct {
	ID          DefinitionID `json:"id"`
	Revision    Revision     `json:"revision"`
	Fingerprint string       `json:"fingerprint"`
}

// DefinitionResolver resolves exact Workflow revisions for compilation.
// Implementations must be safe for concurrent calls and return detached
// Definition snapshots.
type DefinitionResolver interface {
	ResolveDefinition(context.Context, DefinitionID, Revision) (Definition, error)
}

// DefinitionResolverFunc adapts a function to [DefinitionResolver].
type DefinitionResolverFunc func(context.Context, DefinitionID, Revision) (Definition, error)

// ResolveDefinition implements [DefinitionResolver].
func (f DefinitionResolverFunc) ResolveDefinition(
	ctx context.Context,
	id DefinitionID,
	revision Revision,
) (Definition, error) {
	return f(ctx, id, revision)
}

type compositeCompileContext interface {
	compileDefinition(context.Context, Definition) (*Plan, error)
	resolveDefinition(context.Context, DefinitionRef) (*Plan, error)
}

type compiledCompositeNode interface {
	CompiledNode
	childPlans() []*Plan
}

type definitionKey struct {
	id       DefinitionID
	revision Revision
}

type childPlanIdentity struct {
	Node        NodeID `json:"node"`
	Fingerprint string `json:"fingerprint"`
}
