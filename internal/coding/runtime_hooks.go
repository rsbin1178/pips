package coding

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/hooks"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	maxHookProjectionBytes = 256 << 10
	hookSessionEndDeadline = 3 * time.Second
)

type lifecycleHookInput struct {
	Schema        string      `json:"schema"`
	SessionID     string      `json:"session_id"`
	CWD           string      `json:"cwd"`
	HookEventName hooks.Event `json:"hook_event_name"`
}

type sessionStartHookInput struct {
	lifecycleHookInput
	Source string `json:"source"`
}

type userPromptHookInput struct {
	lifecycleHookInput
	Prompt            string `json:"prompt"`
	PromptTruncated   bool   `json:"prompt_truncated,omitempty"`
	HasNonTextContent bool   `json:"has_non_text_content"`
}

type toolHookInput struct {
	lifecycleHookInput
	ToolName           string          `json:"tool_name"`
	ToolUseID          string          `json:"tool_use_id"`
	Turn               int             `json:"turn"`
	ToolInput          json.RawMessage `json:"tool_input"`
	ToolInputTruncated bool            `json:"tool_input_truncated,omitempty"`
}

type postToolHookInput struct {
	toolHookInput
	ToolResponse hookToolResponse `json:"tool_response"`
}

type permissionRequestHookInput struct {
	toolHookInput
	Justification string `json:"justification,omitempty"`
}

type hookToolResponse struct {
	Content           string `json:"content"`
	IsError           bool   `json:"is_error"`
	Truncated         bool   `json:"truncated"`
	HasNonTextContent bool   `json:"has_non_text_content"`
}

type preCompactHookInput struct {
	lifecycleHookInput
	Trigger         string `json:"trigger"`
	EstimatedTokens int    `json:"estimated_tokens"`
	ThresholdTokens int    `json:"threshold_tokens"`
}

type postCompactHookInput struct {
	lifecycleHookInput
	Trigger      string `json:"trigger"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
	DurationMS   int64  `json:"duration_ms"`
}

type sessionEndHookInput struct {
	lifecycleHookInput
	Reason SessionCloseReason `json:"reason"`
}

type subagentStartHookInput struct {
	lifecycleHookInput
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	Task      string `json:"task"`
}

type subagentStopHookInput struct {
	lifecycleHookInput
	AgentID              string `json:"agent_id"`
	AgentType            string `json:"agent_type"`
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
}

type stopHookInput struct {
	lifecycleHookInput
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
}

func (r *Runtime) hookInput(event hooks.Event) lifecycleHookInput {
	return lifecycleHookInput{
		Schema: hooks.InputSchema, SessionID: r.handle.Metadata().ID,
		CWD: r.workspace.Root(), HookEventName: event,
	}
}

func (r *Runtime) invokeHooks(
	ctx context.Context,
	event hooks.Event,
	target string,
	input any,
) (hooks.Outcome, error) {
	if len(r.hookDefinitions) == 0 {
		return hooks.Outcome{}, nil
	}

	return r.hookRunner.Invoke(ctx, r.hookDefinitions, hooks.Invocation{
		Event: event, Target: target, Input: input,
	})
}

func (r *Runtime) runSessionStart(ctx context.Context, resumed bool) error {
	source := "startup"
	if resumed {
		source = "resume"
	}

	_, err := r.runSessionStartSource(ctx, source, nil)

	return err
}

func (r *Runtime) runSessionStartSource(
	ctx context.Context,
	source string,
	emitter *eventEmitter,
) (hooks.Outcome, error) {
	outcome, err := r.invokeHooks(ctx, hooks.EventSessionStart, source, sessionStartHookInput{
		lifecycleHookInput: r.hookInput(hooks.EventSessionStart), Source: source,
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventSessionStart, err)

		return outcome, err
	}
	r.mu.Lock()
	r.hookContext = append(r.hookContext, outcome.Context...)
	r.mu.Unlock()

	return outcome, nil
}

func (r *Runtime) runUserPromptSubmit(
	ctx context.Context,
	messages []ai.Message,
	emitter *eventEmitter,
) ([]string, error) {
	prompt, truncated, hasNonTextContent := hookPromptProjection(messages)
	outcome, err := r.invokeHooks(ctx, hooks.EventUserPromptSubmit, "", userPromptHookInput{
		lifecycleHookInput: r.hookInput(hooks.EventUserPromptSubmit),
		Prompt:             prompt,
		PromptTruncated:    truncated,
		HasNonTextContent:  hasNonTextContent,
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventUserPromptSubmit, err)

		return nil, err
	}
	if outcome.Blocked || outcome.Stopped {
		return nil, &HookDeniedError{Reason: outcome.Reason}
	}

	return outcome.Context, nil
}

func (r *Runtime) hookBeforeTool(
	emitter *eventEmitter,
) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	if len(r.hookDefinitions) == 0 {
		return nil
	}

	return func(ctx context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		toolInput, inputTruncated := hookToolInputValue(info.Args)
		outcome, err := r.invokeHooks(ctx, hooks.EventPreToolUse, info.Name, toolHookInput{
			lifecycleHookInput: r.hookInput(hooks.EventPreToolUse),
			ToolName:           info.Name,
			ToolUseID:          info.ID,
			Turn:               info.Turn,
			ToolInput:          toolInput,
			ToolInputTruncated: inputTruncated,
		})
		r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
		if err != nil {
			r.recordHookInvocationFailure(ctx, emitter, hooks.EventPreToolUse, err)

			return agent.DenyTool("trusted lifecycle hook did not complete")
		}
		if outcome.Blocked {
			return agent.DenyTool(outcome.Reason)
		}
		r.addHookToolContext(info.ID, outcome.Context)

		return agent.ToolDecision{UpdatedInput: outcome.UpdatedInput}
	}
}

func (r *Runtime) hookAfterTool(
	emitter *eventEmitter,
) func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride {
	if len(r.hookDefinitions) == 0 {
		return nil
	}

	return func(ctx context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
		return r.applyPostToolHook(ctx, info, emitter)
	}
}

func (r *Runtime) hookAfterPendingTool(
	ctx context.Context,
	call agent.ToolCall,
	parts []ai.Part,
	isError bool,
) ([]ai.Part, bool) {
	override := r.applyPostToolHook(ctx, agent.ToolResultInfo{
		ToolCall: call,
		Result: ai.ToolResultPart{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    slices.Clone(parts),
			IsError:    isError,
		},
	}, nil)
	if override == nil {
		return parts, isError
	}
	if override.Content != nil {
		parts = slices.Clone(override.Content)
	}
	if override.IsError != nil {
		isError = *override.IsError
	}

	return parts, isError
}

func (r *Runtime) applyPostToolHook(
	ctx context.Context,
	info agent.ToolResultInfo,
	emitter *eventEmitter,
) *agent.ToolResultOverride {
	toolInput, inputTruncated := hookToolInputValue(info.Args)
	outcome, err := r.invokeHooks(ctx, hooks.EventPostToolUse, info.Name, postToolHookInput{
		toolHookInput: toolHookInput{
			lifecycleHookInput: r.hookInput(hooks.EventPostToolUse),
			ToolName:           info.Name,
			ToolUseID:          info.ID,
			Turn:               info.Turn,
			ToolInput:          toolInput,
			ToolInputTruncated: inputTruncated,
		},
		ToolResponse: hookToolResponseProjection(info.Result),
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventPostToolUse, err)
	}
	contexts := append(r.takeHookToolContext(info.ID), outcome.Context...)
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

func (r *Runtime) runPermissionRequest(
	ctx context.Context,
	review approval.Review,
	emitter *eventEmitter,
) (hooks.Outcome, error) {
	toolInput, inputTruncated := hookToolInputValue(review.Call.Args)
	outcome, err := r.invokeHooks(ctx, hooks.EventPermissionRequest, review.Call.Name, permissionRequestHookInput{
		toolHookInput: toolHookInput{
			lifecycleHookInput: r.hookInput(hooks.EventPermissionRequest),
			ToolName:           review.Call.Name,
			ToolUseID:          review.Call.ID,
			ToolInput:          toolInput,
			ToolInputTruncated: inputTruncated,
		},
		Justification: review.Operation.Justification(),
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventPermissionRequest, err)
	}

	return outcome, err
}

func (r *Runtime) runStopHook(
	ctx context.Context,
	stopHookActive bool,
	emitter *eventEmitter,
) (hooks.Outcome, error) {
	outcome, err := r.invokeHooks(ctx, hooks.EventStop, "", stopHookInput{
		lifecycleHookInput:   r.hookInput(hooks.EventStop),
		StopHookActive:       stopHookActive,
		LastAssistantMessage: r.lastHookAssistantMessage(),
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventStop, err)
	}

	return outcome, err
}

func (r *Runtime) subagentHookLifecycle() subagent.Lifecycle {
	if len(r.hookDefinitions) == 0 {
		return subagent.Lifecycle{}
	}

	return subagent.Lifecycle{
		BeforeStart: func(ctx context.Context, info subagent.LifecycleStart) string {
			agentType := subagentHookAgentType(info.Identity, info.Role)
			outcome, err := r.invokeHooks(ctx, hooks.EventSubagentStart, agentType, subagentStartHookInput{
				lifecycleHookInput: r.hookInput(hooks.EventSubagentStart),
				AgentID:            agentType,
				AgentType:          agentType,
				Task:               hookTextProjection(info.Task),
			})
			r.recordHookDiagnostics(ctx, nil, outcome.Diagnostics)
			if err != nil {
				r.recordHookInvocationFailure(ctx, nil, hooks.EventSubagentStart, err)

				return ""
			}
			if len(outcome.Context) == 0 {
				return ""
			}
			suffix, suffixErr := trustedHookContextSuffix(outcome.Context)
			if suffixErr != nil {
				r.recordHookInvocationFailure(ctx, nil, hooks.EventSubagentStart, suffixErr)

				return ""
			}

			return suffix
		},
		BeforeStop: func(ctx context.Context, info subagent.LifecycleStop) subagent.LifecycleStopDecision {
			agentType := subagentHookAgentType(info.Identity, info.Role)
			outcome, err := r.invokeHooks(ctx, hooks.EventSubagentStop, agentType, subagentStopHookInput{
				lifecycleHookInput:   r.hookInput(hooks.EventSubagentStop),
				AgentID:              agentType,
				AgentType:            agentType,
				StopHookActive:       info.StopHookActive,
				LastAssistantMessage: hookTextProjection(info.LastAssistantMessage),
			})
			r.recordHookDiagnostics(ctx, nil, outcome.Diagnostics)
			if err != nil {
				r.recordHookInvocationFailure(ctx, nil, hooks.EventSubagentStop, err)

				return subagent.LifecycleStopDecision{}
			}
			if outcome.Stopped || !outcome.Blocked {
				return subagent.LifecycleStopDecision{}
			}

			return subagent.LifecycleStopDecision{
				Continue: true,
				Reason:   hookContinuationReason(outcome.Reason),
			}
		},
	}
}

func subagentHookAgentType(identity subagent.AgentIdentity, role subagent.Role) string {
	if identity.ID != "" {
		return identity.ID
	}

	return string(role)
}

func (r *Runtime) runPreCompact(
	ctx context.Context,
	mode CompactionMode,
	preview CompactionPreview,
	emitter *eventEmitter,
) error {
	trigger := hookCompactionTrigger(mode)
	outcome, err := r.invokeHooks(ctx, hooks.EventPreCompact, trigger, preCompactHookInput{
		lifecycleHookInput: r.hookInput(hooks.EventPreCompact),
		Trigger:            trigger, EstimatedTokens: preview.EstimatedTokens, ThresholdTokens: preview.ThresholdTokens,
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventPreCompact, err)

		return err
	}
	if outcome.Stopped {
		return &HookStoppedError{Event: hooks.EventPreCompact, Reason: outcome.Reason}
	}

	return nil
}

func (r *Runtime) runPostCompact(
	ctx context.Context,
	mode CompactionMode,
	before int,
	after int,
	durationMS int64,
	emitter *eventEmitter,
) error {
	trigger := hookCompactionTrigger(mode)
	outcome, err := r.invokeHooks(ctx, hooks.EventPostCompact, trigger, postCompactHookInput{
		lifecycleHookInput: r.hookInput(hooks.EventPostCompact),
		Trigger:            trigger, TokensBefore: before, TokensAfter: after, DurationMS: durationMS,
	})
	r.recordHookDiagnostics(ctx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(ctx, emitter, hooks.EventPostCompact, err)

		return err
	}
	if outcome.Stopped {
		return &HookStoppedError{Event: hooks.EventPostCompact, Reason: outcome.Reason}
	}

	return nil
}

func (r *Runtime) runSessionEnd(
	ctx context.Context,
	reason SessionCloseReason,
	emitter *eventEmitter,
) {
	hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookSessionEndDeadline)
	defer cancel()

	outcome, err := r.invokeHooks(hookCtx, hooks.EventSessionEnd, string(reason), sessionEndHookInput{
		lifecycleHookInput: r.hookInput(hooks.EventSessionEnd), Reason: reason,
	})
	r.recordHookDiagnostics(hookCtx, emitter, outcome.Diagnostics)
	if err != nil {
		r.recordHookInvocationFailure(hookCtx, emitter, hooks.EventSessionEnd, err)
	}
}

func (r *Runtime) addHookToolContext(callID string, values []string) {
	if callID == "" || len(values) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hookToolContext == nil {
		r.hookToolContext = make(map[string][]string)
	}
	r.hookToolContext[callID] = append(r.hookToolContext[callID], values...)
}

func (r *Runtime) takeHookToolContext(callID string) []string {
	if callID == "" {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	values := slices.Clone(r.hookToolContext[callID])
	delete(r.hookToolContext, callID)

	return values
}

func (r *Runtime) clearHookToolContext() {
	r.mu.Lock()
	clear(r.hookToolContext)
	r.mu.Unlock()
}

func (r *Runtime) hookContextSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.hookContext)
}

func (r *Runtime) hookContextAfter(index int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.hookContext) {
		return nil
	}

	return slices.Clone(r.hookContext[index:])
}

func hookToolContextOverride(
	result ai.ToolResultPart,
	contexts []string,
) *agent.ToolResultOverride {
	content := slices.Clone(result.Content)
	for _, value := range contexts {
		content = append(content, ai.TextPart{
			Text: "[Trusted lifecycle hook context]\n" + value,
		})
	}

	return &agent.ToolResultOverride{Content: content}
}

func hookToolFeedbackOverride(reason string, isError bool) *agent.ToolResultOverride {
	if reason == "" {
		reason = "hook blocked the lifecycle event"
	}

	return &agent.ToolResultOverride{
		Content: []ai.Part{ai.TextPart{Text: "[Trusted lifecycle hook feedback]\n" + reason}},
		IsError: &isError,
	}
}

func hookContinuationReason(reason string) string {
	if reason == "" {
		return "Continue the task and complete one more focused pass."
	}

	return reason
}

func hookCompactionTrigger(mode CompactionMode) string {
	if mode == CompactionAutomatic {
		return "auto"
	}

	return "manual"
}

func (r *Runtime) recordHookDiagnostics(
	ctx context.Context,
	emitter *eventEmitter,
	diagnostics []hooks.Diagnostic,
) {
	for _, hookDiagnostic := range diagnostics {
		message := hookDiagnostic.Message
		if hookDiagnostic.Reference != "" {
			message = "hook " + hookDiagnostic.Reference + ": " + message
		}
		diagnostic := IntegrationDiagnostic{
			Component: componentHook,
			Code:      hookDiagnostic.Code,
			Message:   message,
			Disabled:  hookDiagnostic.Code == "pending_trust",
		}
		if emitter != nil {
			_ = emitter.emit("", "", EventIntegrationDiagnostic, diagnostic)
			continue
		}
		r.recordDiagnostic(ctx, diagnostic)
	}
}

func (r *Runtime) recordHookInvocationFailure(
	ctx context.Context,
	emitter *eventEmitter,
	event hooks.Event,
	err error,
) {
	if err == nil {
		return
	}
	code := "invocation_failed"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timed_out"
	} else if errors.Is(err, context.Canceled) {
		code = "canceled"
	}
	r.recordHookDiagnostics(ctx, emitter, []hooks.Diagnostic{{
		Code: code, Message: "hook " + string(event) + " did not complete",
	}})
}

func hookPromptProjection(messages []ai.Message) (string, bool, bool) {
	var value strings.Builder
	truncated := false
	hasNonTextContent := false
	for _, message := range messages {
		user, ok := message.(ai.UserMessage)
		if !ok {
			continue
		}

		for _, part := range user.Parts {
			text, ok := part.(ai.TextPart)
			if !ok {
				hasNonTextContent = true
				continue
			}
			appendHookProjectionText(&value, text.Text, &truncated)
		}
	}

	return value.String(), truncated, hasNonTextContent
}

func hookToolInputValue(value ai.JSON) (json.RawMessage, bool) {
	if len(value) == 0 {
		return json.RawMessage("null"), false
	}
	if len(value) > maxHookProjectionBytes || !json.Valid(value) {
		return json.RawMessage("null"), true
	}

	return slices.Clone(value), false
}

func hookToolResponseProjection(result ai.ToolResultPart) hookToolResponse {
	var value strings.Builder
	projection := hookToolResponse{IsError: result.IsError}
	for _, part := range result.Content {
		text, ok := part.(ai.TextPart)
		if !ok {
			projection.HasNonTextContent = true
			continue
		}
		appendHookProjectionText(&value, text.Text, &projection.Truncated)
	}
	projection.Content = value.String()

	return projection
}

func (r *Runtime) lastHookAssistantMessage() string {
	if r == nil || r.session == nil {
		return ""
	}

	contextValue, err := r.session.Context()
	if err != nil {
		return ""
	}
	for index := len(contextValue.Messages) - 1; index >= 0; index-- {
		message, ok := contextValue.Messages[index].(ai.AssistantMessage)
		if !ok {
			continue
		}
		var value strings.Builder
		truncated := false
		for _, part := range message.Parts {
			text, ok := part.(ai.TextPart)
			if !ok {
				continue
			}
			appendHookProjectionText(&value, text.Text, &truncated)
		}

		return value.String()
	}

	return ""
}

func hookTextProjection(text string) string {
	var value strings.Builder
	truncated := false
	appendHookProjectionText(&value, text, &truncated)

	return value.String()
}

func appendHookProjectionText(value *strings.Builder, text string, truncated *bool) {
	if value == nil || truncated == nil || *truncated {
		return
	}
	remaining := maxHookProjectionBytes - value.Len()
	if remaining <= 0 {
		*truncated = true

		return
	}
	bounded, textTruncated := boundedEventText(text, remaining)
	if bounded == "" && text != "" {
		*truncated = true

		return
	}
	value.WriteString(bounded)
	*truncated = textTruncated
}
