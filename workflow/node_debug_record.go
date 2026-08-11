package workflow

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const nodeDebugCheckpointVersion = 1

type nodeDebugCheckpoint struct {
	Version     int                            `json:"version"`
	Fingerprint string                         `json:"fingerprint"`
	Target      []NodeID                       `json:"target"`
	Records     []nodeDebugExecutionCheckpoint `json:"records"`
}

type nodeDebugExecutionCheckpoint struct {
	Address      NodeAddress       `json:"address"`
	Node         checkpointNodeRun `json:"node"`
	Inputs       map[string]Value  `json:"inputs"`
	Outputs      map[string]Value  `json:"outputs"`
	Route        string            `json:"route,omitempty"`
	ErrorMessage string            `json:"error_message,omitempty"`
}

type nodeDebugCollector struct {
	mu      sync.Mutex
	records map[string]NodeDebugExecution
}

func newNodeDebugCollector(node planNode, inputs map[string]Value) *nodeDebugCollector {
	address := NodeAddress{NodeID: node.definition.ID, Scope: []ScopeFrame{}}
	record := NodeDebugExecution{
		Address: address,
		NodeRun: NodeRun{
			ID:     node.definition.ID,
			Type:   node.definition.Type,
			Status: NodeStatusPending,
		},
		Inputs:  cloneValues(inputs),
		Outputs: map[string]Value{},
	}

	return &nodeDebugCollector{
		records: map[string]NodeDebugExecution{nodeDebugAddressKey(address): record},
	}
}

func (c *nodeDebugCollector) record(
	execution *execution,
	index int,
	inputs map[string]Value,
	outputs map[string]Value,
	route string,
	errorMessage string,
) {
	if c == nil || execution == nil || index < 0 || index >= len(execution.nodes) {
		return
	}

	nodeType := execution.plan.nodes[index].definition.Type
	if nodeType == NodeTypeStart || nodeType == NodeTypeEnd {
		return
	}

	record := NodeDebugExecution{
		Address:      execution.nodeAddress(index),
		NodeRun:      execution.nodes[index],
		Inputs:       cloneValues(inputs),
		Outputs:      cloneValues(outputs),
		Route:        route,
		ErrorMessage: errorMessage,
	}

	c.mu.Lock()
	c.records[nodeDebugAddressKey(record.Address)] = record
	c.mu.Unlock()
}

func (c *nodeDebugCollector) synchronize(execution *execution) {
	if c == nil || execution == nil {
		return
	}

	for index, summary := range execution.nodes {
		nodeType := execution.plan.nodes[index].definition.Type
		if nodeType == NodeTypeStart || nodeType == NodeTypeEnd ||
			summary.Status == NodeStatusPending {
			continue
		}

		address := execution.nodeAddress(index)
		key := nodeDebugAddressKey(address)

		c.mu.Lock()
		record, exists := c.records[key]
		c.mu.Unlock()

		if !exists {
			inputs, err := execution.resolveInputs(index)
			if err != nil {
				inputs = map[string]Value{}
			}

			record = NodeDebugExecution{
				Address: address,
				Inputs:  inputs,
				Outputs: map[string]Value{},
			}
		}

		record.Address = address

		record.NodeRun = summary
		if execution.outputs[index] != nil && len(record.Outputs) == 0 {
			record.Outputs = cloneValues(execution.outputs[index])
		}

		c.mu.Lock()
		c.records[key] = cloneNodeDebugExecution(record)
		c.mu.Unlock()
	}
}

func (c *nodeDebugCollector) snapshot() []NodeDebugExecution {
	if c == nil {
		return nil
	}

	c.mu.Lock()

	records := make([]NodeDebugExecution, 0, len(c.records))
	for _, record := range c.records {
		records = append(records, cloneNodeDebugExecution(record))
	}
	c.mu.Unlock()

	slices.SortFunc(records, func(left, right NodeDebugExecution) int {
		return compareNodeDebugAddress(left.Address, right.Address)
	})

	return records
}

func (c *nodeDebugCollector) checkpoint(metadata *nodeDebugPlanMetadata) *nodeDebugCheckpoint {
	if c == nil || metadata == nil {
		return nil
	}

	records := c.snapshot()

	checkpoint := &nodeDebugCheckpoint{
		Version:     nodeDebugCheckpointVersion,
		Fingerprint: metadata.fingerprint,
		Target:      metadata.target.Nodes(),
		Records:     make([]nodeDebugExecutionCheckpoint, len(records)),
	}
	for index, record := range records {
		checkpoint.Records[index] = nodeDebugExecutionCheckpoint{
			Address:      cloneNodeAddress(record.Address),
			Node:         checkpointNodeRun(record.NodeRun),
			Inputs:       cloneValues(record.Inputs),
			Outputs:      cloneValues(record.Outputs),
			Route:        record.Route,
			ErrorMessage: record.ErrorMessage,
		}
	}

	return checkpoint
}

func restoreNodeDebugCollector(checkpoint *nodeDebugCheckpoint) *nodeDebugCollector {
	collector := &nodeDebugCollector{
		records: make(map[string]NodeDebugExecution, len(checkpoint.Records)),
	}
	for _, stored := range checkpoint.Records {
		record := NodeDebugExecution{
			Address:      cloneNodeAddress(stored.Address),
			NodeRun:      NodeRun(stored.Node),
			Inputs:       cloneValues(stored.Inputs),
			Outputs:      cloneValues(stored.Outputs),
			Route:        stored.Route,
			ErrorMessage: stored.ErrorMessage,
		}
		collector.records[nodeDebugAddressKey(record.Address)] = record
	}

	return collector
}

func validateNodeDebugCheckpoint(checkpoint *nodeDebugCheckpoint, plan *Plan) error {
	if err := validateNodeDebugCheckpointIdentity(checkpoint, plan); err != nil {
		return err
	}

	if plan.nodeDebug == nil {
		return nil
	}

	seen := make(map[string]struct{}, len(checkpoint.Records))
	rootFound := false
	rootID := plan.nodes[0].definition.ID

	for _, record := range checkpoint.Records {
		if err := validateNodeDebugCheckpointRecord(record, plan); err != nil {
			return err
		}

		key := nodeDebugAddressKey(record.Address)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("node debug checkpoint has duplicate address")
		}

		seen[key] = struct{}{}

		if len(record.Address.Scope) == 0 && record.Address.NodeID == rootID {
			rootFound = true
		}
	}

	if !rootFound {
		return errors.New("node debug checkpoint root record is missing")
	}

	return nil
}

func validateNodeDebugCheckpointIdentity(checkpoint *nodeDebugCheckpoint, plan *Plan) error {
	if plan.nodeDebug == nil {
		if checkpoint != nil {
			return errors.New("ordinary checkpoint contains node debug state")
		}

		return nil
	}

	if checkpoint == nil {
		return errors.New("node debug checkpoint state is missing")
	}

	if checkpoint.Version != nodeDebugCheckpointVersion {
		return fmt.Errorf("unsupported node debug checkpoint version %d", checkpoint.Version)
	}

	if checkpoint.Fingerprint != plan.nodeDebug.fingerprint ||
		!slices.Equal(checkpoint.Target, plan.nodeDebug.target.nodes) {
		return errors.New("node debug checkpoint identity mismatch")
	}

	if len(checkpoint.Records) == 0 {
		return errors.New("node debug checkpoint records are missing")
	}

	return nil
}

func validateNodeDebugCheckpointRecord(
	record nodeDebugExecutionCheckpoint,
	plan *Plan,
) error {
	if err := validateNodeAddress(record.Address); err != nil {
		return fmt.Errorf("node debug checkpoint record: %w", err)
	}

	node, ok := planNodeAtAddress(plan, record.Address)
	if !ok || node.definition.Type == NodeTypeStart || node.definition.Type == NodeTypeEnd {
		return errors.New("node debug checkpoint record has unknown or endpoint address")
	}

	if record.Node.ID != node.definition.ID || record.Node.Type != node.definition.Type ||
		record.Node.Attempts < 0 {
		return errors.New("node debug checkpoint record identity mismatch")
	}

	if record.Inputs == nil || record.Outputs == nil {
		return errors.New("node debug checkpoint record has nil values")
	}

	if record.Node.Status != NodeStatusSkipped {
		if err := validatePlanNodeInputValues(record.Inputs, node); err != nil {
			return fmt.Errorf("node debug checkpoint record inputs: %w", err)
		}
	} else if len(record.Inputs) != 0 {
		return errors.New("node debug checkpoint skipped record has inputs")
	}

	return validateNodeDebugRecordOutcome(record, node)
}

func validateNodeDebugRecordOutcome(
	record nodeDebugExecutionCheckpoint,
	node planNode,
) error {
	var err error

	switch record.Node.Status {
	case NodeStatusReady, NodeStatusInterrupted:
		err = validateNodeDebugUnfinishedRecord(record)
	case NodeStatusSkipped:
		err = validateNodeDebugSkippedRecord(record)
	case NodeStatusSucceeded:
		err = validateNodeDebugSucceededRecord(record, node)
	case NodeStatusFailed:
		err = validateNodeDebugFailedRecord(record, node)
	default:
		return errors.New("node debug checkpoint has invalid record status")
	}

	if err != nil {
		return err
	}

	if !record.Node.StartedAt.IsZero() && !record.Node.EndedAt.IsZero() &&
		record.Node.EndedAt.Before(record.Node.StartedAt) {
		return errors.New("node debug checkpoint record has invalid timing")
	}

	return nil
}

func validateNodeDebugUnfinishedRecord(record nodeDebugExecutionCheckpoint) error {
	if record.Route != "" || len(record.Outputs) != 0 ||
		record.ErrorMessage != "" || record.Node.Failure != "" ||
		!record.Node.EndedAt.IsZero() {
		return errors.New("node debug checkpoint has invalid unfinished record")
	}

	return nil
}

func validateNodeDebugSkippedRecord(record nodeDebugExecutionCheckpoint) error {
	if record.Route != "" || len(record.Outputs) != 0 ||
		record.ErrorMessage != "" || record.Node.Failure != "" {
		return errors.New("node debug checkpoint has invalid skipped record")
	}

	return nil
}

func validateNodeDebugSucceededRecord(
	record nodeDebugExecutionCheckpoint,
	node planNode,
) error {
	if record.ErrorMessage != "" || record.Node.Failure != "" ||
		!slices.Contains(node.spec.Routes, record.Route) {
		return errors.New("node debug checkpoint has invalid successful record")
	}

	if err := validatePortValues(record.Outputs, node.spec.Outputs); err != nil {
		return fmt.Errorf("node debug checkpoint record outputs: %w", err)
	}

	return nil
}

func validateNodeDebugFailedRecord(
	record nodeDebugExecutionCheckpoint,
	node planNode,
) error {
	if record.ErrorMessage == "" || !validFailureKind(record.Node.Failure) {
		return errors.New("node debug checkpoint has invalid failed record")
	}

	switch node.definition.Policy.Error {
	case ErrorRoute:
		if record.Route != RouteError || len(record.Outputs) != 0 {
			return errors.New("node debug checkpoint has invalid error-route record")
		}
	case ErrorContinueWithDefault:
		if record.Route != RouteSuccess {
			return errors.New("node debug checkpoint has invalid default-output route")
		}

		if err := validatePortValues(record.Outputs, node.spec.Outputs); err != nil {
			return fmt.Errorf("node debug checkpoint default outputs: %w", err)
		}
	default:
		if record.Route != "" || len(record.Outputs) != 0 {
			return errors.New("node debug checkpoint has invalid stopped record")
		}
	}

	return nil
}

func validFailureKind(kind FailureKind) bool {
	switch kind {
	case FailureError, FailureTimeout, FailurePanic, FailureCanceled, FailureLimit:
		return true
	default:
		return false
	}
}

func planNodeAtAddress(plan *Plan, address NodeAddress) (planNode, bool) {
	current := plan
	for _, frame := range address.Scope {
		index, ok := current.nodeIndex[frame.NodeID]
		if !ok {
			return planNode{}, false
		}

		node := current.nodes[index]
		if (frame.Kind == ScopeSubWorkflow && node.definition.Type != NodeTypeSubWorkflow) ||
			(frame.Kind == ScopeBatchItem && node.definition.Type != NodeTypeBatch) ||
			(frame.Kind == ScopeLoopIteration && node.definition.Type != NodeTypeLoop) {
			return planNode{}, false
		}

		children := childPlans(node.executor)
		if len(children) != 1 || children[0] == nil {
			return planNode{}, false
		}

		current = children[0]
	}

	index, ok := current.nodeIndex[address.NodeID]
	if !ok {
		return planNode{}, false
	}

	return current.nodes[index], true
}

func nodeDebugAddressKey(address NodeAddress) string {
	var builder strings.Builder
	writeNodeDebugKeyPart(&builder, string(address.NodeID))

	for _, frame := range address.Scope {
		writeNodeDebugKeyPart(&builder, string(frame.Kind))
		writeNodeDebugKeyPart(&builder, string(frame.NodeID))
		writeNodeDebugKeyPart(&builder, strconv.Itoa(frame.Index))
	}

	return builder.String()
}

func writeNodeDebugKeyPart(builder *strings.Builder, part string) {
	builder.WriteString(strconv.Itoa(len(part)))
	builder.WriteByte(':')
	builder.WriteString(part)
	builder.WriteByte(';')
}

func compareNodeDebugAddress(left, right NodeAddress) int {
	for index := range min(len(left.Scope), len(right.Scope)) {
		leftFrame := left.Scope[index]
		rightFrame := right.Scope[index]

		if order := cmp.Compare(leftFrame.Kind, rightFrame.Kind); order != 0 {
			return order
		}

		if order := cmp.Compare(leftFrame.NodeID, rightFrame.NodeID); order != 0 {
			return order
		}

		if order := cmp.Compare(leftFrame.Index, rightFrame.Index); order != 0 {
			return order
		}
	}

	if order := cmp.Compare(len(left.Scope), len(right.Scope)); order != 0 {
		return order
	}

	return cmp.Compare(left.NodeID, right.NodeID)
}
