package workflow

import (
	"fmt"
	"sort"
)

func (p *Plan) validateGraph() ([]int, error) {
	for index, node := range p.nodes {
		if err := p.validateGraphNode(index, node); err != nil {
			return nil, err
		}
	}

	topological, err := p.topologicalOrder()
	if err != nil {
		return nil, err
	}

	if err := p.validateReachability(); err != nil {
		return nil, err
	}

	if err := p.validateLoopControlEdges(); err != nil {
		return nil, err
	}

	return topological, nil
}

func (p *Plan) validateGraphNode(index int, node planNode) error {
	if err := p.validateNodeDegree(index, node); err != nil {
		return err
	}

	if node.definition.Policy.Error == ErrorRoute && !p.hasOutgoingRoute(index, RouteError) {
		return compileNodeError(node.definition.ID, "route_error requires an error edge")
	}

	for _, route := range node.spec.Routes {
		if index != p.endIndex && !p.hasOutgoingRoute(index, route) {
			return compileNodeError(node.definition.ID, "route %q has no outgoing edge", route)
		}
	}

	return nil
}

func (p *Plan) validateNodeDegree(index int, node planNode) error {
	incoming := len(p.incoming[index])
	outgoing := len(p.outgoing[index])

	if err := p.validateEndpointDegree(index, node, incoming, outgoing); err != nil {
		return err
	}

	return p.validateFanIn(index, node, incoming)
}

func (p *Plan) validateEndpointDegree(
	index int,
	node planNode,
	incoming int,
	outgoing int,
) error {
	if index == p.startIndex && incoming != 0 {
		return compileNodeError(node.definition.ID, "Start must not have incoming edges")
	}

	if index != p.startIndex && incoming == 0 {
		return compileNodeError(node.definition.ID, "non-Start node requires an incoming edge")
	}

	if index == p.endIndex && outgoing != 0 {
		return compileNodeError(node.definition.ID, "End must not have outgoing edges")
	}

	if index != p.endIndex && outgoing == 0 {
		return compileNodeError(node.definition.ID, "non-End node requires an outgoing edge")
	}

	return nil
}

func (p *Plan) validateFanIn(index int, node planNode, incoming int) error {
	if node.isMerge && incoming < 2 {
		return compileNodeError(node.definition.ID, "Merge requires at least two incoming edges")
	}

	allowsLoopEndFanIn := p.loop != nil && index == p.endIndex
	if !node.isMerge && !allowsLoopEndFanIn && incoming > 1 {
		return compileNodeError(node.definition.ID, "multiple incoming edges require Merge")
	}

	return nil
}

func (p *Plan) hasOutgoingRoute(nodeIndex int, route string) bool {
	for _, edgeIndex := range p.outgoing[nodeIndex] {
		if p.edges[edgeIndex].route == route {
			return true
		}
	}

	return false
}

func (p *Plan) topologicalOrder() ([]int, error) {
	indegree := make([]int, len(p.nodes))

	ready := make([]int, 0, len(p.nodes))
	for index := range p.nodes {
		indegree[index] = len(p.incoming[index])
		if indegree[index] == 0 {
			ready = append(ready, index)
		}
	}

	order := make([]int, 0, len(p.nodes))
	for len(ready) > 0 {
		sort.Ints(ready)
		index := ready[0]
		ready = ready[1:]

		order = append(order, index)

		for _, edgeIndex := range p.outgoing[index] {
			to := p.edges[edgeIndex].to

			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
			}
		}
	}

	if len(order) != len(p.nodes) {
		return nil, fmt.Errorf("%w: root graph must be a DAG", ErrCompile)
	}

	return order, nil
}

func (p *Plan) validateReachability() error {
	fromStart := p.walk(p.startIndex, p.outgoing, func(edge planEdge) int { return edge.to })
	toEnd := p.walk(p.endIndex, p.incoming, func(edge planEdge) int { return edge.from })

	for index, node := range p.nodes {
		if !fromStart[index] || !toEnd[index] {
			return compileNodeError(node.definition.ID, "node is not on a Start-to-End path")
		}
	}

	return nil
}

func (p *Plan) walk(start int, adjacency [][]int, next func(planEdge) int) []bool {
	visited := make([]bool, len(p.nodes))
	queue := []int{start}
	visited[start] = true

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, edgeIndex := range adjacency[current] {
			candidate := next(p.edges[edgeIndex])
			if !visited[candidate] {
				visited[candidate] = true
				queue = append(queue, candidate)
			}
		}
	}

	return visited
}
