package agent_test

import (
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lengthResponse builds an assistant answer the output-token limit cut off.
func lengthResponse(text string) *ai.Response {
	return &ai.Response{
		Message:      ai.AssistantText(text),
		FinishReason: ai.FinishLength,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// requestSystem joins a request's leading system block.
func requestSystem(t *testing.T, req ai.Request) string {
	t.Helper()

	system, _, err := req.Messages.SplitSystem()
	require.NoError(t, err)

	return ai.JoinSystemText(system)
}

// The instruction is unexported; the test pins a stable phrase from it so a
// request carrying it is provable without exporting the constant.
const lengthSalvagePhrase = "exceeded the output token limit"

func TestLengthSalvageResumesTruncatedAnswer(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(lengthResponse("the first half of ")),
		respond(textResponse("the second half")),
	)

	a, err := agent.New(model, agent.WithSystem("base system"))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("write a long answer"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns, "the truncated answer is resumed by one more turn")

	requests := model.Requests()
	require.Len(t, requests, 2)

	assert.NotContains(t, requestSystem(t, requests[0]), lengthSalvagePhrase)
	assert.Contains(t, requestSystem(t, requests[1]), lengthSalvagePhrase,
		"the resumed request carries the salvage instruction")
	assert.Contains(t, requestSystem(t, requests[1]), "base system",
		"the configured prompt still leads the system block")
}

func TestLengthSalvageCommitsPartialThenStopsTruncated(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(lengthResponse("first")),
		respond(lengthResponse("second")),
		respond(lengthResponse("third")),
	)

	a, err := agent.New(model)
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("write"))
	require.NoError(t, err)

	// Two continues are spent; the third still-truncated answer is not a
	// natural end of turn.
	assert.Equal(t, agent.StopTruncated, result.Stop)
	assert.Equal(t, 3, result.Turns)

	// Every partial is real output, so all three stay in the session.
	text := sessionText(sess.Messages())
	assert.Contains(t, text, "first")
	assert.Contains(t, text, "second")
	assert.Contains(t, text, "third")

	requests := model.Requests()
	require.Len(t, requests, 3)

	for _, index := range []int{1, 2} {
		assert.Contains(t, requestSystem(t, requests[index]), lengthSalvagePhrase)
	}
}

func TestLengthSalvageInstructionIsOneShot(t *testing.T) {
	t.Parallel()

	model := newScriptedModel(
		respond(lengthResponse("part one")),
		respond(callResponse(call("c1", "add", `{"a":1,"b":2}`))),
		respond(textResponse("done")),
	)

	a, err := agent.New(model, agent.WithTools(addTool()))
	require.NoError(t, err)

	result, err := a.Run(t.Context(), agent.NewSession(), ai.UserText("add"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 3, result.Turns)

	requests := model.Requests()
	require.Len(t, requests, 3)

	assert.Contains(t, requestSystem(t, requests[1]), lengthSalvagePhrase)
	assert.NotContains(t, requestSystem(t, requests[2]), lengthSalvagePhrase,
		"the salvage suffix applies to exactly one request")
}

func TestLengthStopWithToolCallsKeepsTruncatedBatch(t *testing.T) {
	t.Parallel()

	truncatedCall := &ai.Response{
		Message: ai.Assistant(
			ai.ToolCallPart{ID: "call", Name: "add", Args: ai.JSON(`{"a":1,"b":2}`)},
		),
		FinishReason: ai.FinishLength,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
	model := newScriptedModel(respond(truncatedCall), respond(textResponse("done")))

	a, err := agent.New(model, agent.WithTools(addTool()))
	require.NoError(t, err)

	sess := agent.NewSession()

	result, err := a.Run(t.Context(), sess, ai.UserText("add"))
	require.NoError(t, err)

	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, result.Turns)

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.NotContains(t, requestSystem(t, requests[1]), lengthSalvagePhrase,
		"a truncated tool batch is re-issued, not salvaged")

	// The truncated batch synthesized an error result instead of running the
	// tool, and the run kept going.
	messages := sess.Messages()
	require.Len(t, messages, 4)

	tool, ok := messages[2].(ai.ToolMessage)
	require.True(t, ok)
	require.Len(t, tool.Parts, 1)
	assert.True(t, tool.Parts[0].IsError)
	require.Len(t, tool.Parts[0].Content, 1)

	reason, ok := tool.Parts[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, reason.Text, "output token limit")
}
