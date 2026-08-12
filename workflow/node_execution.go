package workflow

import (
	"context"
	"encoding/json"
)

// NodeExecution is one detached, storage-neutral snapshot of an actual node
// invocation. RunID and Address form its logical upsert identity. Inputs,
// outputs, and error text may contain sensitive application data; hosts own
// persistence, redaction, encryption, retention, and transport schemas.
type NodeExecution struct {
	DefinitionID          DefinitionID
	Revision              Revision
	DefinitionFingerprint string
	PlanFingerprint       string
	RunID                 string
	Address               NodeAddress
	NodeRun               NodeRun
	Inputs                map[string]Value
	Outputs               map[string]Value
	Route                 string
	ErrorMessage          string
}

// NodeExecutionRecorder synchronously receives running and effective outcome
// snapshots for ordinary Run and Resume operations. Implementations handle
// their own persistence errors and timeouts and must not panic.
//
// Calls are serialized within one Run. The same recorder may be called
// concurrently by separate Runs.
type NodeExecutionRecorder func(context.Context, NodeExecution)

// MarshalJSON rejects an accidental unversioned wire format for sensitive
// process-local execution data.
func (NodeExecution) MarshalJSON() ([]byte, error) {
	return nil, ErrNodeExecutionWireFormat
}

// UnmarshalJSON rejects an accidental unversioned wire format for sensitive
// process-local execution data.
func (*NodeExecution) UnmarshalJSON([]byte) error {
	return ErrNodeExecutionWireFormat
}

func projectNodeExecution(
	execution *execution,
	index int,
	nodeRun NodeRun,
	inputs map[string]Value,
	outputs map[string]Value,
	route string,
	errorMessage string,
) NodeExecution {
	return NodeExecution{
		DefinitionID:          execution.plan.definition.ID,
		Revision:              execution.plan.definition.Revision,
		DefinitionFingerprint: execution.plan.definitionFingerprint,
		PlanFingerprint:       execution.plan.fingerprint,
		RunID:                 execution.runID,
		Address:               execution.nodeAddress(index),
		NodeRun:               nodeRun,
		Inputs:                cloneValues(inputs),
		Outputs:               cloneValues(outputs),
		Route:                 route,
		ErrorMessage:          errorMessage,
	}
}

func cloneNodeExecution(record NodeExecution) NodeExecution {
	record.Address = cloneNodeAddress(record.Address)
	record.Inputs = cloneValues(record.Inputs)
	record.Outputs = cloneValues(record.Outputs)

	return record
}

func (e *execution) recordNodeExecution(
	ctx context.Context,
	index int,
	nodeRun NodeRun,
	inputs map[string]Value,
	outputs map[string]Value,
	route string,
	errorMessage string,
) {
	if e == nil || e.runner == nil || e.runner.nodeExecutionRecorder == nil ||
		e.state == nil || e.state.nodeDebug != nil || e.state.partialRun != nil ||
		nodeRun.StartedAt.IsZero() {
		return
	}

	record := projectNodeExecution(
		e,
		index,
		nodeRun,
		inputs,
		outputs,
		route,
		errorMessage,
	)

	e.state.nodeExecutionMu.Lock()
	defer e.state.nodeExecutionMu.Unlock()

	e.runner.nodeExecutionRecorder(context.WithoutCancel(ctx), cloneNodeExecution(record))
}

// Compile-time assertion that the explicit rejection remains the JSON entry
// point even though NodeExecution intentionally has no stable field tags.
var (
	_ json.Marshaler   = NodeExecution{}
	_ json.Unmarshaler = (*NodeExecution)(nil)
)
