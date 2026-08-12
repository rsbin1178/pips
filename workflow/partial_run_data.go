package workflow

import (
	"errors"
	"fmt"
	"slices"
)

const maxPartialRunDataBytes = 16 << 20

type partialMaterializedData struct {
	outputs map[string]Value
	route   string
	origin  PartialDataOrigin
}

type partialRunState struct {
	sourcePlanFingerprint string
	destination           NodeID
	workflowInputs        map[string]struct{}
	available             []*partialMaterializedData
	origins               []PartialDataOrigin
	routes                []string
	outputs               []map[string]Value
}

func normalizePartialRunInput(
	source *Plan,
	plan *Plan,
	input PartialRunInput,
) (map[string]Value, *partialRunState, error) {
	if source == nil || plan == nil || plan.partialRun == nil {
		return nil, nil, errors.New("invalid partial run plan")
	}

	if err := validatePartialRunDataBounds(input); err != nil {
		return nil, nil, err
	}

	resolvedDirty := resolvePartialDirty(source, input.Dirty)

	if err := validatePreviousPartialRunData(source, input.Previous, resolvedDirty); err != nil {
		return nil, nil, err
	}

	state := newPartialRunState(source, plan)

	if err := state.normalizePins(plan, input.Pins); err != nil {
		return nil, nil, err
	}

	previous := preparePreviousPartialData(plan, input.Previous, resolvedDirty, state)

	if err := state.retainReachablePrevious(plan, previous); err != nil {
		return nil, nil, err
	}

	required := state.requiredWorkflowInputs(plan)
	state.workflowInputs = required

	inputs, err := normalizeWorkflowInputs(plan.definition.Inputs, input.Inputs, required)
	if err != nil {
		return nil, nil, err
	}

	return inputs, state, nil
}

func resolvePartialDirty(source *Plan, dirty []NodeID) map[NodeID]struct{} {
	resolved := make(map[NodeID]struct{}, len(dirty))
	for _, nodeID := range dirty {
		if _, ok := source.nodeIndex[nodeID]; ok {
			resolved[nodeID] = struct{}{}
		}
	}

	return resolved
}

func newPartialRunState(source, plan *Plan) *partialRunState {
	return &partialRunState{
		sourcePlanFingerprint: source.fingerprint,
		destination:           plan.partialRun.destination,
		available:             make([]*partialMaterializedData, len(plan.nodes)),
		origins:               make([]PartialDataOrigin, len(plan.nodes)),
		routes:                make([]string, len(plan.nodes)),
		outputs:               make([]map[string]Value, len(plan.nodes)),
	}
}

func preparePreviousPartialData(
	plan *Plan,
	previousData *PartialRunData,
	resolvedDirty map[NodeID]struct{},
	state *partialRunState,
) map[NodeID]PartialNodeData {
	previous := selectedPreviousData(plan, previousData)
	for nodeID := range resolvedDirty {
		if index, ok := plan.nodeIndex[nodeID]; ok {
			invalidatePartialDescendants(previous, plan, index)
		}
	}

	if state.available[plan.endIndex] == nil {
		delete(previous, plan.nodes[plan.endIndex].definition.ID)
	}

	return previous
}

func validatePartialRunDataBounds(input PartialRunInput) error {
	if len(input.Dirty) > maxDefinitionNodes {
		return fmt.Errorf("dirty node count exceeds %d", maxDefinitionNodes)
	}

	if input.Previous != nil && len(input.Previous.Nodes) > maxDefinitionNodes {
		return fmt.Errorf("previous node count exceeds %d", maxDefinitionNodes)
	}

	if len(input.Pins) > maxDefinitionNodes {
		return fmt.Errorf("pin node count exceeds %d", maxDefinitionNodes)
	}

	remaining := maxPartialRunDataBytes

	consume := func(size int) error {
		if size < 0 || size > remaining {
			return fmt.Errorf("partial run data exceeds %d bytes", maxPartialRunDataBytes)
		}

		remaining -= size

		return nil
	}
	if err := consume(128); err != nil {
		return err
	}

	for _, nodeID := range input.Dirty {
		if err := consumePartialString(string(nodeID), consume); err != nil {
			return err
		}
	}

	if err := consumePreviousPartialData(input.Previous, consume); err != nil {
		return err
	}

	return consumePartialPins(input.Pins, consume)
}

func consumePreviousPartialData(
	previous *PartialRunData,
	consume func(int) error,
) error {
	if previous == nil {
		return nil
	}

	for _, value := range []string{
		previous.RunID,
		string(previous.DefinitionID),
		string(previous.Revision),
		previous.SourcePlanFingerprint,
	} {
		if err := consumePartialString(value, consume); err != nil {
			return err
		}
	}

	for nodeID, data := range previous.Nodes {
		if err := consume(64); err != nil {
			return err
		}

		if err := consumePartialString(string(nodeID), consume); err != nil {
			return err
		}

		if err := consumePartialString(data.Route, consume); err != nil {
			return err
		}

		if err := consumePartialValues(data.Outputs, consume); err != nil {
			return err
		}
	}

	return nil
}

func consumePartialPins(pins PinData, consume func(int) error) error {
	for nodeID, outputs := range pins {
		if err := consume(32); err != nil {
			return err
		}

		if err := consumePartialString(string(nodeID), consume); err != nil {
			return err
		}

		if err := consumePartialValues(outputs, consume); err != nil {
			return err
		}
	}

	return nil
}

func consumePartialValues(values map[string]Value, consume func(int) error) error {
	if len(values) > defaultMaxJSONItems {
		return fmt.Errorf("partial node output count exceeds %d", defaultMaxJSONItems)
	}

	for name, value := range values {
		if err := consume(8); err != nil {
			return err
		}

		if err := consumePartialString(name, consume); err != nil {
			return err
		}

		if err := consume(len(value.raw)); err != nil {
			return err
		}
	}

	return nil
}

func consumePartialString(value string, consume func(int) error) error {
	if len(value) > (maxPartialRunDataBytes-2)/6 {
		return fmt.Errorf("partial run data exceeds %d bytes", maxPartialRunDataBytes)
	}

	return consume(2 + len(value)*6)
}

func validatePreviousPartialRunData(
	source *Plan,
	previous *PartialRunData,
	resolvedDirty map[NodeID]struct{},
) error {
	if previous == nil {
		return nil
	}

	if previous.RunID == "" || !validIdentifier(string(previous.Revision)) ||
		!validFingerprint(previous.SourcePlanFingerprint) {
		return errors.New("previous partial run data has invalid provenance")
	}

	if previous.DefinitionID != source.definition.ID {
		return errors.New("previous partial run data definition mismatch")
	}

	planChanged := previous.SourcePlanFingerprint != source.fingerprint
	if planChanged && len(resolvedDirty) == 0 {
		return errors.New("previous partial run data is stale without a current dirty node")
	}

	return nil
}

func (s *partialRunState) normalizePins(plan *Plan, pins PinData) error {
	for nodeID, outputs := range pins {
		index, ok := plan.nodeIndex[nodeID]
		if !ok {
			continue
		}

		node := plan.nodes[index]
		if len(node.spec.Routes) != 1 {
			return fmt.Errorf("pin node %q must have exactly one normal route", nodeID)
		}

		if err := validatePortValues(outputs, node.spec.Outputs); err != nil {
			return fmt.Errorf("pin node %q outputs: %w", nodeID, err)
		}

		s.available[index] = &partialMaterializedData{
			outputs: cloneValues(outputs),
			route:   node.spec.Routes[0],
			origin:  PartialDataPinned,
		}
	}

	return nil
}

func selectedPreviousData(
	plan *Plan,
	previous *PartialRunData,
) map[NodeID]PartialNodeData {
	selected := make(map[NodeID]PartialNodeData)
	if previous == nil {
		return selected
	}

	for nodeID, data := range previous.Nodes {
		if _, ok := plan.nodeIndex[nodeID]; !ok {
			continue
		}

		selected[nodeID] = clonePartialNodeData(data)
	}

	return selected
}

func invalidatePartialDescendants(
	previous map[NodeID]PartialNodeData,
	plan *Plan,
	index int,
) {
	invalid := plan.walk(index, plan.outgoing, func(edge planEdge) int { return edge.to })
	for candidate, remove := range invalid {
		if remove {
			delete(previous, plan.nodes[candidate].definition.ID)
		}
	}
}

func (s *partialRunState) retainReachablePrevious(
	plan *Plan,
	previous map[NodeID]PartialNodeData,
) error {
	visited := make([]bool, len(plan.nodes))
	queue := []int{plan.startIndex}

	for len(queue) > 0 {
		index := queue[0]
		queue = queue[1:]

		if visited[index] {
			continue
		}

		visited[index] = true

		materialized := s.available[index]
		if materialized == nil {
			nodeID := plan.nodes[index].definition.ID

			data, ok := previous[nodeID]
			if !ok {
				invalidatePartialDescendants(previous, plan, index)
				continue
			}

			if err := validatePartialNodeData(data, plan.nodes[index]); err != nil {
				return fmt.Errorf("previous node %q: %w", nodeID, err)
			}

			materialized = &partialMaterializedData{
				outputs: cloneValues(data.Outputs),
				route:   data.Route,
				origin:  PartialDataReused,
			}
			s.available[index] = materialized
		}

		for _, edgeIndex := range plan.outgoing[index] {
			edge := plan.edges[edgeIndex]
			if edge.route == materialized.route {
				queue = append(queue, edge.to)
			}
		}
	}

	return nil
}

func validatePartialNodeData(data PartialNodeData, node planNode) error {
	if err := validatePortValues(data.Outputs, node.spec.Outputs); err != nil {
		return fmt.Errorf("outputs: %w", err)
	}

	if !slices.Contains(node.spec.Routes, data.Route) {
		return fmt.Errorf("invalid route %q", data.Route)
	}

	return nil
}

func (s *partialRunState) requiredWorkflowInputs(plan *Plan) map[string]struct{} {
	required := make(map[string]struct{})
	visited := make([]bool, len(plan.nodes))
	queue := []int{plan.startIndex}

	for len(queue) > 0 {
		index := queue[0]
		queue = queue[1:]

		if visited[index] {
			continue
		}

		visited[index] = true

		materialized := s.available[index]
		if materialized == nil && index != plan.startIndex {
			for _, binding := range plan.nodes[index].bindings {
				switch {
				case binding.Source == BindingWorkflowInput:
					required[binding.Port] = struct{}{}
				case binding.Source == BindingNodeOutput &&
					binding.Node == plan.nodes[plan.startIndex].definition.ID &&
					s.available[plan.startIndex] == nil:
					required[binding.Port] = struct{}{}
				}
			}
		}

		for _, edgeIndex := range plan.outgoing[index] {
			edge := plan.edges[edgeIndex]
			if materialized == nil || edge.route == materialized.route {
				queue = append(queue, edge.to)
			}
		}
	}

	return required
}

func (s *partialRunState) materialized(index int) (*partialMaterializedData, bool) {
	if s == nil || index < 0 || index >= len(s.available) || s.available[index] == nil {
		return nil, false
	}

	data := s.available[index]

	return &partialMaterializedData{
		outputs: cloneValues(data.outputs),
		route:   data.route,
		origin:  data.origin,
	}, true
}

func (s *partialRunState) recordMaterialized(
	index int,
	data *partialMaterializedData,
) {
	if s == nil || data == nil || index < 0 || index >= len(s.origins) {
		return
	}

	s.origins[index] = data.origin
	s.routes[index] = data.route
	s.outputs[index] = cloneValues(data.outputs)
}

func (s *partialRunState) recordExecuted(
	index int,
	attempts int,
	outputs map[string]Value,
	route string,
	isReusable bool,
) {
	if s == nil || attempts < 1 || index < 0 || index >= len(s.origins) {
		return
	}

	s.origins[index] = PartialDataExecuted

	s.routes[index] = route
	if isReusable {
		s.outputs[index] = cloneValues(outputs)
	}
}
