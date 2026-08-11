package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
)

const (
	referencedContractFingerprintStrategy = "pips.workflow/referenced-contract/v1"
	planFingerprintStrategy               = "pips.workflow/plan/v2"
)

type actionLookupContract struct {
	Key       ActionKey         `json:"key"`
	Version   string            `json:"version"`
	IsPresent bool              `json:"present"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Outputs   map[string]string `json:"outputs,omitempty"`
}

type actionLookupRecorder struct {
	mu       sync.Mutex
	records  map[registryKey]actionLookupContract
	isFrozen bool
}

type nodeContract struct {
	Node    NodeID            `json:"node"`
	Type    NodeTypeKey       `json:"type"`
	Version string            `json:"version"`
	Inputs  map[string]string `json:"inputs"`
	Outputs map[string]string `json:"outputs"`
	Routes  []string          `json:"routes"`
}

func newActionLookupRecorder() *actionLookupRecorder {
	return &actionLookupRecorder{
		records: make(map[registryKey]actionLookupContract),
	}
}

func (r *actionLookupRecorder) record(
	key ActionKey,
	version string,
	action Action,
	isPresent bool,
) bool {
	if r == nil {
		return true
	}

	contract := actionLookupContract{
		Key:       key,
		Version:   version,
		IsPresent: isPresent,
	}
	if isPresent {
		spec := action.Spec()
		contract.Inputs = schemaFingerprints(spec.Inputs)
		contract.Outputs = schemaFingerprints(spec.Outputs)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isFrozen {
		return false
	}

	r.records[registryKey{name: string(key), version: version}] = contract

	return true
}

func (r *actionLookupRecorder) freeze() []actionLookupContract {
	if r == nil {
		return []actionLookupContract{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.isFrozen = true

	contracts := make([]actionLookupContract, 0, len(r.records))
	for _, contract := range r.records {
		contract.Inputs = cloneStringMap(contract.Inputs)
		contract.Outputs = cloneStringMap(contract.Outputs)
		contracts = append(contracts, contract)
	}

	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].Key != contracts[j].Key {
			return contracts[i].Key < contracts[j].Key
		}

		return contracts[i].Version < contracts[j].Version
	})

	return contracts
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}

	cloned := make(map[string]string, len(values))
	maps.Copy(cloned, values)

	return cloned
}

func (p *Plan) computeFingerprint() error {
	contractFingerprint, err := p.computeReferencedContractFingerprint()
	if err != nil {
		return err
	}

	p.referencedContractFingerprint = contractFingerprint

	fingerprint, err := p.computeCurrentFingerprint()
	if err != nil {
		return err
	}

	p.fingerprint = fingerprint

	legacyFingerprint, err := p.computeLegacyFingerprint()
	if err != nil {
		return err
	}

	p.legacyFingerprint = legacyFingerprint

	return nil
}

func (p *Plan) computeReferencedContractFingerprint() (string, error) {
	nodes := make([]nodeContract, len(p.nodes))
	for index, node := range p.nodes {
		routes := slices.Clone(node.spec.Routes)
		slices.Sort(routes)
		nodes[index] = nodeContract{
			Node:    node.definition.ID,
			Type:    node.definition.Type,
			Version: node.definition.Version,
			Inputs:  schemaFingerprints(node.spec.Inputs),
			Outputs: schemaFingerprints(node.spec.Outputs),
			Routes:  routes,
		}
	}

	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Node < nodes[j].Node
	})

	children := p.currentChildIdentities(func(child *Plan) string {
		return child.referencedContractFingerprint
	})

	return fingerprintValue(
		struct {
			Strategy string                 `json:"strategy"`
			Nodes    []nodeContract         `json:"nodes"`
			Actions  []actionLookupContract `json:"actions"`
			Children []childPlanIdentity    `json:"children,omitempty"`
		}{
			Strategy: referencedContractFingerprintStrategy,
			Nodes:    nodes,
			Actions:  slices.Clone(p.actionLookups),
			Children: children,
		},
		"referenced contract",
	)
}

func (p *Plan) computeCurrentFingerprint() (string, error) {
	children := p.currentChildIdentities(func(child *Plan) string {
		return child.fingerprint
	})

	return fingerprintValue(
		struct {
			Strategy        string              `json:"strategy"`
			Definition      string              `json:"definition"`
			Contract        string              `json:"contract"`
			Children        []childPlanIdentity `json:"children,omitempty"`
			InterruptBefore []NodeID            `json:"interrupt_before,omitempty"`
			InterruptAfter  []NodeID            `json:"interrupt_after,omitempty"`
		}{
			Strategy:        planFingerprintStrategy,
			Definition:      p.definitionFingerprint,
			Contract:        p.referencedContractFingerprint,
			Children:        children,
			InterruptBefore: p.interruptNodeIDs(p.interruptBefore),
			InterruptAfter:  p.interruptNodeIDs(p.interruptAfter),
		},
		"plan",
	)
}

func (p *Plan) computeLegacyFingerprint() (string, error) {
	children := make([]childPlanIdentity, 0)

	for _, node := range p.nodes {
		composite, ok := node.executor.(compiledCompositeNode)
		if !ok {
			continue
		}

		for _, child := range composite.childPlans() {
			children = append(children, childPlanIdentity{
				Node:        node.definition.ID,
				Fingerprint: child.legacyFingerprint,
			})
		}
	}

	return fingerprintValue(
		struct {
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
		},
		"legacy plan",
	)
}

func (p *Plan) currentChildIdentities(
	fingerprint func(*Plan) string,
) []childPlanIdentity {
	children := make([]childPlanIdentity, 0)

	for _, node := range p.nodes {
		composite, ok := node.executor.(compiledCompositeNode)
		if !ok {
			continue
		}

		for index, child := range composite.childPlans() {
			children = append(children, childPlanIdentity{
				Node:        node.definition.ID,
				Index:       index,
				Fingerprint: fingerprint(child),
			})
		}
	}

	sort.Slice(children, func(i, j int) bool {
		if children[i].Node != children[j].Node {
			return children[i].Node < children[j].Node
		}

		return children[i].Index < children[j].Index
	})

	return children
}

func fingerprintValue(value any, label string) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: encode %s fingerprint: %w", ErrCompile, label, err)
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
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
