package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

const (
	defaultBatchItems   = 100
	maxBatchItems       = 10_000
	maxBatchConcurrency = 256
	batchItemsInput     = "items"
	batchItemInput      = "item"
	batchIndexInput     = "index"
	batchResultsOutput  = "results"
)

// BatchMode controls how Batch items are scheduled.
type BatchMode string

// Batch scheduling modes.
const (
	BatchSequential BatchMode = "sequential"
	BatchParallel   BatchMode = "parallel"
)

// BatchErrorMode controls failed item aggregation.
type BatchErrorMode string

// Batch item error modes.
const (
	BatchTerminate        BatchErrorMode = "terminate"
	BatchContinueWithNull BatchErrorMode = "continue_with_null"
	BatchRemoveFailed     BatchErrorMode = "remove_failed"
)

// BatchConfig defines an inline item Workflow and its bounded map behavior.
type BatchConfig struct {
	Body           Definition     `json:"body"`
	ResultOutput   string         `json:"result_output"`
	Mode           BatchMode      `json:"mode"`
	MaxConcurrency int            `json:"max_concurrency,omitempty"`
	ErrorMode      BatchErrorMode `json:"error_mode"`
	MaxItems       int            `json:"max_items,omitempty"`
}

// BatchNode maps an inline Workflow over an input array.
type BatchNode struct{}

// Spec implements [NodeType].
func (BatchNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key:         NodeTypeBatch,
		Version:     BuiltinNodeVersion,
		DisplayName: "Batch",
	}
}

// Compile implements [NodeType].
func (BatchNode) Compile(
	ctx context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	var config BatchConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	if config.MaxItems == 0 {
		config.MaxItems = defaultBatchItems
	}

	if err := validateBatchConfig(config); err != nil {
		return nil, compileNodeError(definition.ID, "batch config: %v", err)
	}

	compositeContext, ok := compileContext.(compositeCompileContext)
	if !ok {
		return nil, compileNodeError(definition.ID, "composite compiler is unavailable")
	}

	child, err := compositeContext.compileDefinition(ctx, config.Body)
	if err != nil {
		return nil, wrapCompileNodeError(definition.ID, "batch body", err)
	}

	if child.containsNodeType(NodeTypeBatch) || child.containsNodeType(NodeTypeLoop) {
		return nil, compileNodeError(definition.ID, "batch body must not contain Batch or Loop")
	}

	spec, err := batchNodeSpec(config, child, definition.Inputs)
	if err != nil {
		return nil, compileNodeError(definition.ID, "batch contract: %v", err)
	}

	return &compiledBatch{spec: spec, config: config, child: child}, nil
}

type compiledBatch struct {
	spec   NodeSpec
	config BatchConfig
	child  *Plan
}

func (n *compiledBatch) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledBatch) Invoke(ctx context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil {
		return NodeOutput{}, errors.New("batch runtime is unavailable")
	}

	items, err := DecodeValue[[]Value](input.Values[batchItemsInput])
	if err != nil {
		return NodeOutput{}, fmt.Errorf("decode batch items: %w", err)
	}

	if len(items) > n.config.MaxItems {
		return NodeOutput{}, fmt.Errorf("batch item limit exceeded: got %d, max %d", len(items), n.config.MaxItems)
	}

	results, err := n.runItems(ctx, input, items)
	if err != nil {
		return NodeOutput{}, err
	}

	value, err := ValueOf(results)
	if err != nil {
		return NodeOutput{}, fmt.Errorf("encode batch results: %w", err)
	}

	return NodeOutput{
		Values: map[string]Value{batchResultsOutput: value},
		Route:  RouteSuccess,
	}, nil
}

func (n *compiledBatch) childPlans() []*Plan {
	return []*Plan{n.child}
}

func (n *compiledBatch) runItems(
	ctx context.Context,
	input NodeInput,
	items []Value,
) ([]Value, error) {
	if len(items) == 0 {
		return []Value{}, nil
	}

	return n.runResumableItems(ctx, input, items)
}

type batchItemOutcome struct {
	index int
	value Value
	err   error
	pause *executionPauseError
}

func (n *compiledBatch) runResumableItems(
	ctx context.Context,
	input NodeInput,
	items []Value,
) ([]Value, error) {
	checkpoint := newBatchCheckpoint(len(items))
	if input.runtime.resume != nil && input.runtime.resume.Batch != nil {
		checkpoint = cloneBatchCheckpoint(input.runtime.resume.Batch)
	}

	if n.config.Mode == BatchSequential {
		return n.runResumableSequential(ctx, input, items, checkpoint)
	}

	return n.runResumableParallel(ctx, input, items, checkpoint)
}

func newBatchCheckpoint(items int) *batchCheckpoint {
	checkpoint := &batchCheckpoint{Items: make([]batchItemCheckpoint, items)}
	for index := range items {
		checkpoint.Items[index] = batchItemCheckpoint{Index: index, Status: batchItemPending}
	}

	return checkpoint
}

func (n *compiledBatch) runResumableSequential(
	ctx context.Context,
	input NodeInput,
	items []Value,
	checkpoint *batchCheckpoint,
) ([]Value, error) {
	for index := range checkpoint.Items {
		item := &checkpoint.Items[index]
		switch item.Status {
		case batchItemSucceeded, batchItemFailed:
			continue
		case batchItemPending:
		case batchItemInterrupted:
			if !batchItemHasResumeTarget(input.runtime, *item) {
				return nil, batchInterruption(checkpoint)
			}
		}

		outcome := n.runResumableItem(ctx, input, items[index], *item)
		if err := n.applyBatchOutcome(ctx, input.runtime, item, outcome); err != nil {
			return nil, err
		}

		if item.Status == batchItemInterrupted {
			return nil, batchInterruption(checkpoint)
		}
	}

	return n.batchResults(checkpoint), nil
}

func (n *compiledBatch) runResumableParallel(
	ctx context.Context,
	input NodeInput,
	items []Value,
	checkpoint *batchCheckpoint,
) ([]Value, error) {
	for {
		candidates, hasInterrupted := batchCandidates(input.runtime, checkpoint)
		if len(candidates) == 0 {
			if hasInterrupted {
				return nil, batchInterruption(checkpoint)
			}

			return n.batchResults(checkpoint), nil
		}

		outcomes := n.runBatchCandidates(ctx, input, items, checkpoint, candidates)
		if err := n.batchCandidateFailure(ctx, candidates, outcomes); err != nil {
			return nil, err
		}

		for _, index := range candidates {
			outcome, completed := outcomes[index]
			if !completed {
				continue
			}

			if err := n.applyBatchOutcome(
				ctx,
				input.runtime,
				&checkpoint.Items[index],
				outcome,
			); err != nil {
				return nil, err
			}
		}

		if batchHasInterrupted(checkpoint) {
			return nil, batchInterruption(checkpoint)
		}
	}
}

func batchCandidates(
	runtime *nodeRuntime,
	checkpoint *batchCheckpoint,
) ([]int, bool) {
	hasInterrupted := batchHasInterrupted(checkpoint)

	candidates := make([]int, 0, len(checkpoint.Items))
	for _, item := range checkpoint.Items {
		if hasInterrupted {
			if item.Status == batchItemInterrupted && batchItemHasResumeTarget(runtime, item) {
				candidates = append(candidates, item.Index)
			}

			continue
		}

		if item.Status == batchItemPending {
			candidates = append(candidates, item.Index)
		}
	}

	return candidates, hasInterrupted
}

func batchHasInterrupted(checkpoint *batchCheckpoint) bool {
	for _, item := range checkpoint.Items {
		if item.Status == batchItemInterrupted {
			return true
		}
	}

	return false
}

func batchItemHasResumeTarget(runtime *nodeRuntime, item batchItemCheckpoint) bool {
	if len(item.Dynamic) == 0 {
		return true
	}

	for _, interruption := range item.Dynamic {
		if _, targeted := runtime.execution.state.resumeTargets[interruption.ID]; targeted {
			return true
		}
	}

	return false
}

func (n *compiledBatch) runBatchCandidates(
	ctx context.Context,
	input NodeInput,
	items []Value,
	checkpoint *batchCheckpoint,
	candidates []int,
) map[int]batchItemOutcome {
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	outcomeChannel := make(chan batchItemOutcome, len(candidates))
	workers := min(n.config.MaxConcurrency, len(candidates))

	var group sync.WaitGroup
	group.Add(workers)

	for range workers {
		go func() {
			defer group.Done()

			for index := range jobs {
				outcome := n.runResumableItem(
					batchCtx,
					input,
					items[index],
					checkpoint.Items[index],
				)
				outcomeChannel <- outcome

				if n.config.ErrorMode == BatchTerminate && outcome.err != nil {
					cancel()
				}
			}
		}()
	}

	go func() {
		for _, index := range candidates {
			select {
			case <-batchCtx.Done():
				close(jobs)
				group.Wait()
				close(outcomeChannel)

				return
			case jobs <- index:
			}
		}

		close(jobs)
		group.Wait()
		close(outcomeChannel)
	}()

	outcomes := make(map[int]batchItemOutcome, len(candidates))
	for outcome := range outcomeChannel {
		outcomes[outcome.index] = outcome
	}

	return outcomes
}

func (n *compiledBatch) batchCandidateFailure(
	ctx context.Context,
	candidates []int,
	outcomes map[int]batchItemOutcome,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if n.config.ErrorMode != BatchTerminate {
		return nil
	}

	var canceled *batchItemOutcome

	for _, index := range candidates {
		if outcome, ok := outcomes[index]; ok && outcome.err != nil {
			if !errors.Is(outcome.err, context.Canceled) {
				return batchItemError(index, outcome.err)
			}

			canceledOutcome := outcome
			canceled = &canceledOutcome
		}
	}

	if canceled != nil {
		return batchItemError(canceled.index, canceled.err)
	}

	return nil
}

func (n *compiledBatch) runResumableItem(
	ctx context.Context,
	input NodeInput,
	item Value,
	checkpoint batchItemCheckpoint,
) batchItemOutcome {
	inputs := make(map[string]Value, len(input.Values)+2)

	for name := range n.child.definition.Inputs {
		switch name {
		case batchItemInput:
			inputs[name] = item
		case batchIndexInput:
			inputs[name] = MustValueOf(checkpoint.Index)
		default:
			if value, present := input.Values[name]; present {
				inputs[name] = value
			}
		}
	}

	outputs, err := input.runtime.runChildWithCheckpoint(
		ctx,
		n.child,
		inputs,
		ScopeFrame{Kind: ScopeBatchItem, NodeID: input.runtime.nodeID, Index: checkpoint.Index},
		checkpoint.Child,
	)
	if err != nil {
		var pause *executionPauseError
		if errors.As(err, &pause) {
			return batchItemOutcome{index: checkpoint.Index, pause: pause}
		}

		return batchItemOutcome{index: checkpoint.Index, err: err}
	}

	return batchItemOutcome{
		index: checkpoint.Index,
		value: outputs[n.config.ResultOutput],
	}
}

func (n *compiledBatch) applyBatchOutcome(
	ctx context.Context,
	runtime *nodeRuntime,
	item *batchItemCheckpoint,
	outcome batchItemOutcome,
) error {
	item.Result = nil
	item.Child = nil
	item.Dynamic = nil
	item.Info = InterruptInfo{}

	if outcome.pause != nil {
		child := outcome.pause.checkpoint
		item.Status = batchItemInterrupted
		item.Child = &child
		item.Dynamic = cloneDynamicInterrupts(outcome.pause.dynamic)
		item.Info = cloneInterruptInfo(outcome.pause.info)

		return nil //nolint:nilerr // Interruption is resumable control flow.
	}

	if outcome.err == nil {
		value := outcome.value
		item.Status = batchItemSucceeded
		item.Result = &value

		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if n.config.ErrorMode == BatchTerminate {
		return batchItemError(item.Index, outcome.err)
	}

	item.Status = batchItemFailed

	runtime.markHandledFailure()

	return nil
}

func batchInterruption(checkpoint *batchCheckpoint) error {
	info := InterruptInfo{}
	dynamic := []dynamicInterruptCheckpoint{}

	for _, item := range checkpoint.Items {
		if item.Status != batchItemInterrupted {
			continue
		}

		info.Contexts = append(info.Contexts, item.Info.Contexts...)
		info.BeforeNodes = append(info.BeforeNodes, item.Info.BeforeNodes...)
		info.AfterNodes = append(info.AfterNodes, item.Info.AfterNodes...)
		info.RerunNodes = append(info.RerunNodes, item.Info.RerunNodes...)
		dynamic = append(dynamic, cloneDynamicInterrupts(item.Dynamic)...)
	}

	return &executionPauseError{
		batch: cloneBatchCheckpoint(checkpoint),
		info:  cloneInterruptInfo(info), dynamic: dynamic,
	}
}

func (n *compiledBatch) batchResults(checkpoint *batchCheckpoint) []Value {
	results := make([]Value, 0, len(checkpoint.Items))
	for _, item := range checkpoint.Items {
		switch item.Status {
		case batchItemSucceeded:
			results = append(results, *item.Result)
		case batchItemFailed:
			if n.config.ErrorMode == BatchContinueWithNull {
				results = append(results, MustValueOf(nil))
			}
		case batchItemPending, batchItemInterrupted:
		}
	}

	return results
}

func validateBatchConfig(config BatchConfig) error {
	if !validIdentifier(config.ResultOutput) {
		return errors.New("result output must be a valid name")
	}

	if config.MaxItems < 1 || config.MaxItems > maxBatchItems {
		return fmt.Errorf("max items must be between 1 and %d", maxBatchItems)
	}

	switch config.Mode {
	case BatchSequential:
		if config.MaxConcurrency != 0 && config.MaxConcurrency != 1 {
			return errors.New("sequential mode max concurrency must be zero or one")
		}
	case BatchParallel:
		if config.MaxConcurrency < 1 || config.MaxConcurrency > maxBatchConcurrency {
			return fmt.Errorf("parallel max concurrency must be between 1 and %d", maxBatchConcurrency)
		}
	default:
		return fmt.Errorf("unknown mode %q", config.Mode)
	}

	switch config.ErrorMode {
	case BatchTerminate, BatchContinueWithNull, BatchRemoveFailed:
		return nil
	default:
		return fmt.Errorf("unknown error mode %q", config.ErrorMode)
	}
}

func batchNodeSpec(
	config BatchConfig,
	child *Plan,
	bindings map[string]Binding,
) (NodeSpec, error) {
	itemInput, itemOK := child.definition.Inputs[batchItemInput]

	indexInput, indexOK := child.definition.Inputs[batchIndexInput]
	if !itemOK || !indexOK || !itemInput.Required || !indexInput.Required {
		return NodeSpec{}, errors.New("body inputs item and index are required")
	}

	integerSchema, err := ParsePortSchema([]byte(`{"type":"integer"}`))
	if err != nil {
		return NodeSpec{}, fmt.Errorf("prepare index schema: %w", err)
	}

	if !schemaCompatible(integerSchema, indexInput.Schema) {
		return NodeSpec{}, errors.New("body input index must accept an integer")
	}

	result, ok := child.definition.Outputs[config.ResultOutput]
	if !ok {
		return NodeSpec{}, fmt.Errorf("unknown body output %q", config.ResultOutput)
	}

	itemsSchema, err := arrayPortSchema(itemInput.Schema)
	if err != nil {
		return NodeSpec{}, fmt.Errorf("prepare items schema: %w", err)
	}

	resultSchema := result.Schema
	if config.ErrorMode == BatchContinueWithNull {
		resultSchema, err = nullablePortSchema(resultSchema)
		if err != nil {
			return NodeSpec{}, fmt.Errorf("prepare nullable result schema: %w", err)
		}
	}

	resultsSchema, err := arrayPortSchema(resultSchema)
	if err != nil {
		return NodeSpec{}, fmt.Errorf("prepare results schema: %w", err)
	}

	inputs := make(map[string]PortSchema, len(child.definition.Inputs)-1)
	inputs[batchItemsInput] = itemsSchema
	projectBatchLiftedInputs(inputs, child.definition.Inputs, bindings)

	return NodeSpec{
		Inputs:  inputs,
		Outputs: map[string]PortSchema{batchResultsOutput: resultsSchema},
		Routes:  []string{RouteSuccess},
	}, nil
}

func projectBatchLiftedInputs(
	projected map[string]PortSchema,
	inputs map[string]WorkflowInput,
	bindings map[string]Binding,
) {
	for name, input := range inputs {
		if name != batchItemInput && name != batchIndexInput {
			if input.Required {
				projected[name] = input.Schema

				continue
			}

			if _, bound := bindings[name]; bound {
				projected[name] = input.Schema
			}
		}
	}
}

func arrayPortSchema(itemSchema PortSchema) (PortSchema, error) {
	data, err := json.Marshal(struct {
		Type  string          `json:"type"`
		Items json.RawMessage `json:"items"`
	}{Type: "array", Items: itemSchema.RawJSON()})
	if err != nil {
		return PortSchema{}, err
	}

	return ParsePortSchema(data)
}

func nullablePortSchema(schema PortSchema) (PortSchema, error) {
	data, err := json.Marshal(struct {
		AnyOf []json.RawMessage `json:"anyOf"`
	}{AnyOf: []json.RawMessage{schema.RawJSON(), json.RawMessage(`{"type":"null"}`)}})
	if err != nil {
		return PortSchema{}, err
	}

	return ParsePortSchema(data)
}

func batchItemError(index int, err error) error {
	return fmt.Errorf("batch item %d: %w", index, err)
}

func (p *Plan) containsNodeType(nodeType NodeTypeKey) bool {
	for _, node := range p.nodes {
		if node.definition.Type == nodeType {
			return true
		}

		composite, ok := node.executor.(compiledCompositeNode)
		if !ok {
			continue
		}

		for _, child := range composite.childPlans() {
			if child.containsNodeType(nodeType) {
				return true
			}
		}
	}

	return false
}

var (
	_ CompiledNode          = (*compiledBatch)(nil)
	_ compiledCompositeNode = (*compiledBatch)(nil)
)
