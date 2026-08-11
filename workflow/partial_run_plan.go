package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	partialRunDefinitionStrategy = "pips.workflow/partial-run-definition/v1"
	partialRunPlanStrategy       = "pips.workflow/partial-run-plan/v1"
)

type partialRunPlanMetadata struct {
	sourcePlanFingerprint       string
	legacySourcePlanFingerprint string
	destination                 NodeID
}

func derivePartialRunPlan(source *Plan, destination NodeID) (*Plan, error) {
	destinationIndex, included, err := selectPartialRunNodes(source, destination)
	if err != nil {
		return nil, err
	}

	nodes, nodeIndex, definitionNodes, oldToNew := cloneSelectedPartialNodes(source, included)
	edges, definitionEdges, incoming, outgoing := cloneSelectedPartialEdges(
		source,
		nodes,
		oldToNew,
	)

	definition := source.definition
	definition.Inputs = cloneSchemas(source.definition.Inputs)
	definition.Outputs = cloneOutputBindings(source.definition.Outputs)
	definition.Nodes = definitionNodes
	definition.Edges = definitionEdges

	definitionFingerprint, err := partialRunDefinitionFingerprint(source, destination)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint partial definition: %w", ErrCompile, err)
	}

	planFingerprint, err := partialRunPlanFingerprint(source.fingerprint, destination)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint partial plan: %w", ErrCompile, err)
	}

	legacyPlanFingerprint, err := partialRunPlanFingerprint(
		source.legacyFingerprint,
		destination,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint legacy partial plan: %w", ErrCompile, err)
	}

	plan := &Plan{
		definition:                    definition,
		definitionFingerprint:         definitionFingerprint,
		registryFingerprint:           source.registryFingerprint,
		referencedContractFingerprint: source.referencedContractFingerprint,
		fingerprint:                   planFingerprint,
		legacyFingerprint:             legacyPlanFingerprint,
		nodes:                         nodes,
		nodeIndex:                     nodeIndex,
		edges:                         edges,
		incoming:                      incoming,
		outgoing:                      outgoing,
		startIndex:                    oldToNew[source.startIndex],
		endIndex:                      oldToNew[destinationIndex],
		interruptBefore:               remapInterruptSet(source.interruptBefore, oldToNew),
		interruptAfter:                remapInterruptSet(source.interruptAfter, oldToNew),
		partialRun: &partialRunPlanMetadata{
			sourcePlanFingerprint:       source.fingerprint,
			legacySourcePlanFingerprint: source.legacyFingerprint,
			destination:                 destination,
		},
	}

	if plan.startIndex < 0 || plan.endIndex < 0 {
		return nil, errors.New("workflow: partial run plan has invalid endpoints")
	}

	return plan, nil
}

func selectPartialRunNodes(source *Plan, destination NodeID) (int, []bool, error) {
	if source == nil || !validFingerprint(source.fingerprint) || source.nodeDebug != nil ||
		source.partialRun != nil {
		return 0, nil, fmt.Errorf("%w: nil or invalid source plan", ErrCompile)
	}

	destinationIndex, ok := source.nodeIndex[destination]
	if !ok || !validIdentifier(string(destination)) {
		return 0, nil, fmt.Errorf(
			"%w: unknown partial run destination %q",
			ErrCompile,
			destination,
		)
	}

	included := source.walk(
		destinationIndex,
		source.incoming,
		func(edge planEdge) int { return edge.from },
	)
	if !included[source.startIndex] {
		return 0, nil, fmt.Errorf("%w: partial run destination is unreachable", ErrCompile)
	}

	return destinationIndex, included, nil
}

func cloneSelectedPartialNodes(
	source *Plan,
	included []bool,
) ([]planNode, map[NodeID]int, []NodeDefinition, []int) {
	oldToNew := make([]int, len(source.nodes))
	for index := range oldToNew {
		oldToNew[index] = -1
	}

	nodes := make([]planNode, 0, len(source.nodes))
	nodeIndex := make(map[NodeID]int, len(source.nodes))
	definitionNodes := make([]NodeDefinition, 0, len(source.nodes))

	for oldIndex, selected := range included {
		if !selected {
			continue
		}

		newIndex := len(nodes)
		oldToNew[oldIndex] = newIndex

		node := clonePlanNode(source.nodes[oldIndex])
		nodes = append(nodes, node)
		nodeIndex[node.definition.ID] = newIndex
		definitionNodes = append(definitionNodes, cloneNodeDefinition(node.definition))
	}

	return nodes, nodeIndex, definitionNodes, oldToNew
}

func cloneSelectedPartialEdges(
	source *Plan,
	nodes []planNode,
	oldToNew []int,
) ([]planEdge, []ControlEdge, [][]int, [][]int) {
	edges := make([]planEdge, 0, len(source.edges))
	definitionEdges := make([]ControlEdge, 0, len(source.edges))

	for _, edge := range source.edges {
		from := oldToNew[edge.from]
		to := oldToNew[edge.to]

		if from < 0 || to < 0 {
			continue
		}

		edges = append(edges, planEdge{from: from, to: to, route: edge.route})
		definitionEdges = append(definitionEdges, ControlEdge{
			From: NodeRoute{Node: nodes[from].definition.ID, Route: edge.route},
			To:   nodes[to].definition.ID,
		})
	}

	incoming := make([][]int, len(nodes))
	outgoing := make([][]int, len(nodes))

	for edgeIndex, edge := range edges {
		outgoing[edge.from] = append(outgoing[edge.from], edgeIndex)
		incoming[edge.to] = append(incoming[edge.to], edgeIndex)
	}

	return edges, definitionEdges, incoming, outgoing
}

func partialRunDefinitionFingerprint(source *Plan, destination NodeID) (string, error) {
	return partialRunFingerprint(struct {
		Strategy    string `json:"strategy"`
		Source      string `json:"source"`
		Destination NodeID `json:"destination"`
	}{
		Strategy:    partialRunDefinitionStrategy,
		Source:      source.definitionFingerprint,
		Destination: destination,
	})
}

func partialRunPlanFingerprint(sourceFingerprint string, destination NodeID) (string, error) {
	return partialRunFingerprint(struct {
		Strategy    string `json:"strategy"`
		Source      string `json:"source"`
		Destination NodeID `json:"destination"`
	}{
		Strategy:    partialRunPlanStrategy,
		Source:      sourceFingerprint,
		Destination: destination,
	})
}

func clonePlanNode(node planNode) planNode {
	node.definition = cloneNodeDefinition(node.definition)
	node.spec = cloneNodeSpec(node.spec)
	node.bindings = cloneBindings(node.bindings)

	return node
}

func cloneOutputBindings(bindings map[string]OutputBinding) map[string]OutputBinding {
	cloned := make(map[string]OutputBinding, len(bindings))
	for name, binding := range bindings {
		binding.Binding = cloneBinding(binding.Binding)
		cloned[name] = binding
	}

	return cloned
}

func remapInterruptSet(source map[int]struct{}, oldToNew []int) map[int]struct{} {
	if len(source) == 0 {
		return nil
	}

	remapped := make(map[int]struct{}, len(source))
	for oldIndex := range source {
		if oldIndex >= 0 && oldIndex < len(oldToNew) && oldToNew[oldIndex] >= 0 {
			remapped[oldToNew[oldIndex]] = struct{}{}
		}
	}

	if len(remapped) == 0 {
		return nil
	}

	return remapped
}

func partialRunFingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}
