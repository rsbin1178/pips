package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const (
	checkpointVersion       = 3
	legacyCheckpointVersion = 2
)

type checkpointIdentityMode uint8

const (
	checkpointIdentityLegacy checkpointIdentityMode = iota + 1
	checkpointIdentityCurrent
)

type checkpointFingerprint struct {
	value     string
	isPresent bool
}

func newCheckpointFingerprint(value string) checkpointFingerprint {
	return checkpointFingerprint{value: value, isPresent: true}
}

func (f checkpointFingerprint) IsZero() bool {
	return !f.isPresent
}

func (f checkpointFingerprint) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.value)
}

func (f *checkpointFingerprint) UnmarshalJSON(data []byte) error {
	if f == nil {
		return errors.New("nil checkpoint fingerprint")
	}

	if string(data) == "null" {
		return errors.New("checkpoint fingerprint must be a string")
	}

	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	f.value = value
	f.isPresent = true

	return nil
}

var checkpointJSONLimits = jsonLimits{
	maxBytes: 16 << 20,
	maxDepth: 128,
	maxItems: 1_000_000,
}

type workflowCheckpoint struct {
	Version               int                   `json:"version"`
	RunID                 string                `json:"run_id"`
	DefinitionID          DefinitionID          `json:"definition_id"`
	Revision              Revision              `json:"revision"`
	DefinitionFingerprint string                `json:"definition_fingerprint"`
	RegistryFingerprint   checkpointFingerprint `json:"registry_fingerprint,omitzero"`
	ContractFingerprint   checkpointFingerprint `json:"contract_fingerprint,omitzero"`
	PlanFingerprint       string                `json:"plan_fingerprint"`
	StartedAt             time.Time             `json:"started_at"`
	TotalSteps            int64                 `json:"total_steps"`
	HandledFailure        bool                  `json:"handled_failure"`
	Execution             executionCheckpoint   `json:"execution"`
	Interruption          InterruptInfo         `json:"interruption"`
	NodeDebug             *nodeDebugCheckpoint  `json:"node_debug,omitempty"`
	PartialRun            *partialRunCheckpoint `json:"partial_run,omitempty"`
}

type executionCheckpoint struct {
	PlanFingerprint string                 `json:"plan_fingerprint"`
	StartedAt       time.Time              `json:"started_at"`
	Scope           []ScopeFrame           `json:"scope"`
	Input           map[string]Value       `json:"input"`
	Edges           []edgeState            `json:"edges"`
	Nodes           []checkpointNodeRun    `json:"nodes"`
	Outputs         []map[string]Value     `json:"outputs"`
	FailureData     []nodeFailureData      `json:"failure_data"`
	Ready           []int                  `json:"ready"`
	Done            int                    `json:"done"`
	Steps           int64                  `json:"steps"`
	BeforePassed    []int                  `json:"before_passed,omitempty"`
	Paused          []pausedNodeCheckpoint `json:"paused,omitempty"`
}

type checkpointNodeRun struct {
	ID        NodeID      `json:"id"`
	Type      NodeTypeKey `json:"type"`
	Status    NodeStatus  `json:"status"`
	Attempts  int         `json:"attempts"`
	Failure   FailureKind `json:"failure,omitempty"`
	StartedAt time.Time   `json:"started_at,omitzero"`
	EndedAt   time.Time   `json:"ended_at,omitzero"`
}

type pausedNodeCheckpoint struct {
	Index     int                          `json:"index"`
	Inputs    map[string]Value             `json:"inputs"`
	Attempts  int                          `json:"attempts"`
	StartedAt time.Time                    `json:"started_at,omitzero"`
	Child     *executionCheckpoint         `json:"child,omitempty"`
	Batch     *batchCheckpoint             `json:"batch,omitempty"`
	Loop      *loopCheckpoint              `json:"loop,omitempty"`
	Dynamic   []dynamicInterruptCheckpoint `json:"dynamic,omitempty"`
	Local     *dynamicLocalCheckpoint      `json:"local,omitempty"`
	Rerun     bool                         `json:"rerun,omitempty"`
}

type batchItemStatus string

const (
	batchItemPending     batchItemStatus = "pending"
	batchItemSucceeded   batchItemStatus = "succeeded"
	batchItemFailed      batchItemStatus = "failed"
	batchItemInterrupted batchItemStatus = "interrupted"
)

type batchCheckpoint struct {
	Items []batchItemCheckpoint `json:"items"`
}

type batchItemCheckpoint struct {
	Index   int                          `json:"index"`
	Status  batchItemStatus              `json:"status"`
	Result  *Value                       `json:"result,omitempty"`
	Child   *executionCheckpoint         `json:"child,omitempty"`
	Dynamic []dynamicInterruptCheckpoint `json:"dynamic,omitempty"`
	Info    InterruptInfo                `json:"info"`
}

type loopCheckpoint struct {
	Iterations     int                          `json:"iterations"`
	Index          int                          `json:"index"`
	Committed      map[string]Value             `json:"committed"`
	Aggregated     map[string][]Value           `json:"aggregated"`
	IterationState map[string]Value             `json:"iteration_state"`
	ShouldBreak    bool                         `json:"should_break"`
	Child          *executionCheckpoint         `json:"child"`
	Dynamic        []dynamicInterruptCheckpoint `json:"dynamic,omitempty"`
	Info           InterruptInfo                `json:"info"`
}

// Resume validates and restores one interrupted Run from its CheckpointStore.
// Dynamic targets are validated as one set before any node is invoked.
func (r *Runner) Resume(
	ctx context.Context,
	plan *Plan,
	runID string,
	targets []ResumeTarget,
) (RunResult, error) {
	result, _, _, err := r.resumeExecution(ctx, plan, runID, targets, planLimits(plan))

	return result, err
}

func (r *Runner) resumeExecution(
	ctx context.Context,
	plan *Plan,
	runID string,
	targets []ResumeTarget,
	limits Limits,
) (RunResult, *nodeDebugCollector, *partialRunState, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return RunResult{}, nil, nil, errors.New("workflow: nil or invalid runner")
	}

	if plan == nil || plan.Fingerprint() == "" {
		return RunResult{}, nil, nil, errors.New("workflow: nil or invalid plan")
	}

	checkpoint, err := r.loadResumeCheckpoint(ctx, plan, runID)
	if err != nil {
		return RunResult{}, nil, nil, err
	}

	resumeTargets, err := validateResumeTargets(targets, checkpoint.Interruption)
	if err != nil {
		return RunResult{}, nil, nil, fmt.Errorf("%w: resume targets: %w", ErrRun, err)
	}

	state := newRunState(checkpoint.RunID, limits)
	state.steps.Store(checkpoint.TotalSteps)
	state.hasHandledFailure.Store(checkpoint.HandledFailure)
	state.resumeTargets = resumeTargets

	if checkpoint.NodeDebug != nil {
		state.nodeDebug = restoreNodeDebugCollector(checkpoint.NodeDebug)
	}

	state.partialRun = restorePartialRunState(
		checkpoint.PartialRun,
		plan,
		&checkpoint.Execution,
	)

	execution := restoreExecution(
		r,
		plan,
		state,
		&checkpoint.Execution,
		nil,
	)
	execution.resumed = true

	result, runErr := execution.run(ctx)

	return result, state.nodeDebug, state.partialRun, runErr
}

func (r *Runner) loadResumeCheckpoint(
	ctx context.Context,
	plan *Plan,
	runID string,
) (*workflowCheckpoint, error) {
	if isNilInterface(r.checkpointStore) {
		return nil, fmt.Errorf("%w: checkpoint store is required", ErrRun)
	}

	if runID == "" {
		return nil, fmt.Errorf("%w: empty run id", ErrRun)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, found, err := r.checkpointStore.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: load checkpoint: %w", ErrRun, err)
	}

	if !found {
		return nil, fmt.Errorf("%w: checkpoint for run %q was not found", ErrRun, runID)
	}

	checkpoint, err := decodeWorkflowCheckpoint(slices.Clone(data), plan, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: load checkpoint: %w", ErrRun, err)
	}

	return checkpoint, nil
}

func planLimits(plan *Plan) Limits {
	if plan == nil {
		return Limits{}
	}

	return plan.definition.Limits
}

func validateResumeTargets(
	targets []ResumeTarget,
	info InterruptInfo,
) (map[string]ResumeTarget, error) {
	if len(info.Contexts) == 0 {
		if len(targets) != 0 {
			return nil, errors.New("checkpoint has no dynamic interruptions")
		}

		return map[string]ResumeTarget{}, nil
	}

	if len(targets) == 0 {
		return nil, errors.New("at least one dynamic interruption target is required")
	}

	outstanding := make(map[string]struct{}, len(info.Contexts))
	for _, interruption := range info.Contexts {
		if _, duplicate := outstanding[interruption.ID]; duplicate {
			return nil, errors.New("checkpoint has duplicate dynamic interruption IDs")
		}

		outstanding[interruption.ID] = struct{}{}
	}

	normalized := make(map[string]ResumeTarget, len(targets))
	for _, target := range targets {
		if target.InterruptID == "" {
			return nil, errors.New("empty dynamic interruption ID")
		}

		if _, ok := outstanding[target.InterruptID]; !ok {
			return nil, fmt.Errorf("unknown dynamic interruption ID %q", target.InterruptID)
		}

		if _, duplicate := normalized[target.InterruptID]; duplicate {
			return nil, fmt.Errorf("duplicate dynamic interruption ID %q", target.InterruptID)
		}

		normalized[target.InterruptID] = ResumeTarget{
			InterruptID: target.InterruptID,
			Data:        target.Data,
		}
	}

	return normalized, nil
}

func encodeWorkflowCheckpoint(checkpoint workflowCheckpoint) ([]byte, error) {
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint: %w", err)
	}

	return data, nil
}

func decodeWorkflowCheckpoint(
	data []byte,
	plan *Plan,
	runID string,
) (*workflowCheckpoint, error) {
	var checkpoint workflowCheckpoint
	if err := decodeJSON(data, &checkpoint, checkpointJSONLimits, true); err != nil {
		return nil, fmt.Errorf("decode checkpoint: %w", err)
	}

	mode, err := validateWorkflowCheckpointMetadata(&checkpoint, plan, runID)
	if err != nil {
		return nil, err
	}

	if err := validateInterruptInfo(checkpoint.Interruption); err != nil {
		return nil, err
	}

	if err := validateInterruptAddressesForPlan(checkpoint.Interruption, plan); err != nil {
		return nil, err
	}

	if err := validateExecutionCheckpoint(&checkpoint.Execution, plan, mode); err != nil {
		return nil, err
	}

	if !checkpoint.HandledFailure &&
		executionCheckpointHasHandledFailure(&checkpoint.Execution) {
		return nil, errors.New("checkpoint handled failure accounting mismatch")
	}

	if err := validatePartialRunCheckpoint(
		checkpoint.PartialRun,
		plan,
		&checkpoint.Execution,
		mode,
	); err != nil {
		return nil, err
	}

	if err := validateCheckpointDynamicAlignment(
		checkpoint.Interruption,
		&checkpoint.Execution,
	); err != nil {
		return nil, err
	}

	if err := validateNodeDebugCheckpoint(checkpoint.NodeDebug, plan, mode); err != nil {
		return nil, err
	}

	return &checkpoint, nil
}

func validateWorkflowCheckpointMetadata(
	checkpoint *workflowCheckpoint,
	plan *Plan,
	runID string,
) (checkpointIdentityMode, error) {
	mode, err := checkpointMode(checkpoint.Version)
	if err != nil {
		return 0, err
	}

	if checkpoint.RunID != runID || checkpoint.RunID == "" {
		return 0, errors.New("checkpoint run identity mismatch")
	}

	if !workflowCheckpointMatchesPlan(checkpoint, plan, mode) {
		return 0, errors.New("checkpoint plan identity mismatch")
	}

	if checkpoint.StartedAt.IsZero() || checkpoint.TotalSteps < 0 {
		return 0, errors.New("checkpoint has invalid run accounting")
	}

	if interruptInfoEmpty(checkpoint.Interruption) {
		return 0, errors.New("checkpoint has no interruption")
	}

	return mode, nil
}

func checkpointMode(version int) (checkpointIdentityMode, error) {
	switch version {
	case legacyCheckpointVersion:
		return checkpointIdentityLegacy, nil
	case checkpointVersion:
		return checkpointIdentityCurrent, nil
	default:
		return 0, fmt.Errorf("unsupported checkpoint version %d", version)
	}
}

func workflowCheckpointMatchesPlan(
	checkpoint *workflowCheckpoint,
	plan *Plan,
	mode checkpointIdentityMode,
) bool {
	if !workflowCheckpointMatchesDefinition(checkpoint, plan) {
		return false
	}

	switch mode {
	case checkpointIdentityLegacy:
		return workflowCheckpointMatchesLegacyPlan(checkpoint, plan)
	case checkpointIdentityCurrent:
		return workflowCheckpointMatchesCurrentPlan(checkpoint, plan)
	default:
		return false
	}
}

func workflowCheckpointMatchesDefinition(
	checkpoint *workflowCheckpoint,
	plan *Plan,
) bool {
	return checkpoint != nil && plan != nil &&
		checkpoint.DefinitionID == plan.definition.ID &&
		checkpoint.Revision == plan.definition.Revision &&
		checkpoint.DefinitionFingerprint == plan.definitionFingerprint
}

func workflowCheckpointMatchesLegacyPlan(
	checkpoint *workflowCheckpoint,
	plan *Plan,
) bool {
	return checkpoint.RegistryFingerprint.isPresent &&
		!checkpoint.ContractFingerprint.isPresent &&
		checkpoint.RegistryFingerprint.value == plan.registryFingerprint &&
		checkpoint.PlanFingerprint == plan.legacyFingerprint
}

func workflowCheckpointMatchesCurrentPlan(
	checkpoint *workflowCheckpoint,
	plan *Plan,
) bool {
	return !checkpoint.RegistryFingerprint.isPresent &&
		checkpoint.ContractFingerprint.isPresent &&
		checkpoint.ContractFingerprint.value == plan.referencedContractFingerprint &&
		checkpoint.PlanFingerprint == plan.fingerprint
}

func validateInterruptAddressesForPlan(info InterruptInfo, plan *Plan) error {
	for _, context := range info.Contexts {
		if !planContainsAddress(plan, context.Address) {
			return errors.New("checkpoint dynamic interruption has unknown address")
		}
	}

	for _, addresses := range [][]NodeAddress{
		info.BeforeNodes, info.AfterNodes, info.RerunNodes,
	} {
		for _, address := range addresses {
			if !planContainsAddress(plan, address) {
				return errors.New("checkpoint interruption has unknown address")
			}
		}
	}

	return nil
}

func planContainsAddress(plan *Plan, address NodeAddress) bool {
	current := plan
	for _, frame := range address.Scope {
		index, ok := current.nodeIndex[frame.NodeID]
		if !ok {
			return false
		}

		node := current.nodes[index]

		switch frame.Kind {
		case ScopeSubWorkflow:
			if node.definition.Type != NodeTypeSubWorkflow {
				return false
			}
		case ScopeBatchItem:
			if node.definition.Type != NodeTypeBatch {
				return false
			}
		case ScopeLoopIteration:
			if node.definition.Type != NodeTypeLoop {
				return false
			}
		default:
			return false
		}

		children := childPlans(node.executor)
		if len(children) != 1 {
			return false
		}

		current = children[0]
	}

	_, ok := current.nodeIndex[address.NodeID]

	return ok
}

func validateCheckpointDynamicAlignment(
	info InterruptInfo,
	execution *executionCheckpoint,
) error {
	stored := make(map[string]dynamicInterruptCheckpoint)

	for _, paused := range execution.Paused {
		for _, interruption := range paused.Dynamic {
			if _, duplicate := stored[interruption.ID]; duplicate {
				return errors.New("checkpoint duplicates an outstanding dynamic interruption")
			}

			stored[interruption.ID] = interruption
		}
	}

	if len(stored) != len(info.Contexts) {
		return errors.New("checkpoint dynamic interruption frontier mismatch")
	}

	for _, context := range info.Contexts {
		interruption, ok := stored[context.ID]
		if !ok || !nodeAddressEqual(interruption.Address, context.Address) ||
			!interruption.Info.Equal(context.Info) {
			return errors.New("checkpoint dynamic interruption context mismatch")
		}
	}

	return nil
}

func validateInterruptInfo(info InterruptInfo) error {
	for _, context := range info.Contexts {
		if context.ID == "" || !context.Info.IsValid() {
			return errors.New("checkpoint has invalid dynamic interruption")
		}

		if err := validateNodeAddress(context.Address); err != nil {
			return err
		}
	}

	for _, addresses := range [][]NodeAddress{
		info.BeforeNodes, info.AfterNodes, info.RerunNodes,
	} {
		for _, address := range addresses {
			if err := validateNodeAddress(address); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateNodeAddress(address NodeAddress) error {
	if !validIdentifier(string(address.NodeID)) {
		return errors.New("checkpoint has invalid node address")
	}

	for _, frame := range address.Scope {
		if err := validateScopeFrame(frame); err != nil {
			return fmt.Errorf("checkpoint node address: %w", err)
		}
	}

	return nil
}

func validateExecutionCheckpoint(
	checkpoint *executionCheckpoint,
	plan *Plan,
	mode checkpointIdentityMode,
) error {
	if err := validateExecutionCheckpointHeader(checkpoint, plan, mode); err != nil {
		return err
	}

	ready, err := validateCheckpointReady(checkpoint, plan)
	if err != nil {
		return err
	}

	if err := validateCheckpointNodeStates(checkpoint, plan, ready); err != nil {
		return err
	}

	paused, err := validateCheckpointPausedNodes(checkpoint, plan, mode)
	if err != nil {
		return err
	}

	if err := validateCheckpointFrontiers(checkpoint, ready, paused); err != nil {
		return err
	}

	if err := validateCheckpointBeforePassed(checkpoint, plan); err != nil {
		return err
	}

	return validateCheckpointDone(checkpoint)
}

func validateExecutionCheckpointHeader(
	checkpoint *executionCheckpoint,
	plan *Plan,
	mode checkpointIdentityMode,
) error {
	if checkpoint == nil || checkpoint.PlanFingerprint != planCheckpointFingerprint(plan, mode) {
		return errors.New("checkpoint execution plan mismatch")
	}

	if checkpoint.StartedAt.IsZero() {
		return errors.New("checkpoint execution has invalid start time")
	}

	if err := validateCheckpointScope(checkpoint.Scope); err != nil {
		return err
	}

	if err := validatePlanInputValues(checkpoint.Input, plan); err != nil {
		return fmt.Errorf("checkpoint inputs: %w", err)
	}

	if !executionCheckpointShapeMatches(checkpoint, plan) {
		return errors.New("checkpoint execution shape mismatch")
	}

	for _, state := range checkpoint.Edges {
		if state > edgeSkipped {
			return errors.New("checkpoint has invalid edge state")
		}
	}

	return nil
}

func validateCheckpointScope(scope []ScopeFrame) error {
	for _, frame := range scope {
		if err := validateScopeFrame(frame); err != nil {
			return fmt.Errorf("checkpoint scope: %w", err)
		}
	}

	return nil
}

func executionCheckpointShapeMatches(checkpoint *executionCheckpoint, plan *Plan) bool {
	return len(checkpoint.Edges) == len(plan.edges) &&
		len(checkpoint.Nodes) == len(plan.nodes) &&
		len(checkpoint.Outputs) == len(plan.nodes) &&
		len(checkpoint.FailureData) == len(plan.nodes) &&
		checkpoint.Done >= 0 && checkpoint.Done <= len(plan.nodes) && checkpoint.Steps >= 0
}

func validateCheckpointReady(
	checkpoint *executionCheckpoint,
	plan *Plan,
) (map[int]struct{}, error) {
	ready := make(map[int]struct{}, len(checkpoint.Ready))
	for _, index := range checkpoint.Ready {
		if index < 0 || index >= len(plan.nodes) {
			return nil, errors.New("checkpoint has invalid ready node")
		}

		if _, duplicate := ready[index]; duplicate {
			return nil, errors.New("checkpoint has duplicate ready node")
		}

		ready[index] = struct{}{}
	}

	return ready, nil
}

func validateCheckpointNodeStates(
	checkpoint *executionCheckpoint,
	plan *Plan,
	ready map[int]struct{},
) error {
	for index, node := range checkpoint.Nodes {
		if err := validateCheckpointNodeState(checkpoint, plan, ready, index, node); err != nil {
			return err
		}
	}

	return nil
}

func validateCheckpointNodeState(
	checkpoint *executionCheckpoint,
	plan *Plan,
	ready map[int]struct{},
	index int,
	node checkpointNodeRun,
) error {
	if node.ID != plan.nodes[index].definition.ID ||
		node.Type != plan.nodes[index].definition.Type ||
		node.Attempts < 0 {
		return errors.New("checkpoint node identity mismatch")
	}

	switch node.Status {
	case NodeStatusPending, NodeStatusSucceeded, NodeStatusException, NodeStatusFailed,
		NodeStatusSkipped, NodeStatusInterrupted:
	case NodeStatusReady:
		if _, ok := ready[index]; !ok {
			return errors.New("checkpoint ready node is missing from frontier")
		}
	case NodeStatusRunning:
		return errors.New("checkpoint contains a running node")
	default:
		return errors.New("checkpoint has invalid node status")
	}

	if err := validateCheckpointNodeFailure(
		checkpoint,
		plan,
		index,
		node,
	); err != nil {
		return err
	}

	if checkpoint.Outputs[index] == nil {
		return nil
	}

	if err := validatePortValues(
		checkpoint.Outputs[index],
		plan.nodes[index].spec.Outputs,
	); err != nil {
		return fmt.Errorf("checkpoint node %q outputs: %w", node.ID, err)
	}

	return nil
}

func validateCheckpointNodeFailure(
	checkpoint *executionCheckpoint,
	plan *Plan,
	index int,
	node checkpointNodeRun,
) error {
	data := checkpoint.FailureData[index]
	if node.Status != NodeStatusException {
		if data != nil {
			return fmt.Errorf("checkpoint node %q has stale failure data", node.ID)
		}

		return nil
	}

	if !validFailureKind(node.Failure) {
		return fmt.Errorf("checkpoint node %q has invalid exception kind", node.ID)
	}

	if err := validateNodeFailureData(data, node.Failure); err != nil {
		return fmt.Errorf("checkpoint node %q failure data: %w", node.ID, err)
	}

	definition := plan.nodes[index].definition
	switch definition.Policy.Error {
	case ErrorRoute:
		if checkpoint.Outputs[index] != nil ||
			!checkpointNodeSelectedRoute(checkpoint, plan, index, RouteError) {
			return fmt.Errorf("checkpoint node %q has invalid error-route exception", node.ID)
		}
	case ErrorContinueWithDefault:
		if !nodeValuesEqual(checkpoint.Outputs[index], definition.Policy.DefaultOutputs) ||
			!checkpointNodeSelectedRoute(checkpoint, plan, index, RouteSuccess) {
			return fmt.Errorf("checkpoint node %q has invalid default exception", node.ID)
		}
	default:
		return fmt.Errorf("checkpoint node %q has exception without handling", node.ID)
	}

	return nil
}

func checkpointNodeSelectedRoute(
	checkpoint *executionCheckpoint,
	plan *Plan,
	index int,
	route string,
) bool {
	for _, edgeIndex := range plan.outgoing[index] {
		expected := edgeSkipped
		if plan.edges[edgeIndex].route == route {
			expected = edgeTaken
		}

		if checkpoint.Edges[edgeIndex] != expected {
			return false
		}
	}

	return true
}

func nodeValuesEqual(left, right map[string]Value) bool {
	if len(left) != len(right) || (left == nil) != (right == nil) {
		return false
	}

	for name, leftValue := range left {
		rightValue, ok := right[name]
		if !ok || !leftValue.Equal(rightValue) {
			return false
		}
	}

	return true
}

func executionCheckpointHasHandledFailure(checkpoint *executionCheckpoint) bool {
	if checkpoint == nil {
		return false
	}

	for _, node := range checkpoint.Nodes {
		if node.Status == NodeStatusException {
			return true
		}
	}

	for _, paused := range checkpoint.Paused {
		if executionCheckpointHasHandledFailure(paused.Child) ||
			batchCheckpointHasHandledFailure(paused.Batch) ||
			executionCheckpointHasHandledFailure(loopCheckpointChild(paused.Loop)) {
			return true
		}
	}

	return false
}

func batchCheckpointHasHandledFailure(checkpoint *batchCheckpoint) bool {
	if checkpoint == nil {
		return false
	}

	for _, item := range checkpoint.Items {
		if item.Status == batchItemFailed ||
			executionCheckpointHasHandledFailure(item.Child) {
			return true
		}
	}

	return false
}

func loopCheckpointChild(checkpoint *loopCheckpoint) *executionCheckpoint {
	if checkpoint == nil {
		return nil
	}

	return checkpoint.Child
}

func validateCheckpointPausedNodes(
	checkpoint *executionCheckpoint,
	plan *Plan,
	mode checkpointIdentityMode,
) (map[int]struct{}, error) {
	paused := make(map[int]struct{}, len(checkpoint.Paused))
	for _, node := range checkpoint.Paused {
		if node.Index < 0 || node.Index >= len(plan.nodes) {
			return nil, errors.New("checkpoint has invalid interrupted node")
		}

		if _, duplicate := paused[node.Index]; duplicate {
			return nil, errors.New("checkpoint has duplicate interrupted node")
		}

		if err := validateCheckpointPausedNode(checkpoint, plan, node, mode); err != nil {
			return nil, err
		}

		paused[node.Index] = struct{}{}
	}

	return paused, nil
}

func validateCheckpointPausedNode(
	checkpoint *executionCheckpoint,
	plan *Plan,
	node pausedNodeCheckpoint,
	mode checkpointIdentityMode,
) error {
	if !validPausedNodeAccounting(checkpoint.Nodes[node.Index], node) {
		return errors.New("checkpoint interrupted node state mismatch")
	}

	if err := validatePlanNodeInputValues(node.Inputs, plan.nodes[node.Index]); err != nil {
		return fmt.Errorf("checkpoint interrupted node inputs: %w", err)
	}

	if err := validatePausedCompositeState(node, plan.nodes[node.Index], mode); err != nil {
		return err
	}

	if err := validateDynamicInterrupts(node.Dynamic); err != nil {
		return err
	}

	if !validDynamicLocalCheckpoint(node.Local) {
		return errors.New("checkpoint has invalid composite interrupt state")
	}

	return validatePausedNodeResumeShape(node)
}

func validatePlanInputValues(values map[string]Value, plan *Plan) error {
	if plan.nodeDebug != nil {
		return validateNodeDebugInputs(values, plan.nodeDebug.spec)
	}

	if plan.partialRun != nil {
		_, err := validatePartialWorkflowInputs(values, plan.definition.Inputs, nil)

		return err
	}

	return validatePortValues(values, plan.definition.Inputs)
}

func validatePlanNodeInputValues(values map[string]Value, node planNode) error {
	if !node.isMerge {
		return validatePortValues(values, node.spec.Inputs)
	}

	if values == nil {
		return errors.New("nil values")
	}

	for name, value := range values {
		schema, ok := node.spec.Inputs[name]
		if !ok {
			return fmt.Errorf("unknown port %q", name)
		}

		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("port %q: %w", name, err)
		}
	}

	return nil
}

func validPausedNodeAccounting(summary checkpointNodeRun, node pausedNodeCheckpoint) bool {
	return summary.Status == NodeStatusInterrupted && node.Attempts >= 0 &&
		(node.Rerun || node.Attempts >= 1)
}

func validDynamicLocalCheckpoint(local *dynamicLocalCheckpoint) bool {
	return local == nil || (local.Info.IsValid() &&
		(local.State == nil || local.State.IsValid()))
}

func validatePausedNodeResumeShape(node pausedNodeCheckpoint) error {
	hasState := node.Child != nil || node.Batch != nil || node.Loop != nil ||
		len(node.Dynamic) != 0 || node.Rerun
	if !hasState {
		return errors.New("checkpoint interrupted node has no resume state")
	}

	hasNonRerunState := node.Child != nil || node.Batch != nil || node.Loop != nil ||
		len(node.Dynamic) != 0 || node.Local != nil
	if node.Rerun && hasNonRerunState {
		return errors.New("checkpoint rerun node has conflicting resume state")
	}

	return nil
}

func validatePausedCompositeState(
	node pausedNodeCheckpoint,
	planNode planNode,
	mode checkpointIdentityMode,
) error {
	states := 0
	if node.Child != nil {
		states++

		children := childPlans(planNode.executor)
		if len(children) != 1 {
			return errors.New("checkpoint child state belongs to a non-composite node")
		}

		if err := validateExecutionCheckpoint(node.Child, children[0], mode); err != nil {
			return err
		}
	}

	if node.Batch != nil {
		states++

		if err := validateBatchCheckpoint(node.Batch, planNode, node.Inputs, mode); err != nil {
			return err
		}
	}

	if node.Loop != nil {
		states++

		if err := validateLoopCheckpoint(node.Loop, planNode, node.Inputs, mode); err != nil {
			return err
		}
	}

	if states > 1 {
		return errors.New("checkpoint interrupted node has conflicting composite state")
	}

	return nil
}

func planCheckpointFingerprint(plan *Plan, mode checkpointIdentityMode) string {
	if plan == nil {
		return ""
	}

	switch mode {
	case checkpointIdentityLegacy:
		return plan.legacyFingerprint
	case checkpointIdentityCurrent:
		return plan.fingerprint
	default:
		return ""
	}
}

func validateCheckpointFrontiers(
	checkpoint *executionCheckpoint,
	ready map[int]struct{},
	paused map[int]struct{},
) error {
	for index := range ready {
		if checkpoint.Nodes[index].Status != NodeStatusReady {
			return errors.New("checkpoint frontier contains a non-ready node")
		}
	}

	for index, node := range checkpoint.Nodes {
		_, hasPause := paused[index]
		if (node.Status == NodeStatusInterrupted) != hasPause {
			return errors.New("checkpoint interrupted frontier mismatch")
		}
	}

	return nil
}

func validateCheckpointBeforePassed(
	checkpoint *executionCheckpoint,
	plan *Plan,
) error {
	beforePassed := make(map[int]struct{}, len(checkpoint.BeforePassed))
	for _, index := range checkpoint.BeforePassed {
		if index < 0 || index >= len(plan.nodes) {
			return errors.New("checkpoint has invalid before-node marker")
		}

		if _, configured := plan.interruptBefore[index]; !configured {
			return errors.New("checkpoint before-node marker is not configured")
		}

		if _, duplicate := beforePassed[index]; duplicate {
			return errors.New("checkpoint has duplicate before-node marker")
		}

		beforePassed[index] = struct{}{}
	}

	return nil
}

func validateCheckpointDone(checkpoint *executionCheckpoint) error {
	completed := 0

	for _, node := range checkpoint.Nodes {
		switch node.Status {
		case NodeStatusSucceeded, NodeStatusException, NodeStatusFailed, NodeStatusSkipped:
			completed++
		case NodeStatusPending, NodeStatusReady, NodeStatusRunning, NodeStatusInterrupted:
		}
	}

	if completed != checkpoint.Done {
		return errors.New("checkpoint completed-node accounting mismatch")
	}

	return nil
}

func childPlans(executor CompiledNode) []*Plan {
	composite, ok := executor.(compiledCompositeNode)
	if !ok {
		return nil
	}

	return composite.childPlans()
}
