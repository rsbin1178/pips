//nolint:wsl_v5 // Child hook ownership and bounded context updates remain adjacent.
package coding

import (
	"context"
	"slices"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/hooks"
)

// childHookScope applies the same trusted hook definitions as a parent
// interaction, but its input identity and mutable tool-context cache belong
// only to the child Session.
type childHookScope struct {
	definitions []hooks.Definition
	runner      hooks.Runner
	sessionID   string
	workspace   string

	mu          sync.Mutex
	toolContext map[string][]string
}

func newChildHookScope(
	definitions []hooks.Definition,
	runner hooks.Runner,
	sessionID string,
	workspace string,
) childHookScope {
	return childHookScope{
		definitions: slices.Clone(definitions), runner: runner,
		sessionID: sessionID, workspace: workspace,
		toolContext: make(map[string][]string),
	}
}

func (s *childHookScope) input(event hooks.Event) lifecycleHookInput {
	return lifecycleHookInput{
		Schema: hooks.InputSchema, SessionID: s.sessionID, CWD: s.workspace, HookEventName: event,
	}
}

func (s *childHookScope) invoke(
	ctx context.Context,
	event hooks.Event,
	target string,
	input any,
) (hooks.Outcome, error) {
	if s == nil || len(s.definitions) == 0 {
		return hooks.Outcome{}, nil
	}

	return s.runner.Invoke(ctx, s.definitions, hooks.Invocation{Event: event, Target: target, Input: input})
}

func (s *childHookScope) beforeTool(
	ctx context.Context,
	info agent.ToolCallInfo,
) agent.ToolDecision {
	if s == nil || len(s.definitions) == 0 {
		return agent.ToolDecision{}
	}

	toolInput, truncated := hookToolInputValue(info.Args)
	outcome, err := s.invoke(ctx, hooks.EventPreToolUse, info.Name, toolHookInput{
		lifecycleHookInput: s.input(hooks.EventPreToolUse),
		ToolName:           info.Name,
		ToolUseID:          info.ID,
		Turn:               info.Turn,
		ToolInput:          toolInput,
		ToolInputTruncated: truncated,
	})
	if err != nil {
		return agent.DenyTool("trusted lifecycle hook did not complete")
	}
	if outcome.Blocked {
		return agent.DenyTool(outcome.Reason)
	}
	s.addToolContext(info.ID, outcome.Context)

	return agent.ToolDecision{UpdatedInput: outcome.UpdatedInput}
}

func (s *childHookScope) afterTool(
	ctx context.Context,
	info agent.ToolResultInfo,
) *agent.ToolResultOverride {
	if s == nil || len(s.definitions) == 0 {
		return nil
	}

	toolInput, truncated := hookToolInputValue(info.Args)
	outcome, err := s.invoke(ctx, hooks.EventPostToolUse, info.Name, postToolHookInput{
		toolHookInput: toolHookInput{
			lifecycleHookInput: s.input(hooks.EventPostToolUse),
			ToolName:           info.Name,
			ToolUseID:          info.ID,
			Turn:               info.Turn,
			ToolInput:          toolInput,
			ToolInputTruncated: truncated,
		},
		ToolResponse: hookToolResponseProjection(info.Result),
	})
	if err != nil {
		return nil
	}
	contexts := append(s.takeToolContext(info.ID), outcome.Context...)
	if outcome.Blocked {
		return hookToolFeedbackOverride(outcome.Reason, true)
	}
	if outcome.Stopped {
		return hookToolFeedbackOverride(outcome.Reason, false)
	}
	if len(contexts) == 0 {
		return nil
	}

	return hookToolContextOverride(info.Result, contexts)
}

func (s *childHookScope) permissionRequest(
	ctx context.Context,
	review approval.Review,
) (hooks.Outcome, error) {
	toolInput, truncated := hookToolInputValue(review.Call.Args)

	return s.invoke(ctx, hooks.EventPermissionRequest, review.Call.Name, permissionRequestHookInput{
		toolHookInput: toolHookInput{
			lifecycleHookInput: s.input(hooks.EventPermissionRequest),
			ToolName:           review.Call.Name,
			ToolUseID:          review.Call.ID,
			ToolInput:          toolInput,
			ToolInputTruncated: truncated,
		},
		Justification: review.Operation.Justification(),
	})
}

func (s *childHookScope) addToolContext(callID string, values []string) {
	if s == nil || callID == "" || len(values) == 0 {
		return
	}

	s.mu.Lock()
	s.toolContext[callID] = append(s.toolContext[callID], values...)
	s.mu.Unlock()
}

func (s *childHookScope) takeToolContext(callID string) []string {
	if s == nil || callID == "" {
		return nil
	}

	s.mu.Lock()
	values := slices.Clone(s.toolContext[callID])
	delete(s.toolContext, callID)
	s.mu.Unlock()

	return values
}
