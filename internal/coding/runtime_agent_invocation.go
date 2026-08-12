//nolint:wsl_v5,gocyclo,funlen // Direct invocation preserves its complete durable lifecycle in one boundary.
package coding

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

// AgentRunRequest is an explicit user-authored foreground invocation. The
// selected Agent ID is never inferred from a model response. Custom IDs are
// resolved only through the interaction-scoped visibility.user dispatcher.
type AgentRunRequest struct {
	AgentID string
	Task    string
}

// OneShotAgentRunRequest supplies one user-authored definition for this exact
// invocation. Definition is parsed with the same strict profile contract as
// files, but is never persisted or made discoverable to the parent model.
type OneShotAgentRunRequest struct {
	AgentID    string
	Definition []byte
	Task       string
}

// RunAgent starts one user-selected foreground Agent and waits for its durable
// terminal result. It creates an ordinary, content-free parent interaction so
// the child has stable ownership and its lifecycle reaches Runs, but it never
// asks the parent language model to choose or launch the Agent.
//
// A custom Agent must be enabled by the Alpha gate and have visibility.user.
// Builtin IDs remain available through the legacy-compatible manager path.
// This blocking API is non-interactive: a child approval or question is
// durably recorded, then returns its fail-closed error rather than being
// auto-resolved or waiting without a resolver. Interactive callers use
// RunAgentEvents and the targetable SubagentControlState APIs instead.
func (r *Runtime) RunAgent(ctx context.Context, request AgentRunRequest) (subagent.Result, error) {
	return r.runAgent(ctx, request, nil, nil, true)
}

// RunAgentEvents is the event-streaming counterpart of RunAgent. Interactive
// callers use it so a directly launched child appears in Runs immediately and
// can be independently approved or answered while the foreground call waits.
func (r *Runtime) RunAgentEvents(
	ctx context.Context,
	request AgentRunRequest,
) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		_, err := r.runAgent(ctx, request, nil, yield, false)
		if err != nil && !errors.Is(err, errConsumerStopped) {
			yield(Event{}, err)
		}
	}
}

// RunOneShotAgent starts one explicitly supplied, non-persistent definition.
// It follows the same compiler, authority intersection, child controls, and
// durable plan audit as a discovered profile.
func (r *Runtime) RunOneShotAgent(
	ctx context.Context,
	request OneShotAgentRunRequest,
) (subagent.Result, error) {
	if request.AgentID == "" || len(request.Definition) == 0 {
		return subagent.Result{}, fmt.Errorf("%w: one-shot Agent ID and definition are required", ErrRuntimeInvalid)
	}
	definition, err := agentprofile.ParseOneShot(
		request.AgentID,
		request.Definition,
		agentprofile.DefaultLimits(),
	)
	if err != nil {
		return subagent.Result{}, fmt.Errorf("%w: one-shot Agent definition: %w", ErrRuntimeInvalid, err)
	}

	return r.runAgent(ctx, AgentRunRequest{AgentID: definition.ID, Task: request.Task}, &definition, nil, true)
}

func (r *Runtime) runAgent(
	ctx context.Context,
	request AgentRunRequest,
	oneShot *agentprofile.Definition,
	yield func(Event, error) bool,
	failOnInput bool,
) (subagent.Result, error) {
	if r == nil {
		return subagent.Result{}, ErrRuntimeClosed
	}
	if request.AgentID == "" || request.Task == "" {
		return subagent.Result{}, fmt.Errorf("%w: Agent ID and task are required", ErrRuntimeInvalid)
	}

	operationCtx, operation, err := r.beginOperation(ctx, operationDirectAgent, runtimeResolution{}, nil)
	if err != nil {
		return subagent.Result{}, err
	}
	defer r.endOperation(operation)

	emitter := newEventEmitter(operationCtx, r, yield, yield != nil)
	defer emitter.detachConsumer()
	interactionID, err := r.journal.start()
	if err != nil {
		return subagent.Result{}, err
	}
	started := InteractionStarted{Mode: r.currentOperatingMode()}
	if err := emitter.emit(interactionID, "", EventInteractionStarted, started); err != nil {
		return subagent.Result{}, errors.Join(
			err,
			r.finishUnopenedInteraction(interactionID, InteractionCanceled, emitter),
		)
	}

	current, err := r.openInteraction(operationCtx, interactionID, false, emitter, started, nil, nil)
	if err != nil {
		outcome := InteractionFailed
		if errors.Is(err, context.Canceled) {
			outcome = InteractionCanceled
		}

		return subagent.Result{}, errors.Join(
			err,
			r.finishUnopenedInteraction(interactionID, outcome, emitter),
		)
	}
	if err := emitter.emit(current.id, "", EventStatusChanged, StatusChanged{Phase: PhaseRunning}); err != nil {
		return subagent.Result{}, errors.Join(
			err,
			r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionCanceled, emitter),
		)
	}

	r.mu.Lock()
	manager := r.subagents
	r.mu.Unlock()
	directInvocationID := "user-direct-" + current.id
	if err := startDirectAgentInvocation(emitter, current, directInvocationID); err != nil {
		return subagent.Result{}, errors.Join(
			err,
			r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionCanceled, emitter),
		)
	}
	finishDirect := func(stop agent.StopReason, failed bool) error {
		return finishDirectAgentInvocation(emitter, current, directInvocationID, stop, failed)
	}
	if manager == nil {
		return subagent.Result{}, errors.Join(
			fmt.Errorf("%w: Agent dispatch is unavailable for this Runtime", ErrRuntimeInvalid),
			finishDirect(agent.StopWhen, true),
			r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionFailed, emitter),
		)
	}

	dispatcher := current.userDispatcher
	if oneShot != nil {
		custom, ok := dispatcher.(*customSubagentDispatcher)
		if !ok {
			return subagent.Result{}, errors.Join(
				fmt.Errorf("%w: one-shot Agent dispatch is disabled", ErrRuntimeInvalid),
				finishDirect(agent.StopWhen, true),
				r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionFailed, emitter),
			)
		}
		var dispatchErr error
		dispatcher, dispatchErr = custom.withOneShot(*oneShot)
		if dispatchErr != nil {
			return subagent.Result{}, errors.Join(
				dispatchErr,
				finishDirect(agent.StopWhen, true),
				r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionFailed, emitter),
			)
		}
	}
	if failOnInput {
		if custom, ok := dispatcher.(*customSubagentDispatcher); ok {
			dispatcher = custom.withNonInteractiveInput()
		}
	}

	execution, err := manager.StartWithDispatcher(operationCtx, subagent.Request{
		AgentID: request.AgentID,
		Task:    request.Task,
		// A direct invocation has no model run or Tool call. The protocol's
		// complete ownership tuple is nevertheless required for lifecycle
		// ordering, so use a clearly namespaced synthetic routing ID derived
		// from this durable interaction rather than pretending it came from a
		// model-provided call ID.
		Ownership: subagent.Ownership{
			ParentSessionID:     r.handle.Metadata().ID,
			ParentInteractionID: current.id,
			ParentRunID:         directInvocationID,
			ParentToolCallID:    directInvocationID,
			RootInteractionID:   current.rootInteractionID,
		},
		Delivery: subagent.DeliveryForeground,
	}, r.subagentObserver(current, emitter), dispatcher)
	if err != nil {
		return subagent.Result{}, errors.Join(
			err,
			finishDirect(agent.StopWhen, true),
			r.finishInteraction(context.WithoutCancel(operationCtx), current, InteractionFailed, emitter),
		)
	}

	result, runErr := execution.Wait(operationCtx)
	outcome := InteractionSucceeded
	current.stop = agent.StopEndTurn
	if runErr != nil {
		outcome = InteractionFailed
		current.stop = agent.StopWhen
		if errors.Is(runErr, context.Canceled) || errors.Is(operationCtx.Err(), context.Canceled) {
			outcome = InteractionCanceled
		}
	}
	toolErr := finishDirect(current.stop, outcome != InteractionSucceeded)
	finishErr := r.finishInteraction(context.WithoutCancel(operationCtx), current, outcome, emitter)

	return result, errors.Join(runErr, toolErr, finishErr)
}

func startDirectAgentInvocation(
	emitter *eventEmitter,
	current *interaction,
	id string,
) error {
	if emitter == nil || current == nil || id == "" {
		return fmt.Errorf("%w: invalid direct Agent lifecycle", ErrRuntimeInvalid)
	}
	if err := emitter.emit(current.id, id, EventRunStarted, RunStarted{Agent: "user-direct"}); err != nil {
		return err
	}
	if err := emitter.emit(current.id, id, EventTurnStarted, TurnStarted{Turn: 1}); err != nil {
		return err
	}
	if err := emitter.emit(current.id, id, EventToolStarted, ToolStarted{
		Turn: 1,
		Call: ToolCall{ID: id, Name: subagent.ToolName},
	}); err != nil {
		return err
	}
	current.activeRunID = id
	current.runIDs = append(current.runIDs, id)

	return nil
}

func finishDirectAgentInvocation(
	emitter *eventEmitter,
	current *interaction,
	id string,
	stop agent.StopReason,
	failed bool,
) error {
	if emitter == nil || current == nil || id == "" {
		return fmt.Errorf("%w: invalid direct Agent lifecycle", ErrRuntimeInvalid)
	}
	result := ai.ToolResultText(id, subagent.ToolName, "Agent completed")
	if failed {
		result = ai.ToolResultError(id, subagent.ToolName, "Agent did not complete")
	}
	if err := emitter.emit(current.id, id, EventToolCompleted, ToolCompleted{
		Turn: 1, Call: ToolCall{ID: id, Name: subagent.ToolName}, Result: result,
	}); err != nil {
		return err
	}
	if err := emitter.emit(current.id, id, EventTurnCompleted, TurnCompleted{Turn: 1}); err != nil {
		return err
	}
	if err := emitter.emit(current.id, id, EventRunCompleted, RunCompleted{Stop: stop, Turns: 1}); err != nil {
		return err
	}
	current.activeRunID = ""

	return nil
}
