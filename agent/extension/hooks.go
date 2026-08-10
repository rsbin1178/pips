package extension

import (
	"context"
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

// Hooks are the Agent lifecycle functions contributed by an Extension.
// ComposeHooks defines their deterministic composition semantics.
type Hooks struct {
	Observe          func(context.Context, agent.Event)
	BeforeTool       func(context.Context, agent.ToolCallInfo) agent.ToolDecision
	AfterTool        func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride
	PrepareTurn      func(context.Context, agent.RunInfo) agent.TurnUpdate
	TransformContext func(context.Context, []ai.Message) ([]ai.Message, error)
}

// ComposeHooks combines Hook sets in argument order. Observers fan out; gates
// stop at the first non-Allow decision; mutators form ordered pipelines.
func ComposeHooks(values ...Hooks) Hooks {
	observers := make([]func(context.Context, agent.Event), 0, len(values))
	gates := make([]func(context.Context, agent.ToolCallInfo) agent.ToolDecision, 0, len(values))
	after := make([]func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride, 0, len(values))
	turns := make([]func(context.Context, agent.RunInfo) agent.TurnUpdate, 0, len(values))
	transforms := make([]func(context.Context, []ai.Message) ([]ai.Message, error), 0, len(values))

	for _, hooks := range values {
		if hooks.Observe != nil {
			observers = append(observers, hooks.Observe)
		}

		if hooks.BeforeTool != nil {
			gates = append(gates, hooks.BeforeTool)
		}

		if hooks.AfterTool != nil {
			after = append(after, hooks.AfterTool)
		}

		if hooks.PrepareTurn != nil {
			turns = append(turns, hooks.PrepareTurn)
		}

		if hooks.TransformContext != nil {
			transforms = append(transforms, hooks.TransformContext)
		}
	}

	return Hooks{
		Observe:          composeObservers(observers),
		BeforeTool:       composeGates(gates),
		AfterTool:        composeAfterTools(after),
		PrepareTurn:      composePrepareTurns(turns),
		TransformContext: composeTransforms(transforms),
	}
}

// AgentOptions returns one Agent option for each configured Hook family.
func (h Hooks) AgentOptions() []agent.Option {
	return h.agentOptions(true)
}

func (h Hooks) agentOptions(includeObserver bool) []agent.Option {
	opts := make([]agent.Option, 0, 5)
	if includeObserver && h.Observe != nil {
		opts = append(opts, agent.WithOnEvent(h.Observe))
	}

	if h.BeforeTool != nil {
		opts = append(opts, agent.WithBeforeTool(h.BeforeTool))
	}

	if h.AfterTool != nil {
		opts = append(opts, agent.WithAfterTool(h.AfterTool))
	}

	if h.PrepareTurn != nil {
		opts = append(opts, agent.WithPrepareTurn(h.PrepareTurn))
	}

	if h.TransformContext != nil {
		opts = append(opts, agent.WithTransformContext(h.TransformContext))
	}

	return opts
}

func composeObservers(values []func(context.Context, agent.Event)) func(context.Context, agent.Event) {
	if len(values) == 0 {
		return nil
	}

	return func(ctx context.Context, event agent.Event) {
		for _, observe := range values {
			observe(ctx, event)
		}
	}
}

func composeGates(values []func(context.Context, agent.ToolCallInfo) agent.ToolDecision) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	if len(values) == 0 {
		return nil
	}

	return func(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		current := info
		var updatedInput ai.JSON
		for _, gate := range values {
			decision := gate(ctx, current)
			if decision.Action != agent.ToolDecisionAllow {
				if decision.UpdatedInput == nil && updatedInput != nil {
					decision.UpdatedInput = slices.Clone(updatedInput)
				}

				return decision
			}
			if decision.UpdatedInput != nil {
				updatedInput = slices.Clone(decision.UpdatedInput)
				current.Args = slices.Clone(decision.UpdatedInput)
			}
		}

		return agent.ToolDecision{UpdatedInput: updatedInput}
	}
}

func composeAfterTools(values []func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride) func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride {
	if len(values) == 0 {
		return nil
	}

	return func(ctx context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
		var combined *agent.ToolResultOverride

		current := info

		for _, mutate := range values {
			override := mutate(ctx, current)
			if override == nil {
				continue
			}

			if combined == nil {
				combined = &agent.ToolResultOverride{}
			}

			applyToolOverride(&current.Result, combined, override)
		}

		return combined
	}
}

func applyToolOverride(
	result *ai.ToolResultPart,
	combined *agent.ToolResultOverride,
	override *agent.ToolResultOverride,
) {
	if override.Content != nil {
		result.Content = slices.Clone(override.Content)
		combined.Content = slices.Clone(override.Content)
	}

	if override.IsError != nil {
		result.IsError = *override.IsError
		value := *override.IsError
		combined.IsError = &value
	}

	if override.Terminate != nil {
		value := *override.Terminate
		combined.Terminate = &value
	}
}

func composePrepareTurns(values []func(context.Context, agent.RunInfo) agent.TurnUpdate) func(context.Context, agent.RunInfo) agent.TurnUpdate {
	if len(values) == 0 {
		return nil
	}

	return func(ctx context.Context, info agent.RunInfo) agent.TurnUpdate {
		combined := agent.TurnUpdate{}

		for _, prepare := range values {
			update := prepare(ctx, info)
			if update.Err != nil {
				combined.Err = update.Err

				return combined
			}

			if update.Model != nil {
				combined.Model = update.Model
			}

			if update.ReplaceMessages != nil {
				combined.ReplaceMessages = cloneMessages(update.ReplaceMessages)
			}

			if update.Tools != nil {
				combined.Tools = slices.Clone(update.Tools)
			}

			if update.NextRequest != nil {
				next := *update.NextRequest
				next.Tools = slices.Clone(update.NextRequest.Tools)
				combined.NextRequest = &next
			}
		}

		return combined
	}
}

func composeTransforms(values []func(context.Context, []ai.Message) ([]ai.Message, error)) func(context.Context, []ai.Message) ([]ai.Message, error) {
	if len(values) == 0 {
		return nil
	}

	return func(ctx context.Context, messages []ai.Message) ([]ai.Message, error) {
		current := cloneMessages(messages)
		for _, transform := range values {
			next, err := transform(ctx, cloneMessages(current))
			if err != nil {
				return nil, err
			}

			current = cloneMessages(next)
		}

		return current, nil
	}
}

func cloneMessages(messages []ai.Message) []ai.Message {
	cloned := make([]ai.Message, len(messages))
	for i, message := range messages {
		messageCopy, err := ai.CloneMessage(message)
		if err != nil {
			cloned[i] = message
			continue
		}

		cloned[i] = messageCopy
	}

	return cloned
}
