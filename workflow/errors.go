package workflow

import "errors"

// Workflow validation and execution errors. Match them with [errors.Is].
var (
	// ErrInvalidValue classifies malformed or unsupported JSON values.
	ErrInvalidValue = errors.New("workflow: invalid value")
	// ErrInvalidSchema classifies malformed or unsupported port schemas.
	ErrInvalidSchema = errors.New("workflow: invalid schema")
	// ErrSchemaViolation means a Value does not satisfy a PortSchema.
	ErrSchemaViolation = errors.New("workflow: schema violation")
	// ErrInvalidDefinition classifies malformed workflow definitions.
	ErrInvalidDefinition = errors.New("workflow: invalid definition")
	// ErrInvalidRegistry classifies invalid node type or Action registrations.
	ErrInvalidRegistry = errors.New("workflow: invalid registry")
	// ErrCompile classifies a definition that cannot produce an execution plan.
	ErrCompile = errors.New("workflow: compile failed")
	// ErrRun classifies a workflow execution failure.
	ErrRun = errors.New("workflow: run failed")
	// ErrInterrupted classifies a successfully checkpointed, resumable Run.
	ErrInterrupted = errors.New("workflow: interrupted")
	// ErrEventWireFormat reports an attempt to serialize a process-local Event.
	ErrEventWireFormat = errors.New("workflow: event has no wire format")
	// ErrNodeExecutionWireFormat reports an attempt to serialize a sensitive,
	// process-local NodeExecution snapshot.
	ErrNodeExecutionWireFormat = errors.New("workflow: node execution has no wire format")
)
