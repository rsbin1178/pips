package workflow

import (
	"errors"
	"fmt"
	"slices"
)

const partialRunCheckpointVersion = 1

type partialRunCheckpoint struct {
	Version               int                             `json:"version"`
	SourcePlanFingerprint string                          `json:"source_plan_fingerprint"`
	Destination           NodeID                          `json:"destination"`
	Origins               []PartialDataOrigin             `json:"origins"`
	Routes                []string                        `json:"routes"`
	Available             []partialMaterializedCheckpoint `json:"available,omitempty"`
}

type partialMaterializedCheckpoint struct {
	Index   int               `json:"index"`
	Outputs map[string]Value  `json:"outputs"`
	Route   string            `json:"route"`
	Origin  PartialDataOrigin `json:"origin"`
}

func (s *partialRunState) checkpoint(
	plan *Plan,
	execution *executionCheckpoint,
) *partialRunCheckpoint {
	if s == nil || plan == nil || plan.partialRun == nil || execution == nil {
		return nil
	}

	checkpoint := &partialRunCheckpoint{
		Version:               partialRunCheckpointVersion,
		SourcePlanFingerprint: s.sourcePlanFingerprint,
		Destination:           s.destination,
		Origins:               slices.Clone(s.origins),
		Routes:                slices.Clone(s.routes),
		Available:             make([]partialMaterializedCheckpoint, 0),
	}

	for index, data := range s.available {
		if data == nil || index >= len(execution.Nodes) ||
			!partialNodeMayStillRun(execution.Nodes[index].Status) {
			continue
		}

		checkpoint.Available = append(checkpoint.Available, partialMaterializedCheckpoint{
			Index:   index,
			Outputs: cloneValues(data.outputs),
			Route:   data.route,
			Origin:  data.origin,
		})
	}

	return checkpoint
}

func validatePartialRunCheckpoint(
	checkpoint *partialRunCheckpoint,
	plan *Plan,
	execution *executionCheckpoint,
) error {
	if plan == nil {
		return errors.New("checkpoint has nil plan")
	}

	if plan.partialRun == nil {
		if checkpoint != nil {
			return errors.New("checkpoint partial run does not match plan")
		}

		return nil
	}

	if checkpoint == nil {
		return errors.New("checkpoint is missing partial run state")
	}

	if checkpoint.Version != partialRunCheckpointVersion {
		return fmt.Errorf("unsupported partial run checkpoint version %d", checkpoint.Version)
	}

	if checkpoint.SourcePlanFingerprint != plan.partialRun.sourcePlanFingerprint ||
		checkpoint.Destination != plan.partialRun.destination {
		return errors.New("checkpoint partial run identity mismatch")
	}

	if len(checkpoint.Origins) != len(plan.nodes) ||
		len(checkpoint.Routes) != len(plan.nodes) {
		return errors.New("checkpoint partial run shape mismatch")
	}

	if err := validatePartialCheckpointDataBounds(checkpoint); err != nil {
		return err
	}

	if err := validatePartialCheckpointAccounting(checkpoint, plan, execution); err != nil {
		return err
	}

	return validatePartialCheckpointAvailable(checkpoint.Available, plan, execution)
}

func validatePartialCheckpointDataBounds(checkpoint *partialRunCheckpoint) error {
	remaining := maxPartialRunDataBytes
	consume := func(size int) error {
		if size < 0 || size > remaining {
			return fmt.Errorf(
				"checkpoint partial run data exceeds %d bytes",
				maxPartialRunDataBytes,
			)
		}

		remaining -= size

		return nil
	}

	if err := consume(128); err != nil {
		return err
	}

	return consumePartialCheckpointData(checkpoint, consume)
}

func consumePartialCheckpointData(
	checkpoint *partialRunCheckpoint,
	consume func(int) error,
) error {
	metadata := []string{
		checkpoint.SourcePlanFingerprint,
		string(checkpoint.Destination),
	}
	for index := range checkpoint.Origins {
		metadata = append(metadata, string(checkpoint.Origins[index]), checkpoint.Routes[index])
	}

	for _, value := range metadata {
		if err := consumePartialString(value, consume); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
	}

	if err := consume(len(checkpoint.Origins) * 16); err != nil {
		return err
	}

	for _, data := range checkpoint.Available {
		if err := consumePartialCheckpointAvailable(data, consume); err != nil {
			return err
		}
	}

	return nil
}

func consumePartialCheckpointAvailable(
	data partialMaterializedCheckpoint,
	consume func(int) error,
) error {
	if err := consume(32); err != nil {
		return err
	}

	for _, value := range []string{data.Route, string(data.Origin)} {
		if err := consumePartialString(value, consume); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
	}

	if err := consumePartialValues(data.Outputs, consume); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}

	return nil
}

func validatePartialCheckpointAccounting(
	checkpoint *partialRunCheckpoint,
	plan *Plan,
	execution *executionCheckpoint,
) error {
	if execution == nil || len(execution.Nodes) != len(plan.nodes) ||
		len(execution.Outputs) != len(plan.nodes) {
		return errors.New("checkpoint partial run execution shape mismatch")
	}

	for index, summary := range execution.Nodes {
		origin := checkpoint.Origins[index]
		route := checkpoint.Routes[index]
		node := plan.nodes[index]

		if !validPartialEffectiveRoute(node, route) {
			return fmt.Errorf("checkpoint partial node %q has invalid route", summary.ID)
		}

		if route != "" && !partialCheckpointRouteMatchesEdges(index, route, plan, execution) {
			return fmt.Errorf("checkpoint partial node %q route does not match edges", summary.ID)
		}

		if err := validatePartialNodeAccounting(summary, execution.Outputs[index], origin, route); err != nil {
			return fmt.Errorf("checkpoint partial node %q: %w", summary.ID, err)
		}
	}

	return nil
}

func partialCheckpointRouteMatchesEdges(
	index int,
	route string,
	plan *Plan,
	execution *executionCheckpoint,
) bool {
	for _, edgeIndex := range plan.outgoing[index] {
		expected := edgeSkipped
		if plan.edges[edgeIndex].route == route {
			expected = edgeTaken
		}

		if execution.Edges[edgeIndex] != expected {
			return false
		}
	}

	return true
}

func validPartialEffectiveRoute(node planNode, route string) bool {
	return route == "" || slices.Contains(node.spec.Routes, route) ||
		(route == RouteError && node.definition.Policy.Error == ErrorRoute)
}

func validatePartialNodeAccounting(
	summary checkpointNodeRun,
	outputs map[string]Value,
	origin PartialDataOrigin,
	route string,
) error {
	if !validPartialDataOrigin(origin) {
		return errors.New("node has invalid origin")
	}

	switch summary.Status {
	case NodeStatusSucceeded:
		return validatePartialSucceededNode(summary, outputs, origin, route)
	case NodeStatusException:
		return validatePartialExceptionNode(summary, origin, route)
	case NodeStatusFailed:
		return validatePartialFailedNode(summary, origin)
	case NodeStatusInterrupted:
		return validatePartialInterruptedNode(summary, origin)
	case NodeStatusPending, NodeStatusReady, NodeStatusSkipped:
		return validatePartialUnsettledNode(origin, route)
	default:
		return errors.New("node has unsupported partial run status")
	}
}

func validatePartialExceptionNode(
	summary checkpointNodeRun,
	origin PartialDataOrigin,
	route string,
) error {
	if summary.Attempts < 1 || origin != PartialDataExecuted || route == "" {
		return errors.New("exception node has invalid origin or route")
	}

	return nil
}

func validPartialDataOrigin(origin PartialDataOrigin) bool {
	return origin == "" || origin == PartialDataExecuted ||
		origin == PartialDataPinned || origin == PartialDataReused
}

func validatePartialSucceededNode(
	summary checkpointNodeRun,
	outputs map[string]Value,
	origin PartialDataOrigin,
	route string,
) error {
	if outputs == nil || route == "" {
		return errors.New("successful node is missing reusable data")
	}

	if summary.Attempts == 0 &&
		origin != PartialDataPinned && origin != PartialDataReused {
		return errors.New("substituted node has invalid origin")
	}

	if summary.Attempts > 0 && origin != PartialDataExecuted {
		return errors.New("executed node has invalid origin")
	}

	return nil
}

func validatePartialFailedNode(
	summary checkpointNodeRun,
	origin PartialDataOrigin,
) error {
	if summary.Attempts < 1 || origin != PartialDataExecuted {
		return errors.New("failed node has invalid origin")
	}

	return nil
}

func validatePartialInterruptedNode(
	summary checkpointNodeRun,
	origin PartialDataOrigin,
) error {
	if summary.Attempts > 0 && origin != PartialDataExecuted {
		return errors.New("interrupted node has invalid origin")
	}

	if summary.Attempts == 0 && origin != "" {
		return errors.New("unattempted interrupted node has an origin")
	}

	return nil
}

func validatePartialUnsettledNode(origin PartialDataOrigin, route string) error {
	if origin != "" || route != "" {
		return errors.New("unsettled node has effective data")
	}

	return nil
}

func validatePartialCheckpointAvailable(
	available []partialMaterializedCheckpoint,
	plan *Plan,
	execution *executionCheckpoint,
) error {
	if len(available) > len(plan.nodes) {
		return errors.New("checkpoint has too much partial run data")
	}

	seen := make(map[int]struct{}, len(available))
	for _, data := range available {
		if data.Index < 0 || data.Index >= len(plan.nodes) {
			return errors.New("checkpoint has invalid partial data index")
		}

		if _, duplicate := seen[data.Index]; duplicate {
			return errors.New("checkpoint has duplicate partial data index")
		}

		seen[data.Index] = struct{}{}

		if !partialNodeMayStillRun(execution.Nodes[data.Index].Status) {
			return errors.New("checkpoint partial data belongs to a settled node")
		}

		if data.Origin != PartialDataPinned && data.Origin != PartialDataReused {
			return errors.New("checkpoint partial data has invalid origin")
		}

		if err := validatePartialNodeData(
			PartialNodeData{Outputs: data.Outputs, Route: data.Route},
			plan.nodes[data.Index],
		); err != nil {
			return fmt.Errorf("checkpoint partial data: %w", err)
		}
	}

	return nil
}

func partialNodeMayStillRun(status NodeStatus) bool {
	return status == NodeStatusPending || status == NodeStatusReady
}

func restorePartialRunState(
	checkpoint *partialRunCheckpoint,
	plan *Plan,
	execution *executionCheckpoint,
) *partialRunState {
	if checkpoint == nil || plan == nil || plan.partialRun == nil || execution == nil {
		return nil
	}

	state := &partialRunState{
		sourcePlanFingerprint: checkpoint.SourcePlanFingerprint,
		destination:           checkpoint.Destination,
		available:             make([]*partialMaterializedData, len(plan.nodes)),
		origins:               slices.Clone(checkpoint.Origins),
		routes:                slices.Clone(checkpoint.Routes),
		outputs:               make([]map[string]Value, len(plan.nodes)),
	}

	for _, data := range checkpoint.Available {
		state.available[data.Index] = &partialMaterializedData{
			outputs: cloneValues(data.Outputs),
			route:   data.Route,
			origin:  data.Origin,
		}
	}

	for index, summary := range execution.Nodes {
		if summary.Status == NodeStatusSucceeded {
			state.outputs[index] = cloneValues(execution.Outputs[index])
		}
	}

	return state
}
