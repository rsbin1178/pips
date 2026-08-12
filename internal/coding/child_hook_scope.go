//nolint:wsl_v5 // Child hook ownership and bounded context updates remain adjacent.
package coding

import (
	"context"
	"slices"
	"sync"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/hooks"
)

// childHookScope applies the same trusted hook definitions as a parent
// interaction, but its input identity and mutable tool-context cache belong
// only to the child Session.
type childHookScope struct {
	ambient       []hooks.Definition
	private       []hooks.Definition
	runner        hooks.Runner
	sessionID     string
	workspace     string
	onDiagnostics func(context.Context, []hooks.Diagnostic)

	mu          sync.Mutex
	toolContext map[string][]string
}

func newChildHookScope(
	ambient []hooks.Definition,
	private []hooks.Definition,
	runner hooks.Runner,
	sessionID string,
	workspace string,
	onDiagnostics func(context.Context, []hooks.Diagnostic),
) childHookScope {
	return childHookScope{
		ambient: slices.Clone(ambient), private: slices.Clone(private), runner: runner,
		sessionID: sessionID, workspace: workspace,
		toolContext: make(map[string][]string), onDiagnostics: onDiagnostics,
	}
}

func (s *childHookScope) input(event hooks.Event) lifecycleHookInput {
	return lifecycleHookInput{
		Schema: hooks.InputSchema, SessionID: s.sessionID, CWD: s.workspace, HookEventName: event,
	}
}

func (s *childHookScope) invokeDefinitions(
	ctx context.Context,
	definitions []hooks.Definition,
	event hooks.Event,
	target string,
	input any,
) (hooks.Outcome, error) {
	if s == nil || len(definitions) == 0 {
		return hooks.Outcome{}, nil
	}

	outcome, err := s.runner.Invoke(ctx, definitions, hooks.Invocation{Event: event, Target: target, Input: input})
	s.report(ctx, outcome.Diagnostics)

	return outcome, err
}

func (s *childHookScope) beforeTool(
	ctx context.Context,
	info agent.ToolCallInfo,
) agent.ToolDecision {
	if s == nil || len(s.ambient)+len(s.private) == 0 {
		return agent.ToolDecision{}
	}

	outcome, err := s.invokeBeforeTool(ctx, s.ambient, info)
	if err != nil {
		return agent.DenyTool("trusted lifecycle hook did not complete")
	}
	if outcome.Blocked {
		return agent.DenyTool(outcome.Reason)
	}
	updated := outcome.UpdatedInput
	if len(updated) > 0 {
		info.Args = slices.Clone(updated)
	}

	privateOutcome, err := s.invokeBeforeTool(ctx, s.private, info)
	if err != nil {
		return agent.DenyTool("trusted lifecycle hook did not complete")
	}
	if privateOutcome.Blocked {
		return agent.DenyTool(privateOutcome.Reason)
	}
	s.addToolContext(info.ID, outcome.Context)
	s.addToolContext(info.ID, privateOutcome.Context)
	if len(privateOutcome.UpdatedInput) > 0 {
		updated = privateOutcome.UpdatedInput
	}

	return agent.ToolDecision{UpdatedInput: updated}
}

func (s *childHookScope) invokeBeforeTool(
	ctx context.Context,
	definitions []hooks.Definition,
	info agent.ToolCallInfo,
) (hooks.Outcome, error) {
	toolInput, truncated := hookToolInputValue(info.Args)

	return s.invokeDefinitions(ctx, definitions, hooks.EventPreToolUse, info.Name, toolHookInput{
		lifecycleHookInput: s.input(hooks.EventPreToolUse),
		ToolName:           info.Name,
		ToolUseID:          info.ID,
		Turn:               info.Turn,
		ToolInput:          toolInput,
		ToolInputTruncated: truncated,
	})
}

func (s *childHookScope) afterTool(
	ctx context.Context,
	info agent.ToolResultInfo,
) *agent.ToolResultOverride {
	if s == nil || len(s.ambient)+len(s.private) == 0 {
		return nil
	}

	outcome, err := s.invokeAfterTool(ctx, s.ambient, info)
	if err != nil {
		return nil
	}
	privateOutcome, privateErr := s.invokeAfterTool(ctx, s.private, info)
	if privateErr == nil {
		outcome.Context = append(outcome.Context, privateOutcome.Context...)
		if !outcome.Blocked && privateOutcome.Blocked {
			outcome.Blocked = true
			outcome.Reason = privateOutcome.Reason
		}
		if privateOutcome.Stopped {
			outcome.Stopped = true
			if outcome.Reason == "" {
				outcome.Reason = privateOutcome.Reason
			}
		}
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

func (s *childHookScope) invokeAfterTool(
	ctx context.Context,
	definitions []hooks.Definition,
	info agent.ToolResultInfo,
) (hooks.Outcome, error) {
	toolInput, truncated := hookToolInputValue(info.Args)

	return s.invokeDefinitions(ctx, definitions, hooks.EventPostToolUse, info.Name, postToolHookInput{
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
}

func (s *childHookScope) permissionRequest(
	ctx context.Context,
	review approval.Review,
) (hooks.Outcome, error) {
	input := permissionRequestInput(s, review)
	ambient, err := s.invokeDefinitions(
		ctx, s.ambient, hooks.EventPermissionRequest, review.Call.Name, input,
	)
	if err != nil || ambient.Blocked {
		return ambient, err
	}
	private, err := s.invokeDefinitions(
		ctx, s.private, hooks.EventPermissionRequest, review.Call.Name, input,
	)
	if err != nil {
		return hooks.Outcome{}, err
	}
	if private.Allowed {
		s.report(ctx, []hooks.Diagnostic{{
			Code: "private_allow_ignored", Message: "agent-private Hook cannot grant tool approval",
		}})
	}
	ambient.Context = append(ambient.Context, private.Context...)
	if private.Blocked {
		ambient.Blocked = true
		ambient.Allowed = false
		ambient.Reason = private.Reason
	}

	return ambient, nil
}

func permissionRequestInput(s *childHookScope, review approval.Review) permissionRequestHookInput {
	toolInput, truncated := hookToolInputValue(review.Call.Args)

	return permissionRequestHookInput{
		toolHookInput: toolHookInput{
			lifecycleHookInput: s.input(hooks.EventPermissionRequest),
			ToolName:           review.Call.Name,
			ToolUseID:          review.Call.ID,
			ToolInput:          toolInput,
			ToolInputTruncated: truncated,
		},
		Justification: review.Operation.Justification(),
	}
}

func (s *childHookScope) report(ctx context.Context, diagnostics []hooks.Diagnostic) {
	if s != nil && s.onDiagnostics != nil && len(diagnostics) > 0 {
		s.onDiagnostics(ctx, diagnostics)
	}
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
