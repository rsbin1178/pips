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

	if child.containsNodeType(NodeTypeBatch) {
		return nil, compileNodeError(definition.ID, "batch body must not contain Batch")
	}

	spec, err := batchNodeSpec(config, child)
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

	if n.config.Mode == BatchSequential {
		return n.runSequential(ctx, input, items)
	}

	return n.runParallel(ctx, input, items)
}

func (n *compiledBatch) runSequential(
	ctx context.Context,
	input NodeInput,
	items []Value,
) ([]Value, error) {
	results := make([]Value, 0, len(items))
	for index, item := range items {
		value, err := n.runItem(ctx, input, item, index)
		if err != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			switch n.config.ErrorMode {
			case BatchTerminate:
				return nil, batchItemError(index, err)
			case BatchContinueWithNull:
				results = append(results, MustValueOf(nil))
			case BatchRemoveFailed:
			}

			continue
		}

		results = append(results, value)
	}

	return results, nil
}

func (n *compiledBatch) runParallel(
	ctx context.Context,
	input NodeInput,
	items []Value,
) ([]Value, error) {
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]Value, len(items))
	succeeded := make([]bool, len(items))
	jobs := make(chan int)
	workerCount := min(n.config.MaxConcurrency, len(items))

	var (
		firstError error
		errorOnce  sync.Once
		workers    sync.WaitGroup
	)

	workers.Add(workerCount)

	for range workerCount {
		go func() {
			defer workers.Done()

			for {
				select {
				case <-batchCtx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}

					value, err := n.runItem(batchCtx, input, items[index], index)
					if err != nil {
						if n.config.ErrorMode == BatchTerminate && ctx.Err() == nil {
							errorOnce.Do(func() {
								firstError = batchItemError(index, err)

								cancel()
							})
						}

						continue
					}

					results[index] = value
					succeeded[index] = true
				}
			}
		}()
	}

	scheduleItems(batchCtx, jobs, len(items))
	close(jobs)
	workers.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if firstError != nil {
		return nil, firstError
	}

	return n.collectParallelResults(results, succeeded), nil
}

func (n *compiledBatch) runItem(
	ctx context.Context,
	input NodeInput,
	item Value,
	index int,
) (Value, error) {
	inputs := make(map[string]Value, len(n.child.definition.Inputs))
	for name := range n.child.definition.Inputs {
		switch name {
		case batchItemInput:
			inputs[name] = item
		case batchIndexInput:
			inputs[name] = MustValueOf(index)
		default:
			inputs[name] = input.Values[name]
		}
	}

	outputs, err := input.runtime.runChild(
		ctx,
		n.child,
		inputs,
		ScopeFrame{Kind: ScopeBatchItem, NodeID: input.runtime.nodeID, Index: index},
	)
	if err != nil {
		return Value{}, err
	}

	return outputs[n.config.ResultOutput], nil
}

func (n *compiledBatch) collectParallelResults(results []Value, succeeded []bool) []Value {
	switch n.config.ErrorMode {
	case BatchContinueWithNull:
		for index := range results {
			if !succeeded[index] {
				results[index] = MustValueOf(nil)
			}
		}

		return results
	case BatchRemoveFailed:
		compacted := make([]Value, 0, len(results))
		for index, result := range results {
			if succeeded[index] {
				compacted = append(compacted, result)
			}
		}

		return compacted
	default:
		return results
	}
}

func scheduleItems(ctx context.Context, jobs chan<- int, itemCount int) {
	for index := range itemCount {
		select {
		case <-ctx.Done():
			return
		case jobs <- index:
		}
	}
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

func batchNodeSpec(config BatchConfig, child *Plan) (NodeSpec, error) {
	itemSchema, itemOK := child.definition.Inputs[batchItemInput]

	indexSchema, indexOK := child.definition.Inputs[batchIndexInput]
	if !itemOK || !indexOK {
		return NodeSpec{}, errors.New("body inputs item and index are required")
	}

	integerSchema, err := ParsePortSchema([]byte(`{"type":"integer"}`))
	if err != nil {
		return NodeSpec{}, fmt.Errorf("prepare index schema: %w", err)
	}

	if !schemaCompatible(integerSchema, indexSchema) {
		return NodeSpec{}, errors.New("body input index must accept an integer")
	}

	result, ok := child.definition.Outputs[config.ResultOutput]
	if !ok {
		return NodeSpec{}, fmt.Errorf("unknown body output %q", config.ResultOutput)
	}

	itemsSchema, err := arrayPortSchema(itemSchema)
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

	for name, schema := range child.definition.Inputs {
		if name != batchItemInput && name != batchIndexInput {
			inputs[name] = schema
		}
	}

	return NodeSpec{
		Inputs:  inputs,
		Outputs: map[string]PortSchema{batchResultsOutput: resultsSchema},
		Routes:  []string{RouteSuccess},
	}, nil
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
