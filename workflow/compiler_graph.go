package workflow

import (
	"fmt"
	"sort"
)

func (p *Plan) validateGraph() ([]int, error) {
	var collector compileErrorCollector

	for index, node := range p.nodes {
		collector.add(
			p.validateGraphNode(index, node),
			newNodeCompileIssue(
				CompileIssueInvalidGraph,
				NewNodePath(node.definition.ID),
				fmt.Sprintf("node %q: invalid graph contract", node.definition.ID),
			),
		)
	}

	if err := collector.err(); err != nil {
		return nil, err
	}

	topological, err := p.topologicalOrder()
	if err != nil {
		return nil, p.cycleCompileError(err)
	}

	var postOrderCollector compileErrorCollector
	postOrderCollector.add(p.validateReachability(), CompileIssue{})
	postOrderCollector.add(p.validateLoopControlEdges(), CompileIssue{})

	if err := postOrderCollector.err(); err != nil {
		return nil, err
	}

	return topological, nil
}

func (p *Plan) validateGraphNode(index int, node planNode) error {
	var collector compileErrorCollector

	collector.add(
		p.validateNodeDegree(index, node),
		newNodeCompileIssue(
			CompileIssueInvalidGraph,
			NewNodePath(node.definition.ID),
			fmt.Sprintf("node %q: invalid degree", node.definition.ID),
		),
	)

	if node.definition.Policy.Error == ErrorRoute && !p.hasOutgoingRoute(index, RouteError) {
		collector.add(
			compileNodeFailure(
				CompileIssueInvalidGraph,
				node.definition.ID,
				nil,
				"route_error requires an error edge",
			),
			CompileIssue{},
		)
	}

	for _, route := range node.spec.Routes {
		if index != p.endIndex && !p.hasOutgoingRoute(index, route) {
			collector.add(
				compileNodeFailure(
					CompileIssueInvalidGraph,
					node.definition.ID,
					nil,
					"route %q has no outgoing edge",
					route,
				),
				CompileIssue{},
			)
		}
	}

	return collector.err()
}

func (p *Plan) validateNodeDegree(index int, node planNode) error {
	incoming := len(p.incoming[index])
	outgoing := len(p.outgoing[index])

	var collector compileErrorCollector

	collector.add(
		p.validateEndpointDegree(index, node, incoming, outgoing),
		newNodeCompileIssue(
			CompileIssueInvalidGraph,
			NewNodePath(node.definition.ID),
			fmt.Sprintf("node %q: invalid endpoint degree", node.definition.ID),
		),
	)
	collector.add(
		p.validateFanIn(index, node, incoming),
		newNodeCompileIssue(
			CompileIssueInvalidGraph,
			NewNodePath(node.definition.ID),
			fmt.Sprintf("node %q: invalid fan-in", node.definition.ID),
		),
	)

	return collector.err()
}

func (p *Plan) validateEndpointDegree(
	index int,
	node planNode,
	incoming int,
	outgoing int,
) error {
	if index == p.startIndex && incoming != 0 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"Start must not have incoming edges",
		)
	}

	if index != p.startIndex && incoming == 0 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"non-Start node requires an incoming edge",
		)
	}

	if index == p.endIndex && outgoing != 0 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"End must not have outgoing edges",
		)
	}

	if index != p.endIndex && outgoing == 0 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"non-End node requires an outgoing edge",
		)
	}

	return nil
}

func (p *Plan) validateFanIn(index int, node planNode, incoming int) error {
	if node.isMerge && incoming < 2 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"Merge requires at least two incoming edges",
		)
	}

	allowsLoopEndFanIn := p.loop != nil && index == p.endIndex
	if !node.isMerge && !allowsLoopEndFanIn && incoming > 1 {
		return compileNodeFailure(
			CompileIssueInvalidGraph,
			node.definition.ID,
			nil,
			"multiple incoming edges require Merge",
		)
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
		return nil, compileDefinitionFailure(
			CompileIssueInvalidGraph,
			nil,
			"root graph must be a DAG",
		)
	}

	return order, nil
}

func (p *Plan) validateReachability() error {
	var collector compileErrorCollector

	fromStart := p.walk(p.startIndex, p.outgoing, func(edge planEdge) int { return edge.to })
	toEnd := p.walk(p.endIndex, p.incoming, func(edge planEdge) int { return edge.from })

	for index, node := range p.nodes {
		if !fromStart[index] || !toEnd[index] {
			collector.add(
				compileNodeFailure(
					CompileIssueInvalidGraph,
					node.definition.ID,
					nil,
					"node is not on a Start-to-End path",
				),
				CompileIssue{},
			)
		}
	}

	return collector.err()
}

func (p *Plan) cycleCompileError(cause error) error {
	var collector compileErrorCollector

	for _, edge := range p.edges {
		if !p.reachable(edge.to, edge.from) {
			continue
		}

		definition := ControlEdge{
			From: NodeRoute{
				Node:  p.nodes[edge.from].definition.ID,
				Route: edge.route,
			},
			To: p.nodes[edge.to].definition.ID,
		}
		collector.add(
			compileControlPathFailure(
				CompileIssueInvalidGraph,
				definition,
				nil,
				"participates in a cycle; root graph must be a DAG",
			),
			CompileIssue{},
		)
	}

	if err := collector.err(); err != nil {
		return err
	}

	return compileErrorFromFailure(
		cause,
		newDefinitionCompileIssue(CompileIssueInvalidGraph, "root graph must be a DAG"),
	)
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
