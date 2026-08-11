package workflow

import "slices"

func (e *eventEmitter) run(payload EventPayload) {
	e.emit(-1, "", 0, payload)
}

func (e *eventEmitter) node(nodeIndex, attempt int, payload EventPayload) {
	node := e.plan.nodes[nodeIndex].definition
	e.emit(nodeIndex, node.Type, attempt, payload)
}

func (e *eventEmitter) emit(
	nodeIndex int,
	nodeType NodeTypeKey,
	attempt int,
	payload EventPayload,
) {
	if e.runner.eventSink == nil {
		return
	}

	e.state.eventMu.Lock()
	defer e.state.eventMu.Unlock()

	var nodeID NodeID
	if nodeIndex >= 0 {
		nodeID = e.plan.nodes[nodeIndex].definition.ID
	}

	e.runner.eventSink(Event{
		DefinitionID:          e.plan.definition.ID,
		Revision:              e.plan.definition.Revision,
		DefinitionFingerprint: e.plan.definitionFingerprint,
		PlanFingerprint:       e.plan.fingerprint,
		RunID:                 e.runID,
		NodeID:                nodeID,
		NodeType:              nodeType,
		Attempt:               attempt,
		Time:                  e.runner.clock().UTC(),
		payload:               payload,
		scope:                 slices.Clone(e.scope),
	})
}
