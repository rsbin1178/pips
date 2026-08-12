package workflow

import "context"

type executionContextKey struct{}

// ExecutionContext identifies one concrete node invocation. Attempt is the
// one-based cumulative attempt for Address within the Run, including attempts
// restored from a checkpoint.
//
// ExecutionContext is correlation metadata, not an external idempotency key.
// Hosts define idempotency according to their own side-effect boundaries.
type ExecutionContext struct {
	RunID        string
	DefinitionID DefinitionID
	Revision     Revision
	Address      NodeAddress
	Attempt      int
}

// GetExecutionContext returns the scheduler-owned identity of the current
// node invocation. It returns false outside an invocation. The returned
// Address is detached and may be modified without affecting runtime state or
// later reads.
func GetExecutionContext(ctx context.Context) (ExecutionContext, bool) {
	if ctx == nil {
		return ExecutionContext{}, false
	}

	identity, ok := ctx.Value(executionContextKey{}).(ExecutionContext)
	if !ok {
		return ExecutionContext{}, false
	}

	identity.Address = cloneNodeAddress(identity.Address)

	return identity, true
}

func invocationExecutionContext(runtime *nodeRuntime, attempt int) ExecutionContext {
	execution := runtime.execution

	return ExecutionContext{
		RunID:        execution.runID,
		DefinitionID: execution.plan.definition.ID,
		Revision:     execution.plan.definition.Revision,
		Address: cloneNodeAddress(NodeAddress{
			NodeID: runtime.nodeID,
			Scope:  execution.scope,
		}),
		Attempt: attempt,
	}
}
