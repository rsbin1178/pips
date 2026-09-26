package coding

import (
	"os"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runtimeToolBatchResponse(calls ...ai.ToolCallPart) *ai.Response {
	parts := make([]ai.AssistantPart, len(calls))
	for index, call := range calls {
		parts[index] = call
	}

	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "runtime-test",
		Message:      ai.AssistantMessage{Parts: parts},
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

// TestRuntimeBatchedWriteToolsRunSeriallyInOrder checks the queue model: a
// response may carry several write tools; none is rejected for batching, and
// they execute one at a time in call order, so a later call observes the
// effects of an earlier one.
func TestRuntimeBatchedWriteToolsRunSeriallyInOrder(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolBatchResponse(
			ai.ToolCallPart{ID: "add", Name: "apply_patch", Args: ai.JSON(
				`{"patch":"*** Begin Patch\n*** Add File: notes.txt\n+one\n*** End Patch\n"}`)},
			ai.ToolCallPart{ID: "update", Name: "apply_patch", Args: ai.JSON(
				`{"patch":"*** Begin Patch\n*** Update File: notes.txt\n@@\n-one\n+two\n*** End Patch\n"}`)},
		),
		runtimeToolBatchResponse(
			ai.ToolCallPart{ID: "read", Name: "read", Args: ai.JSON(`{"path":"notes.txt"}`)},
			ai.ToolCallPart{ID: "rewrite", Name: "apply_patch", Args: ai.JSON(
				`{"patch":"*** Begin Patch\n*** Update File: notes.txt\n@@\n-two\n+three\n*** End Patch\n"}`)},
		),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("edit notes")))

	results := toolResultsByCallID(events)
	for _, id := range []string{"add", "update", "read", "rewrite"} {
		require.Contains(t, results, id)
		assert.False(t, results[id].IsError, "%s: %s", id, toolResultTextOf(t, results[id]))
	}

	var order []string

	for _, payload := range payloadsOfType[ToolCompleted](events, EventToolCompleted) {
		for _, part := range payload.Result.Parts {
			order = append(order, part.ToolCallID)
		}
	}

	assert.Equal(t, []string{"add", "update"}, order[:2])
	assert.Contains(t, toolResultTextOf(t, results["read"]), "two")

	content, err := os.ReadFile(runtime.workspace.Root() + "/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "three\n", string(content))
}
