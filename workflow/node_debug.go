package workflow

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// NodeDebugSpec is the explicit caller contract for one isolated node trial.
// Literal-bound inputs are retained by the prepared plan and are not exposed.
type NodeDebugSpec struct {
	Inputs         map[string]PortSchema
	OptionalInputs []string
	Outputs        map[string]PortSchema
	Routes         []string
}

// NodeDebugPlan is an immutable, concurrent-safe isolated node execution plan.
type NodeDebugPlan struct {
	sourcePlanFingerprint string
	target                NodePath
	spec                  NodeDebugSpec
	plan                  *Plan
	globalLimits          Limits
}

type nodeDebugPlanMetadata struct {
	fingerprint       string
	legacyFingerprint string
	target            NodePath
	spec              NodeDebugSpec
}

// NodeDebugExecution is one detached node execution record. ErrorMessage may
// contain sensitive Action error text and must not be treated as an Event.
type NodeDebugExecution struct {
	Address      NodeAddress
	NodeRun      NodeRun
	Inputs       map[string]Value
	Outputs      map[string]Value
	Route        string
	ErrorMessage string
}

// NodeDebugResult is the detached result of one isolated node trial. Inputs,
// outputs, and ErrorMessage may contain sensitive application data.
type NodeDebugResult struct {
	RunID           string
	Target          NodePath
	Status          RunStatus
	Execution       NodeDebugExecution
	InnerExecutions []NodeDebugExecution
	StartedAt       time.Time
	EndedAt         time.Time
	Interruption    *InterruptInfo
}

// PrepareNodeDebug derives an immutable isolated executable for target without
// invoking a node or mutating source.
func PrepareNodeDebug(source *Plan, target NodePath) (*NodeDebugPlan, error) {
	if source == nil || source.Fingerprint() == "" || !validFingerprint(source.Fingerprint()) {
		return nil, fmt.Errorf("%w: node debug source plan is nil or invalid", ErrCompile)
	}

	segments := target.Nodes()
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: node debug target path is empty", ErrCompile)
	}

	for _, segment := range segments {
		if !validIdentifier(string(segment)) {
			return nil, fmt.Errorf("%w: node debug target contains invalid node ID %q", ErrCompile, segment)
		}
	}

	containingPlan, targetIndex, err := resolveNodeDebugTarget(source, segments)
	if err != nil {
		return nil, err
	}

	selected := containingPlan.nodes[targetIndex]
	if err := validateNodeDebugTarget(selected.definition); err != nil {
		return nil, err
	}

	prepared, err := buildNodeDebugPlan(source, containingPlan, targetIndex, target)
	if err != nil {
		return nil, err
	}

	return prepared, nil
}

// Target returns the original static path of the selected node.
func (p *NodeDebugPlan) Target() NodePath {
	if p == nil {
		return NodePath{}
	}

	return NewNodePath(p.target.nodes...)
}

// Spec returns a detached explicit input and selected output/route contract.
func (p *NodeDebugPlan) Spec() NodeDebugSpec {
	if p == nil {
		return NodeDebugSpec{}
	}

	return cloneNodeDebugSpec(p.spec)
}

// SourcePlanFingerprint returns the exact source Plan identity.
func (p *NodeDebugPlan) SourcePlanFingerprint() string {
	if p == nil {
		return ""
	}

	return p.sourcePlanFingerprint
}

// Fingerprint returns the derived isolated executable identity.
func (p *NodeDebugPlan) Fingerprint() string {
	if p == nil || p.plan == nil {
		return ""
	}

	return p.plan.fingerprint
}

func (p *NodeDebugPlan) validate() error {
	if p == nil || p.plan == nil || p.plan.nodeDebug == nil ||
		p.sourcePlanFingerprint == "" || !validFingerprint(p.sourcePlanFingerprint) ||
		p.plan.fingerprint == "" || !validFingerprint(p.plan.fingerprint) ||
		p.plan.nodeDebug.fingerprint != p.plan.fingerprint ||
		!slices.Equal(p.target.nodes, p.plan.nodeDebug.target.nodes) {
		return errors.New("nil or invalid node debug plan")
	}

	return nil
}

func cloneNodeDebugSpec(spec NodeDebugSpec) NodeDebugSpec {
	return NodeDebugSpec{
		Inputs:         cloneSchemas(spec.Inputs),
		OptionalInputs: slices.Clone(spec.OptionalInputs),
		Outputs:        cloneSchemas(spec.Outputs),
		Routes:         slices.Clone(spec.Routes),
	}
}

func cloneNodeDebugExecution(record NodeDebugExecution) NodeDebugExecution {
	record.Address = cloneNodeAddress(record.Address)
	record.Inputs = cloneValues(record.Inputs)
	record.Outputs = cloneValues(record.Outputs)

	return record
}

func cloneNodeDebugResult(result NodeDebugResult) NodeDebugResult {
	result.Target = NewNodePath(result.Target.nodes...)
	result.Execution = cloneNodeDebugExecution(result.Execution)

	inner := make([]NodeDebugExecution, len(result.InnerExecutions))
	for index, record := range result.InnerExecutions {
		inner[index] = cloneNodeDebugExecution(record)
	}

	result.InnerExecutions = inner
	if result.Interruption != nil {
		info := cloneInterruptInfo(*result.Interruption)
		result.Interruption = &info
	}

	return result
}
