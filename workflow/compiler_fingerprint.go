package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

func (p *Plan) computeFingerprint() error {
	children := make([]childPlanIdentity, 0)

	for _, node := range p.nodes {
		composite, ok := node.executor.(compiledCompositeNode)
		if !ok {
			continue
		}

		for _, child := range composite.childPlans() {
			children = append(children, childPlanIdentity{
				Node:        node.definition.ID,
				Fingerprint: child.Fingerprint(),
			})
		}
	}

	data, err := json.Marshal(struct {
		Definition      string              `json:"definition"`
		Registry        string              `json:"registry"`
		Children        []childPlanIdentity `json:"children,omitempty"`
		InterruptBefore []NodeID            `json:"interrupt_before,omitempty"`
		InterruptAfter  []NodeID            `json:"interrupt_after,omitempty"`
	}{
		Definition:      p.definitionFingerprint,
		Registry:        p.registryFingerprint,
		Children:        children,
		InterruptBefore: p.interruptNodeIDs(p.interruptBefore),
		InterruptAfter:  p.interruptNodeIDs(p.interruptAfter),
	})
	if err != nil {
		return fmt.Errorf("%w: encode plan fingerprint: %w", ErrCompile, err)
	}

	digest := sha256.Sum256(data)
	p.fingerprint = hex.EncodeToString(digest[:])

	return nil
}

func (p *Plan) interruptNodeIDs(indexes map[int]struct{}) []NodeID {
	if len(indexes) == 0 {
		return nil
	}

	nodeIDs := make([]NodeID, 0, len(indexes))
	for index := range indexes {
		nodeIDs = append(nodeIDs, p.nodes[index].definition.ID)
	}

	slices.Sort(nodeIDs)

	return nodeIDs
}
