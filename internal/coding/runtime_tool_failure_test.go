//nolint:wsl_v5 // Guard fixtures keep each repeated call beside its transition assertions.
package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolFailureGuardStopsCanonicalIdenticalDenials(t *testing.T) {
	t.Parallel()

	guard := newToolFailureGuard()
	before := guard.wrapBeforeTool(func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
		return agent.DenyTool("invalid arguments")
	})
	calls := []agent.ToolCall{
		{Name: "shell", Args: ai.JSON(`{"command":"true","permissions":[]}`)},
		{Name: "shell", Args: ai.JSON(`{ "permissions": [], "command": "true" }`)},
		{Name: "shell", Args: ai.JSON(`{"command":"true","permissions":[]}`)},
		{Name: "shell", Args: ai.JSON(`{"command":"true","permissions":[]}`)},
	}

	for index, call := range calls {
		decision := before(t.Context(), agent.ToolCallInfo{ToolCall: call})
		require.Equal(t, agent.ToolDecisionDeny, decision.Action)
		switch index {
		case toolFailureLimit - 1:
			assert.Contains(t, decision.Reason, toolFailureCorrectionMessage)
			assert.False(t, guard.stopWhen(agent.RunInfo{}))
		case toolFailureLimit:
			assert.Contains(t, decision.Reason, toolFailureMessage)
		default:
			assert.NotContains(t, decision.Reason, toolFailureCorrectionMessage)
			assert.NotContains(t, decision.Reason, toolFailureMessage)
		}
	}
	assert.True(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureGuardResetsAfterProgress(t *testing.T) {
	t.Parallel()

	guard := newToolFailureGuard()
	denied := guard.wrapBeforeTool(func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
		return agent.DenyTool("invalid arguments")
	})
	call := agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"true","permissions":[]}`)}

	for range toolFailureLimit - 1 {
		denied(t.Context(), agent.ToolCallInfo{ToolCall: call})
	}
	after := guard.wrapAfterTool(nil)
	after(t.Context(), agent.ToolResultInfo{
		ToolCall: agent.ToolCall{Name: "read", Args: ai.JSON(`{"path":"ok"}`)},
		Result:   ai.ToolResultPart{IsError: false},
	})
	decision := denied(t.Context(), agent.ToolCallInfo{ToolCall: call})

	assert.NotContains(t, decision.Reason, toolFailureMessage)
	assert.False(t, guard.stopWhen(agent.RunInfo{}))

	paused := guard.wrapBeforeTool(func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
		return agent.ToolDecision{Action: agent.ToolDecisionPause}
	})
	paused(t.Context(), agent.ToolCallInfo{ToolCall: call})
	assert.False(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureGuardStopsRepeatedExecutedErrors(t *testing.T) {
	t.Parallel()

	guard := newToolFailureGuard()
	after := guard.wrapAfterTool(nil)
	calls := []agent.ToolCall{
		{ID: "run-1", Name: "shell", Args: ai.JSON(`{"command":"false"}`)},
		{ID: "run-2", Name: "shell", Args: ai.JSON(`{ "command": "false" }`)},
		{ID: "run-3", Name: "shell", Args: ai.JSON(`{"command":"false"}`)},
		{ID: "run-4", Name: "shell", Args: ai.JSON(`{"command":"false"}`)},
	}

	for index, call := range calls {
		override := after(t.Context(), agent.ToolResultInfo{
			ToolCall: call,
			Result: ai.ToolResultPart{
				IsError: true,
				Content: agent.TextResult("command failed"),
			},
		})
		if index < toolFailureLimit-1 {
			assert.Nil(t, override)

			continue
		}

		require.NotNil(t, override)
		message := transcriptText([]ai.Message{ai.ToolResults(
			ai.ToolResultPart{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    override.Content,
				IsError:    true,
			},
		)})
		if index == toolFailureLimit-1 {
			assert.Contains(t, message, toolFailureCorrectionMessage)
			assert.False(t, guard.stopWhen(agent.RunInfo{}))
		} else {
			assert.Contains(t, message, toolFailureMessage)
		}
	}

	assert.True(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureGuardGroupsEquivalentStructuredShellFailures(t *testing.T) {
	t.Parallel()

	const failureResult = `{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"cwd_not_found","problem":{"field":"cwd","retryable":true,"hint":"choose an existing Workspace directory"}}`
	guard := newToolFailureGuard()
	before := guard.wrapBeforeTool(func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
		return agent.DenyTool(failureResult)
	})
	calls := []agent.ToolCall{
		{Name: "shell", Args: ai.JSON(`{"command":"pwd","cwd":"missing"}`)},
		{Name: "shell", Args: ai.JSON(`{"cwd":"another-missing","command":"pwd"}`)},
		{Name: "shell", Args: ai.JSON(`{"command":"pwd","cwd":"/workspace/missing"}`)},
		{Name: "shell", Args: ai.JSON(`{"command":"pwd","cwd":"still-missing","permissions":{}}`)},
	}

	for index, call := range calls {
		decision := before(t.Context(), agent.ToolCallInfo{ToolCall: call})
		if index == toolFailureLimit-1 {
			assert.Contains(t, decision.Reason, toolFailureCorrectionMessage)
			assert.Contains(t, decision.Reason, "choose an existing Workspace directory")
		}
		if index == toolFailureLimit {
			assert.Contains(t, decision.Reason, toolFailureMessage)
		}
	}
	assert.True(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureGuardSeparatesStructuredFailureCategories(t *testing.T) {
	t.Parallel()

	const cwdFailure = `{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"cwd_not_found","problem":{"field":"cwd","retryable":true,"hint":"choose an existing Workspace directory"}}`
	const permissionFailure = `{"schema":"pips.coding.tool_result/v1alpha1","ok":false,"tool":"shell","code":"invalid_argument","reason":"permissions_shape","problem":{"field":"permissions","retryable":true,"hint":"use a permissions object"}}`
	guard := newToolFailureGuard()
	call := agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"pwd","cwd":"missing"}`)}

	for range toolFailureLimit {
		disposition, _ := guard.observeFailure(call, cwdFailure)
		if disposition == toolFailureCorrect {
			break
		}
	}
	disposition, _ := guard.observeFailure(call, permissionFailure)
	assert.Equal(t, toolFailureObserved, disposition)
	assert.False(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureGuardChangedFailingCallResetsSequence(t *testing.T) {
	t.Parallel()

	guard := newToolFailureGuard()
	before := guard.wrapBeforeTool(func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
		return agent.DenyTool("invalid arguments")
	})
	first := agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"one"}`)}
	second := agent.ToolCall{Name: "shell", Args: ai.JSON(`{"command":"two"}`)}

	before(t.Context(), agent.ToolCallInfo{ToolCall: first})
	before(t.Context(), agent.ToolCallInfo{ToolCall: first})
	before(t.Context(), agent.ToolCallInfo{ToolCall: second})
	decision := before(t.Context(), agent.ToolCallInfo{ToolCall: first})

	assert.NotContains(t, decision.Reason, toolFailureMessage)
	assert.False(t, guard.stopWhen(agent.RunInfo{}))
}

func TestToolFailureFingerprintPreservesLargeJSONNumbers(t *testing.T) {
	t.Parallel()

	left := toolCallFingerprint(agent.ToolCall{
		Name: "tool", Args: ai.JSON(`{"value":9007199254740992}`),
	})
	right := toolCallFingerprint(agent.ToolCall{
		Name: "tool", Args: ai.JSON(`{"value":9007199254740993}`),
	})
	assert.NotEqual(t, left, right)
}

func TestRuntimeStopsAfterFailedToolCorrectionTurn(t *testing.T) {
	t.Parallel()

	invalid := `{"command":"true","permissions":[]}`
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "shell", invalid),
		runtimeToolResponse("call-2", "shell", invalid),
		runtimeToolResponse("call-3", "shell", invalid),
		runtimeToolResponse("call-4", "shell", invalid),
		runtimeTextResponse("must not be requested"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run the command")))
	state := runtime.Snapshot()

	assert.Equal(t, InteractionIncomplete, state.Interaction.Outcome)
	assert.Equal(t, agent.StopWhen, state.Interaction.Stop)
	assert.Len(t, model.Requests(), toolFailureLimit+1)
	transcript := transcriptText(state.Transcript)
	assert.Contains(t, transcript, toolFailureCorrectionMessage)
	assert.Contains(t, transcript, toolFailureMessage)
	assert.NotContains(t, transcript, "tools.shellPermissions")
	assert.NotContains(t, transcript, "cannot unmarshal")
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
}

func transcriptText(messages []ai.Message) string {
	var text strings.Builder
	for _, message := range messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			switch value := part.(type) {
			case ai.TextPart:
				text.WriteString(value.Text)
			case ai.ToolResultPart:
				for _, content := range value.Content {
					if value, ok := content.(ai.TextPart); ok {
						text.WriteString(value.Text)
					}
				}
			}
		}
	}

	return text.String()
}
