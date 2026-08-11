package workflow

import (
	"maps"
	"time"
)

// PinData maps root node IDs to explicit successful output snapshots used by
// [Runner.RunPartial]. Pins are development data and are never consulted by
// [Runner.Run] or [Runner.Resume].
type PinData map[NodeID]map[string]Value

// PartialNodeData is one reusable successful node result.
type PartialNodeData struct {
	Outputs map[string]Value
	Route   string
}

// PartialRunData is a detached, host-persistable snapshot produced by a
// Partial Run. It is prior execution data, not a resumable checkpoint.
type PartialRunData struct {
	RunID                 string
	DefinitionID          DefinitionID
	Revision              Revision
	SourcePlanFingerprint string
	Nodes                 map[NodeID]PartialNodeData
}

// PartialDataOrigin identifies how one node obtained its effective result in a
// Partial Run. Its zero value means no result was materialized or executed.
type PartialDataOrigin string

// Partial Run data origins.
const (
	PartialDataExecuted PartialDataOrigin = "executed"
	PartialDataPinned   PartialDataOrigin = "pinned"
	PartialDataReused   PartialDataOrigin = "reused"
)

// PartialNodeRun combines one node summary with its Partial Run data origin.
type PartialNodeRun struct {
	NodeRun NodeRun
	Origin  PartialDataOrigin
}

// PartialRunInput supplies Workflow inputs and optional host-owned development
// data for [Runner.RunPartial].
type PartialRunInput struct {
	Inputs   map[string]Value
	Previous *PartialRunData
	Pins     PinData
	Dirty    []NodeID
}

// PartialRunResult is a detached snapshot of one terminal or interrupted
// Partial Run. Data can be persisted by the host and supplied as Previous to a
// later Partial Run.
type PartialRunResult struct {
	RunID                 string
	Destination           NodeID
	SourcePlanFingerprint string
	PlanFingerprint       string
	Status                RunStatus
	Outputs               map[string]Value
	Nodes                 map[NodeID]PartialNodeRun
	Data                  PartialRunData
	StartedAt             time.Time
	EndedAt               time.Time
	Interruption          *InterruptInfo
}

func clonePartialNodeData(data PartialNodeData) PartialNodeData {
	data.Outputs = cloneValues(data.Outputs)

	return data
}

func clonePartialRunData(data PartialRunData) PartialRunData {
	nodes := make(map[NodeID]PartialNodeData, len(data.Nodes))
	for nodeID, node := range data.Nodes {
		nodes[nodeID] = clonePartialNodeData(node)
	}

	data.Nodes = nodes

	return data
}

func clonePartialRunResult(result PartialRunResult) PartialRunResult {
	result.Outputs = cloneValues(result.Outputs)

	nodes := make(map[NodeID]PartialNodeRun, len(result.Nodes))
	maps.Copy(nodes, result.Nodes)
	result.Nodes = nodes
	result.Data = clonePartialRunData(result.Data)

	if result.Interruption != nil {
		info := cloneInterruptInfo(*result.Interruption)
		result.Interruption = &info
	}

	return result
}
