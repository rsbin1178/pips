package workflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// NodePath identifies one node through zero or more composite node boundaries.
// NewNodePath and Nodes detach their slices so a path is safe to reuse.
type NodePath struct {
	nodes []NodeID
}

// NewNodePath creates an immutable node path. Compile rejects an empty path or
// an invalid or unresolved segment.
func NewNodePath(nodes ...NodeID) NodePath {
	return NodePath{nodes: slices.Clone(nodes)}
}

// Nodes returns an independent snapshot of the path segments.
func (p NodePath) Nodes() []NodeID {
	return slices.Clone(p.nodes)
}

type interruptPolicy struct {
	before   bool
	after    bool
	children map[NodeID]*interruptPolicy
}

// WithInterruptBeforeNodes pauses a Run before any addressed node is invoked.
func WithInterruptBeforeNodes(paths ...NodePath) CompileOption {
	snapshot := cloneNodePaths(paths)

	return func(config *compileConfig) error {
		config.interruptBefore = append(config.interruptBefore, cloneNodePaths(snapshot)...)

		return nil
	}
}

// WithInterruptAfterNodes pauses a Run after any addressed node succeeds.
func WithInterruptAfterNodes(paths ...NodePath) CompileOption {
	snapshot := cloneNodePaths(paths)

	return func(config *compileConfig) error {
		config.interruptAfter = append(config.interruptAfter, cloneNodePaths(snapshot)...)

		return nil
	}
}

func cloneNodePaths(paths []NodePath) []NodePath {
	cloned := make([]NodePath, len(paths))
	for index, path := range paths {
		cloned[index] = NewNodePath(path.nodes...)
	}

	return cloned
}

func normalizeInterruptPolicy(config compileConfig) (*interruptPolicy, error) {
	if len(config.interruptBefore) == 0 && len(config.interruptAfter) == 0 {
		return nil, nil
	}

	root := &interruptPolicy{children: map[NodeID]*interruptPolicy{}}
	for _, path := range config.interruptBefore {
		if err := root.add(path, true); err != nil {
			return nil, err
		}
	}

	for _, path := range config.interruptAfter {
		if err := root.add(path, false); err != nil {
			return nil, err
		}
	}

	return root, nil
}

func (p *interruptPolicy) add(path NodePath, before bool) error {
	if len(path.nodes) == 0 {
		return errors.New("static interrupt path is empty")
	}

	current := p

	for _, nodeID := range path.nodes {
		if !validIdentifier(string(nodeID)) {
			return fmt.Errorf("static interrupt path %q contains invalid node ID", pathString(path))
		}

		if current.children == nil {
			current.children = map[NodeID]*interruptPolicy{}
		}

		next, ok := current.children[nodeID]
		if !ok {
			next = &interruptPolicy{}
			current.children[nodeID] = next
		}

		current = next
	}

	if before {
		if current.before {
			return fmt.Errorf("duplicate before-node interrupt path %q", pathString(path))
		}

		current.before = true
	} else {
		if current.after {
			return fmt.Errorf("duplicate after-node interrupt path %q", pathString(path))
		}

		current.after = true
	}

	return nil
}

func pathString(path NodePath) string {
	segments := make([]string, len(path.nodes))
	for index, nodeID := range path.nodes {
		segments[index] = string(nodeID)
	}

	return strings.Join(segments, "/")
}

func isCompositeNodeType(nodeType NodeTypeKey) bool {
	switch nodeType {
	case NodeTypeSubWorkflow, NodeTypeBatch, NodeTypeLoop:
		return true
	default:
		return false
	}
}
