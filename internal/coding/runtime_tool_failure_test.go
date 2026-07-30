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
	}

	for index, call := range calls {
		decision := before(t.Context(), agent.ToolCallInfo{ToolCall: call})
		require.Equal(t, agent.ToolDecisionDeny, decision.Action)
		if index < toolFailureLimit-1 {
			assert.NotContains(t, decision.Reason, toolFailureMessage)
		} else {
			assert.Contains(t, decision.Reason, toolFailureMessage)
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
		assert.Contains(t, transcriptText([]ai.Message{{
			Role: ai.RoleTool,
			Parts: []ai.Part{ai.ToolResultPart{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    override.Content,
				IsError:    true,
			}},
		}}), toolFailureMessage)
	}

	assert.True(t, guard.stopWhen(agent.RunInfo{}))
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

func TestRuntimeStopsAfterThreeIdenticalInvalidToolCalls(t *testing.T) {
	t.Parallel()

	invalid := `{"command":"true","permissions":[]}`
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "shell", invalid),
		runtimeToolResponse("call-2", "shell", invalid),
		runtimeToolResponse("call-3", "shell", invalid),
		runtimeTextResponse("must not be requested"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run the command")))
	state := runtime.Snapshot()

	assert.Equal(t, InteractionIncomplete, state.Interaction.Outcome)
	assert.Equal(t, agent.StopWhen, state.Interaction.Stop)
	assert.Len(t, model.Requests(), toolFailureLimit)
	transcript := transcriptText(state.Transcript)
	assert.Contains(t, transcript, toolFailureMessage)
	assert.NotContains(t, transcript, "tools.shellPermissions")
	assert.NotContains(t, transcript, "cannot unmarshal")
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
}

func transcriptText(messages []ai.Message) string {
	var text strings.Builder
	for _, message := range messages {
		for _, part := range message.Parts {
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
