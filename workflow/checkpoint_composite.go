package workflow

import (
	"errors"
	"fmt"
)

func cloneBatchCheckpoint(checkpoint *batchCheckpoint) *batchCheckpoint {
	if checkpoint == nil {
		return nil
	}

	cloned := &batchCheckpoint{Items: make([]batchItemCheckpoint, len(checkpoint.Items))}
	for index, item := range checkpoint.Items {
		cloned.Items[index] = batchItemCheckpoint{
			Index: item.Index, Status: item.Status,
			Result:  cloneValuePointer(item.Result),
			Dynamic: cloneDynamicInterrupts(item.Dynamic),
			Info:    cloneInterruptInfo(item.Info),
		}
		if item.Child != nil {
			child := cloneExecutionCheckpoint(*item.Child)
			cloned.Items[index].Child = &child
		}
	}

	return cloned
}

func cloneLoopCheckpoint(checkpoint *loopCheckpoint) *loopCheckpoint {
	if checkpoint == nil {
		return nil
	}

	cloned := &loopCheckpoint{
		Iterations:     checkpoint.Iterations,
		Index:          checkpoint.Index,
		Committed:      cloneValues(checkpoint.Committed),
		Aggregated:     cloneValueSlices(checkpoint.Aggregated),
		IterationState: cloneValues(checkpoint.IterationState),
		ShouldBreak:    checkpoint.ShouldBreak,
		Dynamic:        cloneDynamicInterrupts(checkpoint.Dynamic),
		Info:           cloneInterruptInfo(checkpoint.Info),
	}
	if checkpoint.Child != nil {
		child := cloneExecutionCheckpoint(*checkpoint.Child)
		cloned.Child = &child
	}

	return cloned
}

func cloneValueSlices(values map[string][]Value) map[string][]Value {
	if values == nil {
		return nil
	}

	cloned := make(map[string][]Value, len(values))
	for name, items := range values {
		cloned[name] = append([]Value(nil), items...)
	}

	return cloned
}

func validateBatchCheckpoint(
	checkpoint *batchCheckpoint,
	node planNode,
	inputs map[string]Value,
) error {
	batch, ok := node.executor.(*compiledBatch)
	if !ok {
		return errors.New("checkpoint Batch state belongs to a non-Batch node")
	}

	items, err := DecodeValue[[]Value](inputs[batchItemsInput])
	if err != nil || len(checkpoint.Items) != len(items) {
		return errors.New("checkpoint Batch item shape mismatch")
	}

	resultSchema := batch.child.definition.Outputs[batch.config.ResultOutput].Schema

	for index, item := range checkpoint.Items {
		if err := validateBatchCheckpointItem(
			item,
			index,
			resultSchema,
			batch.child,
			batch.config.ErrorMode,
		); err != nil {
			return err
		}
	}

	return nil
}

func validateBatchCheckpointItem(
	item batchItemCheckpoint,
	index int,
	resultSchema PortSchema,
	child *Plan,
	errorMode BatchErrorMode,
) error {
	if item.Index != index {
		return errors.New("checkpoint Batch item index mismatch")
	}

	switch item.Status {
	case batchItemPending:
		return validateInactiveBatchItem(item)
	case batchItemFailed:
		if errorMode == BatchTerminate {
			return errors.New("checkpoint terminating Batch has handled failed item")
		}

		return validateInactiveBatchItem(item)
	case batchItemSucceeded:
		return validateCompletedBatchItem(item, resultSchema)
	case batchItemInterrupted:
		return validateInterruptedBatchItem(item, child)
	default:
		return errors.New("checkpoint Batch item has invalid status")
	}
}

func validateInactiveBatchItem(item batchItemCheckpoint) error {
	if item.Result != nil || item.Child != nil || len(item.Dynamic) != 0 ||
		!interruptInfoEmpty(item.Info) {
		return errors.New("checkpoint Batch inactive item has resume state")
	}

	return nil
}

func validateCompletedBatchItem(item batchItemCheckpoint, resultSchema PortSchema) error {
	if item.Result == nil || item.Child != nil || len(item.Dynamic) != 0 ||
		!interruptInfoEmpty(item.Info) {
		return errors.New("checkpoint Batch completed item state mismatch")
	}

	if err := resultSchema.Validate(*item.Result); err != nil {
		return fmt.Errorf("checkpoint Batch result: %w", err)
	}

	return nil
}

func validateInterruptedBatchItem(item batchItemCheckpoint, child *Plan) error {
	if item.Result != nil || item.Child == nil || interruptInfoEmpty(item.Info) {
		return errors.New("checkpoint Batch interrupted item state mismatch")
	}

	if err := validateExecutionCheckpoint(item.Child, child); err != nil {
		return err
	}

	if err := validateInterruptInfo(item.Info); err != nil {
		return err
	}

	return validateDynamicInterrupts(item.Dynamic)
}

func validateLoopCheckpoint(
	checkpoint *loopCheckpoint,
	node planNode,
	inputs map[string]Value,
) error {
	loop, ok := node.executor.(*compiledLoop)
	if !ok {
		return errors.New("checkpoint Loop state belongs to a non-Loop node")
	}

	if err := validateLoopCheckpointShape(checkpoint, loop, inputs); err != nil {
		return err
	}

	if err := validatePortValues(checkpoint.Committed, loop.variables); err != nil {
		return fmt.Errorf("checkpoint Loop committed variables: %w", err)
	}

	if err := validatePortValues(checkpoint.IterationState, loop.variables); err != nil {
		return fmt.Errorf("checkpoint Loop iteration variables: %w", err)
	}

	if err := validateLoopCheckpointAggregates(checkpoint, loop); err != nil {
		return err
	}

	if err := validateExecutionCheckpoint(checkpoint.Child, loop.child); err != nil {
		return err
	}

	if err := validateInterruptInfo(checkpoint.Info); err != nil {
		return err
	}

	return validateDynamicInterrupts(checkpoint.Dynamic)
}

func validateLoopCheckpointShape(
	checkpoint *loopCheckpoint,
	loop *compiledLoop,
	inputs map[string]Value,
) error {
	iterations, _, err := loop.iterationInputs(inputs)
	if err != nil || checkpoint.Iterations != iterations || checkpoint.Index < 0 ||
		checkpoint.Index >= iterations || checkpoint.Child == nil ||
		interruptInfoEmpty(checkpoint.Info) {
		return errors.New("checkpoint Loop state shape mismatch")
	}

	return nil
}

func validateLoopCheckpointAggregates(
	checkpoint *loopCheckpoint,
	loop *compiledLoop,
) error {
	expected := 0

	for _, output := range loop.config.Outputs {
		if output.Source != LoopOutputBody {
			continue
		}

		expected++

		values, ok := checkpoint.Aggregated[output.Name]
		if !ok || len(values) > checkpoint.Index {
			return errors.New("checkpoint Loop aggregate shape mismatch")
		}

		if err := validateLoopAggregateValues(
			values,
			loop.child.definition.Outputs[output.Port].Schema,
		); err != nil {
			return err
		}
	}

	if len(checkpoint.Aggregated) != expected {
		return errors.New("checkpoint Loop has unknown aggregate output")
	}

	return nil
}

func validateLoopAggregateValues(values []Value, schema PortSchema) error {
	for _, value := range values {
		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("checkpoint Loop aggregate: %w", err)
		}
	}

	return nil
}

func validateDynamicInterrupts(interruptions []dynamicInterruptCheckpoint) error {
	seen := make(map[string]struct{}, len(interruptions))
	for _, interruption := range interruptions {
		if interruption.ID == "" || !interruption.Info.IsValid() {
			return errors.New("checkpoint has invalid dynamic interruption")
		}

		if _, duplicate := seen[interruption.ID]; duplicate {
			return errors.New("checkpoint has duplicate dynamic interruption")
		}

		seen[interruption.ID] = struct{}{}

		if err := validateNodeAddress(interruption.Address); err != nil {
			return err
		}

		if interruption.State != nil && !interruption.State.IsValid() {
			return errors.New("checkpoint has invalid dynamic interrupt state")
		}
	}

	return nil
}
