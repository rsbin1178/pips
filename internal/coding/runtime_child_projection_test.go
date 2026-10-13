package coding

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimePublishesAChildTaskOnce pins the top of a child's page: the task the
// parent delegated reaches the child's own projection exactly once, as its first
// message. Every child agent event carries that task, so a child that runs a
// second turn is where a repeat used to appear — once per turn.
func TestRuntimePublishesAChildTaskOnce(t *testing.T) {
	t.Parallel()

	const task = "Inspect the runtime composition root."
	model := newRuntimeModel(
		runtimeToolResponse("delegate-1", subagent.ToolName,
			`{"prompt":"`+task+`","description":"delegate",`+
				`"subagent_type":"explore","run_in_background":false}`),
		runtimeToolResponse("read-1", "read", `{"path":"runtime.go"}`),
		runtimeTextResponse("the child answer"),
		runtimeTextResponse("the parent answer"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate the read")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	childID := summaries[0].ChildSessionID
	// The child answered after a tool call, so it crossed a second turn boundary.
	require.Equal(t, 1, summaries[0].ToolCalls)

	state, err := runtime.InspectSubagentState(t.Context(), childID)
	require.NoError(t, err)
	require.NotEmpty(t, state.Transcript)
	assert.Equal(t, []string{task}, userMessageTexts(state.Transcript),
		"the delegated task is the child's only user message")
	_, firstIsUser := state.Transcript[0].(ai.UserMessage)
	assert.True(t, firstIsUser, "the task sits at the top of the child's page")

	detail, err := runtime.InspectSubagent(t.Context(), childID)
	require.NoError(t, err)
	assert.Equal(t, []string{task}, userMessageTexts(detail.Transcript),
		"the durable transcript carries the same single prompt")
}

// userMessageTexts returns the text of every user message, in order.
func userMessageTexts(messages []ai.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		if _, ok := message.(ai.UserMessage); !ok {
			continue
		}
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			if text, ok := part.(ai.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
	}

	return texts
}
